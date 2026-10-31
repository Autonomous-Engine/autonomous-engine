package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R17 L4 against a real PostgreSQL (TEST_POSTGRES_DSN, migrated): the
// parent's transitions form one chain (each from_state is the previous
// to_state) and show implementation, on live run attempt 14's sequence.

type transitionRow struct{ from, to, agent string }

func parentHistory(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) []transitionRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT COALESCE(from_state::text, ''), to_state::text, agent_id FROM work_item_transitions WHERE work_item_id = $1 ORDER BY "timestamp", id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []transitionRow
	for rows.Next() {
		var r transitionRow
		if err := rows.Scan(&r.from, &r.to, &r.agent); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func requireChain(t *testing.T, h []transitionRow, start string, want ...string) {
	t.Helper()
	prev := start
	var got []string
	for _, r := range h {
		if r.from != prev {
			t.Errorf("transition %s → %s (%s) does not start where the previous one ended (%s)", r.from, r.to, r.agent, prev)
		}
		prev = r.to
		got = append(got, r.from+"→"+r.to)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("history = %v, want %v", got, want)
	}
}

// seedPrompt stores an escalation prompt for item (as the producer that
// raises it does) and returns the user_prompt.requested to deliver.
func seedPrompt(t *testing.T, pool *pgxpool.Pool, item, product uuid.UUID, trigger string) (uuid.UUID, *bus.Message) {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO user_interactions (id, work_item_id, product_id, asking_agent, trigger, prompt_type, headline, question, state)
		VALUES ($1, $2, $3, 'implementation-agent', $4, 'escalation', 'h', 'q', 'pending')`, id, item, product, trigger); err != nil {
		t.Fatal(err)
	}
	msg := eventMessage(t, bus.TopicUserPromptRequested, map[string]any{"id": id.String(), "work_item_id": item.String()})
	cleanupLedgerRow(t, pool, msg)
	return id, msg
}

func productIDOf(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) uuid.UUID {
	t.Helper()
	var p uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT product_id FROM work_items WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Attempt 14: the planner dispatches the subtask; the budget stop raises
// its prompt and blocks the parent (guards.BlockWorkItemTx, as
// implementation-agent does); the prompt parks the blocked parent; the
// subtask's implementation.complete blocks it; the planner blocks the
// parent again on the subtask failure. Each delivery is replayed.
func TestIntegration_Bookkeeping_BlockedParentWhoseSubtaskIsInImplementation(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	parent, sub, _ := seedParentWithSubtask(t, pool, models.WorkItemStatePlanning)
	h := guardedOrchestrator(t, pool)

	dispatched := eventMessage(t, bus.TopicImplementationTasks, map[string]any{"task_id": sub.String(), "work_id": parent.String()})
	cleanupLedgerRow(t, pool, dispatched)
	replay(t, h, dispatched, 3)
	if s := itemState(t, pool, parent); s != "implementation" {
		t.Fatalf("parent after its task was dispatched = %s, want implementation", s)
	}

	_, prompt := seedPrompt(t, pool, parent, productIDOf(t, pool, parent), "budget:exhausted")
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, parent, parent, "implementation-agent", "budget_exhausted: tokens")
	}); err != nil {
		t.Fatal(err)
	}
	replay(t, h, prompt, 3)
	if s := itemState(t, pool, parent); s != "awaiting_user" {
		t.Fatalf("parent after its prompt = %s, want awaiting_user", s)
	}

	failed := eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": sub.String(), "work_id": parent.String(), "success": false, "error": "budget_exhausted"})
	cleanupLedgerRow(t, pool, failed)
	replay(t, h, failed, 3)
	if s := itemState(t, pool, sub); s != "blocked" {
		t.Fatalf("subtask = %s, want blocked", s)
	}
	if err := guards.BlockWorkItem(ctx, workgraph.New(pool), nil, parent, "implementation-planner", "Subtask failed"); err != nil {
		t.Fatal(err)
	}

	requireChain(t, parentHistory(t, pool, parent), "planning",
		"planning→implementation", "implementation→blocked", "blocked→awaiting_user", "awaiting_user→blocked")
	if n := transitionsTo(t, pool, parent, "planning", "blocked"); n != 0 {
		t.Errorf("planning→blocked rows = %d, want 0 (the parent was in implementation)", n)
	}
}

// A parent whose implementation.tasks walk was missed is recorded in
// implementation by its subtask's implementation.complete, then walks to
// verification — one row each, one chain.
func TestIntegration_Bookkeeping_ImplementationCompleteWalksAPlanningParent(t *testing.T) {
	pool := integrationPool(t)
	parent, _, done := seedParentWithSubtask(t, pool, models.WorkItemStatePlanning)
	replay(t, guardedOrchestrator(t, pool), done, 3)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("parent = %s, want verification", s)
	}
	requireChain(t, parentHistory(t, pool, parent), "planning", "planning→implementation", "implementation→verification")
}

// Parking on a prompt and the answer's restore are recorded, from the
// state read under the lock; a second prompt while parked changes nothing.
func TestIntegration_Bookkeeping_PromptParkAndRestoreAreRecorded(t *testing.T) {
	pool := integrationPool(t)
	item, product := seedItem(t, pool, models.WorkItemStateImplementation)
	h := guardedOrchestrator(t, pool)
	id, asked := seedPrompt(t, pool, item, product, "approval:x")
	replay(t, h, asked, 3)
	_, again := seedPrompt(t, pool, item, product, "approval:y")
	replay(t, h, again, 1)
	answered := eventMessage(t, bus.TopicUserPromptAnswered, map[string]any{"prompt_id": id.String(), "work_item_id": item.String()})
	cleanupLedgerRow(t, pool, answered)
	replay(t, h, answered, 3)
	if s := itemState(t, pool, item); s != "implementation" {
		t.Fatalf("item after the answer = %s, want implementation", s)
	}
	requireChain(t, parentHistory(t, pool, item), "implementation", "implementation→awaiting_user", "awaiting_user→implementation")
	if r := latestReason(t, pool, item, "awaiting_user"); r != "awaiting user: prompt "+id.String() {
		t.Errorf("reason = %q", r)
	}
}
