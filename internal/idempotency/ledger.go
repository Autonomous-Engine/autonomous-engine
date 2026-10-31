// Package idempotency is the consumer side of at-least-once delivery
// (roadmap 1.2, ticket M1-05).
//
// Kafka redelivers any message whose offset was not committed, and the
// transactional outbox (internal/outbox) can publish one event twice when a
// dispatcher crashes between the broker's ack and recording it. Every outbox
// event carries a stable event id (bus.Message.EventID). A consumer records
// (consumer, event_id) in processed_events (migration 049) as the first
// statement of the transaction that applies the event, so "processed" and the
// effects commit together; a redelivery finds the row and is acknowledged
// without being applied again.
//
// Three layers, from the most explicit to the most convenient:
//
//   - Ledger.Guard inserts the row inside a caller's transaction and says
//     whether this is the first time; Ledger.Seen is the read-only check.
//   - Ledger.Handler wraps a bus.Handler: it acknowledges an already-recorded
//     event before the handler runs (the cheap early exit that keeps a
//     duplicate from creating a worktree or calling an LLM), attaches a
//     Delivery to the handler's context, and records the event itself when
//     the handler succeeded without a guarded transaction.
//   - Wrap(ctx, db) turns the transaction beginner a handler already passes
//     to workgraph.WithTx into one whose first committed transaction carries
//     the guard row. Handlers keep their transactions; they only swap the db.
package idempotency

import (
	"context"
	"errors"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrDuplicate is returned by a guarded transaction's Begin when the event
// has already been processed by this consumer. Nothing was written. Handlers
// may log it like any other failure; Ledger.Handler acknowledges the message.
var ErrDuplicate = errors.New("idempotency: event already processed by this consumer")

var (
	// MetricDuplicates counts deliveries acknowledged without being applied
	// because the consumer had already processed the event: found by the
	// up-front Seen check or by Guard inside the state-change transaction.
	MetricDuplicates = metrics.NewCounter("ae_idempotency_duplicates_total",
		"Deliveries acknowledged without being applied because the consumer had already processed the event_id")
	// MetricUnguarded counts Guard calls for an event without an id: the
	// effects are applied with no deduplication (legacy behaviour).
	MetricUnguarded = metrics.NewCounter("ae_idempotency_unguarded_total",
		"Guard calls for events without an event_id; applied without deduplication")
	// MetricRecordedAfter counts events Ledger.Handler recorded in a
	// transaction of their own because the handler succeeded without
	// opening a guarded one (no state change, or pooled writes only).
	MetricRecordedAfter = metrics.NewCounter("ae_idempotency_recorded_after_handler_total",
		"Events recorded as processed after the handler returned, outside its state-change transaction")
)

const (
	insertProcessedSQL = `
		INSERT INTO processed_events (consumer, event_id, topic)
		VALUES ($1, $2, $3)
		ON CONFLICT (consumer, event_id) DO NOTHING`
	selectProcessedSQL = `
		SELECT EXISTS (SELECT 1 FROM processed_events WHERE consumer = $1 AND event_id = $2)`
)

// TxBeginner opens a transaction. *pgxpool.Pool satisfies it, and so does
// workgraph.TxBeginner's method set, so a Wrap result can be handed to
// workgraph.WithTx and every *Tx helper unchanged.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DB is what a Ledger needs from its database: a row query for Seen and
// transactions for recording outside a handler's own transaction.
// *pgxpool.Pool satisfies it.
type DB interface {
	TxBeginner
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ledger reads and writes processed_events.
type Ledger struct {
	db DB
}

// NewLedger returns a Ledger on db (*pgxpool.Pool in production).
func NewLedger(db DB) *Ledger {
	return &Ledger{db: db}
}

// Guard records that consumer processed eventID, inside tx, and reports
// whether this is the first time. It must be the first statement of the
// transaction that applies the event, so the record and the effects commit
// or roll back together; when firstTime is false the caller must apply
// nothing and acknowledge the message.
//
// A second delivery racing the first blocks on the uncommitted row and sees
// firstTime=false once the first commits (or true if it rolls back).
//
// An event without an id (uuid.Nil) cannot be guarded: Guard returns
// firstTime=true without writing and counts it in MetricUnguarded.
func (l *Ledger) Guard(ctx context.Context, tx pgx.Tx, consumer string, eventID uuid.UUID, topic string) (firstTime bool, err error) {
	if tx == nil {
		return false, errors.New("idempotency guard: nil transaction")
	}
	if consumer == "" {
		return false, errors.New("idempotency guard: consumer is required")
	}
	if eventID == uuid.Nil {
		MetricUnguarded.Inc()
		return true, nil
	}
	tag, err := tx.Exec(ctx, insertProcessedSQL, consumer, eventID, topic)
	if err != nil {
		return false, fmt.Errorf("idempotency guard %s/%s: %w", consumer, eventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Seen reports whether consumer has a committed record of eventID. It is a
// read-only early exit; Guard inside the state-change transaction stays
// authoritative. A nil Ledger or a uuid.Nil id has seen nothing.
func (l *Ledger) Seen(ctx context.Context, consumer string, eventID uuid.UUID) (bool, error) {
	if l == nil || l.db == nil || eventID == uuid.Nil {
		return false, nil
	}
	var seen bool
	if err := l.db.QueryRow(ctx, selectProcessedSQL, consumer, eventID).Scan(&seen); err != nil {
		return false, fmt.Errorf("idempotency seen %s/%s: %w", consumer, eventID, err)
	}
	return seen, nil
}

// record writes the ledger row in a transaction of its own. It returns
// whether the row is new.
func (l *Ledger) record(ctx context.Context, consumer string, eventID uuid.UUID, topic string) (bool, error) {
	if l == nil || l.db == nil {
		return false, errors.New("idempotency record: no database")
	}
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("idempotency record: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op once committed
	first, err := l.Guard(ctx, tx, consumer, eventID, topic)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("idempotency record: commit: %w", err)
	}
	return first, nil
}
