package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-05: pr.ready replayed ten times into review-agent's guarded handler (the
// one main subscribes) against a real PostgreSQL (TEST_POSTGRES_DSN; it is
// migrated). The fixture has no repository clone, so the independent
// verifier escalates (M3-07): one verdict artifact and one human_review
// transition, recorded through the same guarded commit path a finished
// review takes — and the replays must not repeat it.

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

type nopBus struct{ bus.MessageBus }

func (nopBus) Publish(context.Context, string, string, []byte) error { return nil }

func TestIntegration_Idempotency_PRReadyReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	item := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description)
		VALUES ($1, 'feature', 'implementation', 'M1-05 review item', 'd')`, item); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	t.Cleanup(func() {
		// One argument per statement: pgx rejects extra ones.
		for _, q := range []struct {
			sql string
			arg any
		}{
			{`DELETE FROM processed_events WHERE event_id = $1`, eventID},
			{`DELETE FROM outbox_events WHERE aggregate_id = $1`, item},
			{`DELETE FROM agent_assignments WHERE work_item_id = $1`, item},
			{`DELETE FROM work_item_artifacts WHERE work_item_id = $1`, item},
			{`DELETE FROM work_item_transitions WHERE work_item_id = $1`, item},
			{`DELETE FROM work_items WHERE id = $1`, item},
		} {
			if _, err := pool.Exec(context.Background(), q.sql, q.arg); err != nil {
				t.Logf("cleanup %q: %v", q.sql, err)
			}
		}
	})
	b, _ := json.Marshal(map[string]any{"work_id": item.String(), "pr_url": "https://example.invalid/pr/1", "pr_number": 1, "branch": "agent/x"})
	msg := &bus.Message{Topic: bus.TopicPRReady, Key: item.String(), Value: b, Headers: map[string]string{bus.HeaderEventID: eventID.String()}}

	h := idempotency.NewLedger(pool).Handler("review-agent",
		buildHandler(ctx, pool, workgraph.New(pool), agentregistry.New(pool), nil, nopBus{}, false, "http://github-service.invalid", nil, evidence.CompatWindowLegacy))
	for i := 0; i < 10; i++ {
		if err := h(ctx, msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	count := func(sql string) int {
		var n int
		if err := pool.QueryRow(ctx, sql, item).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`); n != 1 {
		t.Errorf("assignments = %d, want 1 (the handler ran once)", n)
	}
	if n := count(`SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'human_review'`); n != 1 {
		t.Errorf("transitions to human_review = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1 AND artifact_type = 'review_comments' AND content->>'kind' = 'verifier'`); n != 1 {
		t.Errorf("verifier artifacts = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`); n != 0 {
		t.Errorf("work.blocked events = %d, want 0", n)
	}
	if n := count(`SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'review'`); n != 1 {
		t.Errorf("transitions to review = %d, want 1", n)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE consumer = 'review-agent' AND event_id = $1`, eventID).Scan(&rows)
	if rows != 1 {
		t.Errorf("ledger rows = %d, want 1", rows)
	}
}
