package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Codex coherence Part 1: status-orchestrator re-plans a parent whose codex
// changed by moving it back to planning and re-emitting technical-brief.ready
// with replan=true and replan_event_id, the replanned event of
// work_item_codex_events it recorded in the same transaction (which locks the
// parent). commitDecomposition, after locking the parent row first, commits a
// plan only when (codex.ReplanBriefAllowedTx)
//
//   - it was built from a re-plan brief whose event is the parent's latest
//     re-plan (an earlier re-plan's brief, or a duplicate, never adds a second
//     plan), or
//   - it was built from an ordinary brief produced (briefProducedAt: its
//     outbox row's created_at, on the database clock) after the parent's last
//     re-plan, so a redelivered brief from before a re-plan does not commit
//     while a research re-run's fresh brief does.
//
// Otherwise it returns errPlanSupersededByReplan and writes nothing.

// errPlanSupersededByReplan: a codex re-plan of the parent superseded the
// brief this plan was built from; nothing was written.
var errPlanSupersededByReplan = errors.New("plan superseded by a codex re-plan of the parent")

// replanGuard is what commitDecomposition checks: the re-plan event the
// brief came from (uuid.Nil for an ordinary brief) and when an ordinary
// brief was produced (zero: unknown).
type replanGuard struct {
	replanEvent uuid.UUID
	briefAt     time.Time
}

type replanGuardKey struct{}

// withReplanGuard attaches the brief's guard to ctx.
func withReplanGuard(ctx context.Context, g replanGuard) context.Context {
	return context.WithValue(ctx, replanGuardKey{}, g)
}

func replanGuardFrom(ctx context.Context) (replanGuard, bool) {
	g, ok := ctx.Value(replanGuardKey{}).(replanGuard)
	return g, ok
}

// briefProducedAt is when the brief of msg was produced: its outbox row's
// created_at (the row the delivery's event id names; the database clock the
// re-plan's event is written with), else the message's RFC 3339 timestamp,
// else zero.
func briefProducedAt(ctx context.Context, q codex.Querier, msg *bus.Message, timestamp string) time.Time {
	if q != nil && msg != nil {
		if id := msg.EventID(); id != uuid.Nil {
			var at time.Time
			if err := q.QueryRow(ctx, `SELECT created_at FROM outbox_events WHERE id = $1`, id).Scan(&at); err == nil {
				return at
			}
		}
	}
	if at, err := time.Parse(time.RFC3339Nano, timestamp); err == nil {
		return at
	}
	return time.Time{}
}

// checkNotSupersededTx locks the parent row (the lock the re-plan's
// transaction takes for its transition) and applies the commit rule. A plan
// with no guard (callers outside handleTechnicalBrief) is not checked.
func checkNotSupersededTx(ctx context.Context, tx pgx.Tx, parentWorkID uuid.UUID) error {
	g, ok := replanGuardFrom(ctx)
	if !ok {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE id = $1 FOR UPDATE`, parentWorkID); err != nil {
		return fmt.Errorf("lock parent %s: %w", parentWorkID, err)
	}
	allowed, reason, err := codex.ReplanBriefAllowedTx(ctx, tx, parentWorkID, g.replanEvent, g.briefAt)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: %s", errPlanSupersededByReplan, reason)
	}
	return nil
}
