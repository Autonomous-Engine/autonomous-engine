package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-05: implementation.tasks replayed into implementation-agent's guarded
// handler (the one main subscribes) against a real PostgreSQL. The fixture
// repository has no clone, so a task that passes the precondition ends at
// the repository check with one recorded outcome (a block) — the same
// commit path every outcome takes — without reaching a worktree or the LLM.

type nopBus struct{ bus.MessageBus }

func (nopBus) Publish(context.Context, string, string, []byte) error { return nil }

func guardedAgent(pool *pgxpool.Pool) bus.Handler {
	h := buildHandler(context.Background(), pool, workgraph.New(pool), products.NewStore(pool),
		agentregistry.New(pool), nil, nopBus{}, nil)
	return idempotency.NewLedger(pool).Handler("implementation-agent", h)
}

func seedSubtask(t *testing.T, pool *pgxpool.Pool, f dbFixture, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO work_items (id, type, state, title, product_id, parent_id)
		VALUES ($1, 'subtask', $2, 'M1-05 subtask', $3, $4)`, id, state, f.productID, f.workID); err != nil {
		t.Fatalf("seed subtask: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM outbox_events WHERE aggregate_id = $1`, f.workID)
		_, _ = pool.Exec(c, `DELETE FROM agent_assignments WHERE work_item_id = $1`, f.workID)
		_, _ = pool.Exec(c, `DELETE FROM work_items WHERE id = $1`, id)
	})
	return id
}

func taskMessage(t *testing.T, pool *pgxpool.Pool, f dbFixture, taskID uuid.UUID) *bus.Message {
	t.Helper()
	b, err := json.Marshal(map[string]any{"task_id": taskID.String(), "work_id": f.workID.String(), "title": "t", "description": "d"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, id)
	})
	return &bus.Message{Topic: bus.TopicImplementationTasks, Key: taskID.String(), Value: b,
		Headers: map[string]string{bus.HeaderEventID: id.String()}}
}

func replayAgent(t *testing.T, h bus.Handler, msg *bus.Message, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
}

func TestIntegration_Idempotency_ImplementationTasksReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	f := newDBFixture(t, pool)
	taskID := seedSubtask(t, pool, f, "implementation")
	msg := taskMessage(t, pool, f, taskID)
	assignmentsBefore := countRows(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`, f.workID)

	replayAgent(t, guardedAgent(pool), msg, 10)

	if n := countRows(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`, f.workID) - assignmentsBefore; n != 1 {
		t.Errorf("assignments created = %d, want 1 (the handler ran once)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, f.workID); n != 1 {
		t.Errorf("work.blocked events = %d, want 1 (one recorded outcome)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = 'implementation-agent' AND event_id = $1`, msg.EventID()); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// The known live case: a subtask cancelled while its implementation.tasks
// was queued. Nothing is worked on, nothing is assigned, the cancelled item
// is left alone, and the stale event is recorded so it is not replayed.
func TestIntegration_Precondition_CancelledSubtaskIsNotWorkedOn(t *testing.T) {
	pool := integrationPool(t)
	f := newDBFixture(t, pool)
	taskID := seedSubtask(t, pool, f, "cancelled")
	msg := taskMessage(t, pool, f, taskID)
	assignmentsBefore := countRows(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`, f.workID)

	replayAgent(t, guardedAgent(pool), msg, 3)

	if n := countRows(t, pool, `SELECT count(*) FROM agent_assignments WHERE work_item_id = $1`, f.workID) - assignmentsBefore; n != 0 {
		t.Errorf("assignments created = %d, want 0", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'cancelled'`, taskID); n != 1 {
		t.Error("the cancelled subtask must stay cancelled")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = 'implementation-agent' AND event_id = $1`, msg.EventID()); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// A subtask still in implementation under a cancelled parent is blocked with
// a reason naming the parent's state and the expectation.
func TestIntegration_Precondition_CancelledParentBlocksSubtask(t *testing.T) {
	pool := integrationPool(t)
	f := newDBFixture(t, pool)
	taskID := seedSubtask(t, pool, f, "implementation")
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET state = 'cancelled' WHERE id = $1`, f.workID); err != nil {
		t.Fatal(err)
	}
	msg := taskMessage(t, pool, f, taskID)

	replayAgent(t, guardedAgent(pool), msg, 3)

	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT w.state::text, t.reason FROM work_items w
		JOIN work_item_transitions t ON t.work_item_id = w.id AND t.to_state = 'blocked' WHERE w.id = $1`, taskID).Scan(&state, &reason); err != nil {
		t.Fatalf("read block: %v", err)
	}
	if state != "blocked" || !strings.Contains(reason, "parent work item "+f.workID.String()+" is cancelled, expected not one of cancelled, blocked") {
		t.Errorf("state=%s reason=%q", state, reason)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'work.blocked'`, f.workID); n != 1 {
		t.Errorf("work.blocked events = %d, want 1", n)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_transitions WHERE work_item_id = $1`, taskID)
	})
}
