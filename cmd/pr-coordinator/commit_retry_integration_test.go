package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// F2: a replay.complete commit that fails transiently is retried in process
// (each failed transaction rolls its guard row back); after
// maxCommitAttempts the item is blocked with the replay wording; a block
// that fails too is not acknowledged. Failures are injected by a trigger on
// outbox_events, scoped to the item and topic and limited by a sequence
// (nextval is not rolled back with the failing transaction).
// Env-gated (TEST_POSTGRES_DSN).
func TestIntegration_ReplayCompleteRetriesInProcessThenBlocks(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE TABLE IF NOT EXISTS f2_fail_outbox (aggregate_id UUID NOT NULL, topic TEXT NOT NULL, failures INT NOT NULL)`,
		`CREATE SEQUENCE IF NOT EXISTS f2_fail_seq`,
		`CREATE OR REPLACE FUNCTION f2_fail_outbox_fn() RETURNS trigger AS $$
		DECLARE n INT;
		BEGIN
			SELECT failures INTO n FROM f2_fail_outbox WHERE aggregate_id = NEW.aggregate_id AND topic = NEW.topic;
			IF FOUND AND nextval('f2_fail_seq') <= n THEN
				RAISE EXCEPTION 'f2 injected failure for %', NEW.topic USING ERRCODE = '40001';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS f2_fail_outbox_trg ON outbox_events`,
		`CREATE TRIGGER f2_fail_outbox_trg BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION f2_fail_outbox_fn()`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DROP TRIGGER IF EXISTS f2_fail_outbox_trg ON outbox_events`)
		_, _ = pool.Exec(c, `DROP FUNCTION IF EXISTS f2_fail_outbox_fn()`)
		_, _ = pool.Exec(c, `DROP TABLE IF EXISTS f2_fail_outbox`)
		_, _ = pool.Exec(c, `DROP SEQUENCE IF EXISTS f2_fail_seq`)
	})

	// inject makes the next n inserts of topic for workID fail (n = 0: none).
	inject := func(workID uuid.UUID, topic string, n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM f2_fail_outbox WHERE aggregate_id = $1 AND topic = $2`, workID, topic); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `ALTER SEQUENCE f2_fail_seq RESTART`); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			if _, err := pool.Exec(ctx, `INSERT INTO f2_fail_outbox (aggregate_id, topic, failures) VALUES ($1, $2, $3)`, workID, topic, n); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed := func() (uuid.UUID, *bus.Message, bus.Handler) {
		workID, taskID := seedGatedParent(t, pool, "implementation")
		if _, err := pool.Exec(ctx, `UPDATE work_items SET replay = true WHERE id = $1`, workID); err != nil {
			t.Fatalf("mark replay: %v", err)
		}
		h := idempotency.NewLedger(pool).Handler(idempotencyConsumer, buildHandler(workgraph.New(pool),
			interactions.NewPublisher(nil), newTracker(), "http://github-service.invalid",
			products.NewStore(pool), evidence.NewStore(pool), evidence.CompatWindowLegacy))
		return workID, completionMessage(t, pool, workID, taskID), h
	}

	t.Run("two transient failures, then recorded", func(t *testing.T) {
		workID, msg, h := seed()
		inject(workID, bus.TopicReplayComplete, 2)
		if err := h(ctx, msg); err != nil {
			t.Fatalf("delivery: %v", err)
		}
		if s, r, b, l := itemState(t, pool, workID), outboxCount(t, pool, workID, bus.TopicReplayComplete), outboxCount(t, pool, workID, bus.TopicWorkBlocked), ledgerRow(t, pool, msg); s != "implementation" || r != 1 || b != 0 || l != 1 {
			t.Fatalf("state=%s replay.complete=%d work.blocked=%d ledger=%d, want implementation/1/0/1", s, r, b, l)
		}
	})

	t.Run("three transient failures block the item", func(t *testing.T) {
		workID, msg, h := seed()
		inject(workID, bus.TopicReplayComplete, maxCommitAttempts)
		if err := h(ctx, msg); err != nil {
			t.Fatalf("delivery: %v", err)
		}
		if err := h(ctx, msg); err != nil {
			t.Fatalf("duplicate delivery: %v", err)
		}
		if s, r, b, l := itemState(t, pool, workID), outboxCount(t, pool, workID, bus.TopicReplayComplete), outboxCount(t, pool, workID, bus.TopicWorkBlocked), ledgerRow(t, pool, msg); s != "blocked" || r != 0 || b != 1 || l != 1 {
			t.Fatalf("state=%s replay.complete=%d work.blocked=%d ledger=%d, want blocked/0/1/1", s, r, b, l)
		}
		if reason := blockedReason(t, pool, workID); !strings.HasPrefix(reason, "replay: could not record replay.complete: ") {
			t.Errorf("block reason %q", reason)
		}
	})

	t.Run("a failed block is not acknowledged", func(t *testing.T) {
		workID, msg, h := seed()
		inject(workID, bus.TopicReplayComplete, 1000)
		if _, err := pool.Exec(ctx, `INSERT INTO f2_fail_outbox (aggregate_id, topic, failures) VALUES ($1, $2, 1000)`, workID, bus.TopicWorkBlocked); err != nil {
			t.Fatal(err)
		}
		if err := h(ctx, msg); err == nil {
			t.Fatal("delivery acknowledged although the block failed")
		}
		if s, l := itemState(t, pool, workID), ledgerRow(t, pool, msg); s != "implementation" || l != 0 {
			t.Fatalf("state=%s ledger=%d, want implementation/0", s, l)
		}
		// The database recovers: the redelivery records replay.complete.
		inject(workID, bus.TopicReplayComplete, 0)
		inject(workID, bus.TopicWorkBlocked, 0)
		if err := h(ctx, msg); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if r, l := outboxCount(t, pool, workID, bus.TopicReplayComplete), ledgerRow(t, pool, msg); r != 1 || l != 1 {
			t.Fatalf("replay.complete=%d ledger=%d, want 1/1", r, l)
		}
	})
}

func itemState(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM work_items WHERE id = $1`, workID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func outboxCount(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID, topic string) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, workID, topic)
}

func blockedReason(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID) string {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, workID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var blocked struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(payload, &blocked); err != nil {
		t.Fatal(err)
	}
	return blocked.Reason
}
