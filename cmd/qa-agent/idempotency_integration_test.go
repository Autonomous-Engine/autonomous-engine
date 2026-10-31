package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// M1-05: qa.ready replayed into qa-agent's guarded handler (the one main
// subscribes) against a real PostgreSQL (TEST_POSTGRES_DSN, a disposable
// database: it is migrated). The fixture item has no repository, so a qa.ready
// that passes the precondition ends in one recorded block ("Cannot run QA: no
// repository available") — the e2e fixture's path — without a checkout or an
// LLM call.

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

func seedQAItem(t *testing.T, pool *pgxpool.Pool, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO work_items (id, type, state, title, description)
		VALUES ($1, 'feature', $2, 'M1-05 QA item', 'criteria')`, id, state); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM outbox_events WHERE aggregate_id = $1`,
			`DELETE FROM agent_assignments WHERE work_item_id = $1`,
			`DELETE FROM work_item_transitions WHERE work_item_id = $1`,
			`DELETE FROM work_items WHERE id = $1`,
		} {
			_, _ = pool.Exec(c, q, id)
		}
	})
	return id
}

func qaReady(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID) *bus.Message {
	t.Helper()
	b, err := json.Marshal(map[string]any{"work_id": workID.String(), "title": "t", "acceptance_criteria": "c"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, id)
	})
	return &bus.Message{Topic: bus.TopicQAReady, Key: workID.String(), Value: b, Headers: map[string]string{bus.HeaderEventID: id.String()}}
}

func guardedQA(pool *pgxpool.Pool) bus.Handler {
	h := buildHandler(context.Background(), pool, workgraph.New(pool), agentregistry.New(pool), nil, nopBus{}, evidence.CompatWindowLegacy)
	return idempotency.NewLedger(pool).Handler("qa-agent", h)
}

func countQA(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestIntegration_Idempotency_QAReadyReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	item := seedQAItem(t, pool, "qa")
	msg := qaReady(t, pool, item)
	h := guardedQA(pool)
	for i := 0; i < 10; i++ {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if n := countQA(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`, item); n != 1 {
		t.Errorf("assignments = %d, want 1 (the handler ran once)", n)
	}
	if n := countQA(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, item); n != 1 {
		t.Errorf("work.blocked events = %d, want 1", n)
	}
	if n := countQA(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = 'qa-agent' AND event_id = $1`, msg.EventID()); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

func TestIntegration_Precondition_StaleQAReadyIsBlockedBeforeCheckout(t *testing.T) {
	pool := integrationPool(t)
	item := seedQAItem(t, pool, "review")
	msg := qaReady(t, pool, item)
	if err := guardedQA(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if n := countQA(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`, item); n != 0 {
		t.Errorf("assignments = %d, want 0 (refused before any work)", n)
	}
	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT w.state::text, t.reason FROM work_items w
		JOIN work_item_transitions t ON t.work_item_id = w.id AND t.to_state = 'blocked' WHERE w.id = $1`, item).Scan(&state, &reason); err != nil {
		t.Fatalf("read block: %v", err)
	}
	if state != "blocked" || !strings.Contains(reason, "is review, expected qa") {
		t.Errorf("state=%s reason=%q", state, reason)
	}
}
