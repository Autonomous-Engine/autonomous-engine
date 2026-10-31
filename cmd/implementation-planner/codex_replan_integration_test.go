package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 1: status-orchestrator re-plans a parent by sending
// it back to planning and re-emitting technical-brief.ready carrying the
// replanned event's id. A plan commits only when its brief is the parent's
// latest re-plan, or an ordinary brief produced after the last re-plan
// (review fix 4: the check is by event id, not by when planning started).

func recordReplan(t *testing.T, pool *pgxpool.Pool, f fixture) codex.ItemEvent {
	t.Helper()
	ctx := context.Background()
	var ev codex.ItemEvent
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		ev, err = codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
			WorkItemID: f.parentWorkID, ProductID: f.productID, Branch: "main", Kind: codex.EventReplanned, Automatic: true,
			FromVersion: 1, ToVersion: 2, Modules: []string{"app"}, Reason: "codex_changed: modules app (v1 → v2)",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_codex_events WHERE work_item_id = $1`, f.parentWorkID)
	})
	return ev
}

// tryCommit commits a one-subtask plan under guard and reports the error
// and whether the subtask exists.
func tryCommit(t *testing.T, pool *pgxpool.Pool, f fixture, g replanGuard) (error, bool) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	plan := []plannedSubtask{{repoName: "m103-repo", steps: 1, item: f.subtaskFor(id), msg: f.taskMessageFor(id)}}
	err := commitDecomposition(withReplanGuard(ctx, g), pool, workgraph.New(pool), f.parentWorkID, nil, plan)
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM work_items WHERE id = $1`, id).Scan(&n)
	return err, n == 1
}

// A redelivered (old) ordinary brief produced before a re-plan does not
// commit; a fresh brief produced after it (research re-run) does.
func TestIntegration_CodexReplan_RedeliveredOldBriefAfterAReplanDoesNotCommit(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	oldBrief, _ := codex.DBNow(ctx, pool)
	recordReplan(t, pool, f)
	if err, made := tryCommit(t, pool, f, replanGuard{briefAt: oldBrief}); !errors.Is(err, errPlanSupersededByReplan) || made {
		t.Fatalf("old brief after a re-plan: err=%v committed=%t, want errPlanSupersededByReplan and nothing", err, made)
	}
	fresh, _ := codex.DBNow(ctx, pool)
	if err, made := tryCommit(t, pool, f, replanGuard{briefAt: fresh}); err != nil || !made {
		t.Fatalf("fresh brief: err=%v committed=%t", err, made)
	}
}

// Two re-plans: only the latest re-plan's brief commits.
func TestIntegration_CodexReplan_OnlyTheLatestReplanCommits(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	first := recordReplan(t, pool, f)
	second := recordReplan(t, pool, f)
	if err, made := tryCommit(t, pool, f, replanGuard{replanEvent: first.ID}); !errors.Is(err, errPlanSupersededByReplan) || made {
		t.Fatalf("earlier re-plan's brief: err=%v committed=%t", err, made)
	}
	if err, made := tryCommit(t, pool, f, replanGuard{replanEvent: second.ID}); err != nil || !made {
		t.Fatalf("latest re-plan's brief: err=%v committed=%t", err, made)
	}
}

// briefProducedAt reads when the brief was produced from its outbox row
// (database clock), else from the message's timestamp.
func TestIntegration_CodexReplan_BriefProducedAt(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	id := uuid.New()
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (id, aggregate_type, aggregate_id, topic, key, payload, created_at, published_at) VALUES ($1, 'work_item', $2, 'technical-brief.ready', $4, '{}', $3, now())`,
		id, f.parentWorkID, created, f.parentWorkID.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE id = $1`, id) })
	msg := &bus.Message{Headers: map[string]string{bus.HeaderEventID: id.String()}}
	if got := briefProducedAt(ctx, pool, msg, ""); !got.Equal(created) {
		t.Fatalf("from the outbox row = %s, want %s", got, created)
	}
	other := &bus.Message{Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}}
	if got := briefProducedAt(ctx, pool, other, "2026-10-30T10:00:00Z"); !got.Equal(time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("from the timestamp = %s", got)
	}
	if got := briefProducedAt(ctx, pool, other, ""); !got.IsZero() {
		t.Fatalf("unknown = %s, want zero", got)
	}
}
