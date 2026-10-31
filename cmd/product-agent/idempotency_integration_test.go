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
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-05: intake replayed ten times into product-agent's guarded handler (the
// one main subscribes) against a real PostgreSQL (TEST_POSTGRES_DSN; it is
// migrated). The fixture item has no product, so the handler's guarded block
// transaction is its only effect; the replays must not repeat it, and — the
// late-duplicate-intake gap — a duplicate arriving after the item moved on
// must not touch it.

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

func TestIntegration_Idempotency_IntakeReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	item := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description)
		VALUES ($1, 'feature', 'intake', 'M1-05 intake item', 'd')`, item); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	t.Cleanup(func() {
		// Each statement gets exactly its own parameter: passing both ids to
		// every statement made each DELETE fail (and the rows leak).
		for _, q := range []struct {
			sql string
			arg uuid.UUID
		}{
			{`DELETE FROM processed_events WHERE event_id = $1`, eventID},
			{`DELETE FROM outbox_events WHERE aggregate_id = $1`, item},
			{`DELETE FROM user_interactions WHERE work_item_id = $1`, item},
			{`DELETE FROM agent_assignments WHERE work_item_id = $1`, item},
			{`DELETE FROM work_item_transitions WHERE work_item_id = $1`, item},
			{`DELETE FROM work_items WHERE id = $1`, item},
		} {
			if _, err := pool.Exec(context.Background(), q.sql, q.arg); err != nil {
				t.Errorf("cleanup %q: %v", q.sql, err)
			}
		}
		// The block's work.blocked outbox row must not outlive the test: an
		// unpublished row is claimed by the next package's dispatcher test
		// when packages share one database.
		var left int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, item).Scan(&left); err != nil || left != 0 {
			t.Errorf("cleanup left %d outbox rows for %s (err %v)", left, item, err)
		}
	})
	b, _ := json.Marshal(map[string]any{"work_id": item.String(), "source": "m105"})
	msg := &bus.Message{Topic: bus.TopicIntake, Key: item.String(), Value: b, Headers: map[string]string{bus.HeaderEventID: eventID.String()}}
	h := idempotency.NewLedger(pool).Handler("product-agent",
		buildHandler(ctx, pool, workgraph.New(pool), nil, agentregistry.New(pool), nil, nopBus{}, &intakePinner{repos: products.NewStore(pool), resolver: &fakeResolver{}}))

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
	if n := count(`SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked'`); n != 1 {
		t.Errorf("transitions to blocked = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`); n != 1 {
		t.Errorf("assignments = %d, want 1", n)
	}

	// The item is retried and moves on; a late duplicate of the original
	// intake must leave it alone.
	if _, err := pool.Exec(ctx, `UPDATE work_items SET state = 'research' WHERE id = $1`, item); err != nil {
		t.Fatal(err)
	}
	if err := h(ctx, msg); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = pool.QueryRow(ctx, `SELECT state::text FROM work_items WHERE id = $1`, item).Scan(&state)
	if state != "research" {
		t.Errorf("state after a late duplicate intake = %s, want research", state)
	}
}
