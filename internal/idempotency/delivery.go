package idempotency

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// deliveryState is where one delivery's guard row stands.
type deliveryState int

const (
	// statePending: no transaction has committed the guard row yet.
	statePending deliveryState = iota
	// stateInTx: a live transaction holds the (uncommitted) guard row.
	stateInTx
	// stateRecorded: a transaction committed the guard row; later
	// transactions of the same delivery run unguarded.
	stateRecorded
	// stateDuplicate: Guard found the event already processed; every later
	// guarded Begin of the delivery fails with ErrDuplicate.
	stateDuplicate
)

// Delivery is the guard state of one message being handled by one consumer.
// Ledger.Handler creates it and attaches it to the handler's context; Wrap
// reads it from there.
//
// The guard row goes into the FIRST transaction opened through a Wrap'd
// beginner that commits. A transaction that rolls back takes its guard row
// with it and the next guarded transaction tries again, so the row always
// commits with the first effects that do. Once a Guard finds a duplicate,
// every later guarded transaction of the delivery fails with ErrDuplicate:
// a handler that logs one failure and carries on to its next write still
// applies nothing.
//
// Unguarded window: a transaction is covered only by the guard row it holds
// itself. One begun while a sibling transaction of the same delivery still
// holds the uncommitted row (stateInTx: handlers that run transactions
// concurrently or nest them), every transaction after the row committed
// (stateRecorded), and every write made through Unguarded(ctx) or a pooled
// call run without a guard. A crash after the guarded transaction committed
// and before such a write makes the redelivery a duplicate, so that write is
// lost; a duplicate racing them is not stopped by the ledger. The per-handler
// consequences are listed in the spec's per-consumer table.
type Delivery struct {
	ledger   *Ledger
	consumer string
	eventID  uuid.UUID
	topic    string

	mu    sync.Mutex
	state deliveryState
}

// NewDelivery returns the guard state for msg handled by consumer. Most code
// uses Ledger.Handler instead; NewDelivery and WithDelivery exist for
// handlers driven directly (tests, replay tools).
func (l *Ledger) NewDelivery(consumer string, msg *bus.Message) *Delivery {
	d := &Delivery{ledger: l, consumer: consumer}
	if msg != nil {
		d.eventID = msg.EventID()
		d.topic = msg.Topic
	}
	return d
}

// EventID is the id being guarded (uuid.Nil: unguarded).
func (d *Delivery) EventID() uuid.UUID { return d.eventID }

// Duplicate reports whether a guarded transaction found the event already
// processed.
func (d *Delivery) Duplicate() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state == stateDuplicate
}

// Recorded reports whether a guarded transaction committed the guard row.
func (d *Delivery) Recorded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state == stateRecorded
}

func (d *Delivery) pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state == statePending
}

type deliveryKey struct{}

// WithDelivery attaches d to ctx.
func WithDelivery(ctx context.Context, d *Delivery) context.Context {
	return context.WithValue(ctx, deliveryKey{}, d)
}

// FromContext returns the Delivery attached to ctx, or nil.
func FromContext(ctx context.Context) *Delivery {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(deliveryKey{}).(*Delivery)
	return d
}

// Unguarded returns ctx without its delivery: transactions opened through
// Wrap on it carry no guard row. It is for a write that is idempotent on its
// own (a state-machine edge that cannot be taken twice) and must not consume
// the guard ahead of a later write in the same handler that has no other
// trigger: with the guard in the first transaction, a crash between the two
// would make the redelivery a duplicate and lose the second write.
func Unguarded(ctx context.Context) context.Context {
	return context.WithValue(ctx, deliveryKey{}, (*Delivery)(nil))
}

// Wrap returns a beginner whose transactions carry the guard of the delivery
// attached to ctx: Begin runs Ledger.Guard as the first statement, fails with
// ErrDuplicate (having written nothing) when the event was already processed,
// and the guard row commits with the transaction.
//
// The delivery is captured now, so the returned beginner may be used with a
// different context later (services that commit outcomes on the process
// context so a cancelled message context cannot drop them). Without a
// delivery in ctx, or for a message without an event id, db is returned
// unchanged.
func Wrap(ctx context.Context, db TxBeginner) TxBeginner {
	d := FromContext(ctx)
	if d == nil || d.eventID == uuid.Nil || db == nil {
		return db
	}
	if g, ok := db.(*guardedBeginner); ok && g.d == d {
		return db
	}
	return &guardedBeginner{db: db, d: d}
}

type guardedBeginner struct {
	db TxBeginner
	d  *Delivery
}

func (g *guardedBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	d := g.d
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.state {
	case stateDuplicate:
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, ErrDuplicate
	case statePending:
		first, gerr := d.ledger.Guard(ctx, tx, d.consumer, d.eventID, d.topic)
		if gerr != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, gerr
		}
		if !first {
			d.state = stateDuplicate
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, ErrDuplicate
		}
		d.state = stateInTx
		return &guardedTx{Tx: tx, d: d}, nil
	default:
		// The guard row is committed already (stateRecorded) or held by
		// another live transaction of this delivery (stateInTx): this
		// transaction carries no guard.
		return tx, nil
	}
}

// guardedTx is the transaction holding a delivery's guard row. It tracks
// whether the row committed.
type guardedTx struct {
	pgx.Tx
	d *Delivery
}

func (t *guardedTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.d.mu.Lock()
	if t.d.state == stateInTx {
		if err == nil {
			t.d.state = stateRecorded
		} else {
			t.d.state = statePending
		}
	}
	t.d.mu.Unlock()
	return err
}

func (t *guardedTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.d.mu.Lock()
	if t.d.state == stateInTx {
		t.d.state = statePending
	}
	t.d.mu.Unlock()
	return err
}

// recordTimeout bounds the stand-alone record Ledger.Handler writes after a
// handler that opened no guarded transaction.
const recordTimeout = 10 * time.Second

// Handler wraps h as consumer's idempotent handler:
//
//  1. A message without an event id runs h unguarded (the bus has counted
//     it in ae_bus_events_without_id_total).
//  2. An event this consumer already recorded is acknowledged without
//     running h (ae_idempotency_duplicates_total). This is the cheap early
//     exit that keeps a duplicate from repeating work before its first
//     transaction: a worktree, a git checkout, an LLM call.
//  3. Otherwise h runs with a Delivery on its context. Transactions opened
//     through Wrap(ctx, db) carry the guard; when one finds the event
//     already processed (a delivery racing the first), the message is
//     acknowledged whatever h returned.
//  4. When h returns an error the error is returned (the bus does not
//     commit the offset and the message is redelivered); a guard row that
//     committed before the error is what makes that redelivery a no-op.
//  5. When h succeeds without committing a guarded transaction (it changed
//     nothing, or only through pooled writes), the event is recorded in a
//     transaction of its own (ae_idempotency_recorded_after_handler_total),
//     so a later duplicate is acknowledged at step 2. Those pooled writes
//     are therefore applied at least once, not exactly once; the spec lists
//     them.
//
// A nil Ledger runs h unguarded.
func (l *Ledger) Handler(consumer string, h bus.Handler) bus.Handler {
	return func(ctx context.Context, msg *bus.Message) error {
		if l == nil {
			return h(ctx, msg)
		}
		eventID := msg.EventID()
		if eventID == uuid.Nil {
			return h(ctx, msg)
		}

		seen, err := l.Seen(ctx, consumer, eventID)
		if err != nil {
			// Guard inside the handler's transaction stays authoritative.
			log.Printf("idempotency: %s: seen check for event_id=%s topic=%s: %v — continuing", consumer, eventID, msg.Topic, err)
		} else if seen {
			noteDuplicate(consumer, eventID, msg, "already recorded")
			return nil
		}

		d := l.NewDelivery(consumer, msg)
		herr := h(WithDelivery(ctx, d), msg)
		if d.Duplicate() {
			noteDuplicate(consumer, eventID, msg, "found by guard inside the handler's transaction")
			return nil
		}
		if herr != nil {
			return herr
		}
		if d.pending() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
			defer cancel()
			if _, rerr := l.record(rctx, consumer, eventID, msg.Topic); rerr != nil {
				// The handler's effects are done and the offset will be
				// committed; a redelivery in the meantime would apply them
				// again, which is the pre-ledger behaviour.
				log.Printf("idempotency: %s: record event_id=%s topic=%s after handler: %v", consumer, eventID, msg.Topic, rerr)
			} else {
				MetricRecordedAfter.Inc()
			}
		}
		return nil
	}
}

// noteDuplicate counts and logs an acknowledged duplicate. The standard
// logger has no levels; the line is prefixed "debug:" so it can be filtered.
func noteDuplicate(consumer string, eventID uuid.UUID, msg *bus.Message, how string) {
	MetricDuplicates.Inc()
	log.Printf("debug: idempotency: %s: duplicate event_id=%s topic=%s key=%s (%s) — acknowledged without reprocessing",
		consumer, eventID, msg.Topic, msg.Key, how)
}

// IsDuplicate reports whether err is (or wraps) ErrDuplicate.
func IsDuplicate(err error) bool { return errors.Is(err, ErrDuplicate) }

// String is for logs.
func (d *Delivery) String() string {
	return fmt.Sprintf("%s/%s(%s)", d.consumer, d.eventID, d.topic)
}
