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
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-05: the final implementation.complete of a manual-gate work item is
// replayed into pr-coordinator's guarded handler (the one main subscribes)
// against a real PostgreSQL: exactly one pending PR row and one pr.gated
// event must result. A manual gate is used because an auto gate calls
// github-service, which a fixture repository cannot satisfy.

// seedGatedParent creates a manual-gate parent in state with one passed
// subtask in a fixture repository, and returns the parent and subtask ids.
func seedGatedParent(t *testing.T, pool *pgxpool.Pool, state string) (workID, taskID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	productID, workID := seedWorkItem(t, pool)
	repoID, taskID := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE work_items SET pr_gate_mode = 'manual', state = $2 WHERE id = $1`, []any{workID, state}},
		{`INSERT INTO repositories (id, product_id, name, github_url, clone_path, base_branch)
			VALUES ($1, $2, $3, 'https://example.invalid/m105.git', '/nonexistent/m105', 'main')`, []any{repoID, productID, "m105-repo-" + repoID.String()[:8]}},
		{`INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id)
			VALUES ($1, 'subtask', 'passed', 'M1-05 subtask', $2, $3, $4)`, []any{taskID, productID, workID, repoID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed (%s): %v", q.sql, err)
		}
	}
	// Registered after seedWorkItem's cleanup, so it runs first.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM work_items WHERE id = $1`, taskID)
		_, _ = pool.Exec(c, `DELETE FROM repositories WHERE id = $1`, repoID)
		_, _ = pool.Exec(c, `DELETE FROM agent_assignments WHERE work_item_id = $1`, workID)
	})
	return workID, taskID
}

func guardedCoordinator(pool *pgxpool.Pool) bus.Handler {
	h := buildHandler(workgraph.New(pool), interactions.NewPublisher(nil), newTracker(), "http://github-service.invalid",
		products.NewStore(pool), evidence.NewStore(pool), evidence.CompatWindowLegacy)
	return idempotency.NewLedger(pool).Handler(idempotencyConsumer, h)
}

// completionMessage is the verification.complete the stage publishes for a
// legacy implementation.complete (no evidence, success): not_run, Legacy,
// the pre-evidence path the legacy window keeps (R14).
func completionMessage(t *testing.T, pool *pgxpool.Pool, workID, taskID uuid.UUID) *bus.Message {
	t.Helper()
	b, err := json.Marshal(map[string]any{"task_id": taskID.String(), "work_id": workID.String(), "success": true})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, id)
	})
	return &bus.Message{Topic: bus.TopicVerificationComplete, Key: workID.String(), Value: verificationOf(t, b, stage.DecisionNotRun, ""),
		Headers: map[string]string{bus.HeaderEventID: id.String()}}
}

// verificationOf wraps an implementation.complete payload in the
// verification.complete the stage would publish with decision for head.
func verificationOf(t *testing.T, impl []byte, decision stage.Decision, head string) []byte {
	t.Helper()
	var p implementationCompletePayload
	if err := json.Unmarshal(impl, &p); err != nil {
		t.Fatal(err)
	}
	m := stage.Message{
		WorkID: p.WorkID, TaskID: p.TaskID, HeadSHA: head, Decision: decision, Reason: "test " + string(decision),
		Legacy: !p.hasEvidence(), Implementation: impl, ResultID: uuid.NewString(),
		Criteria: []stage.CriterionSummary{{ID: "AC-1", Status: "met"}},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIntegration_Idempotency_GatedPRReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "implementation")
	msg := completionMessage(t, pool, workID, taskID)

	h := guardedCoordinator(pool)
	for i := 0; i < 10; i++ {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 1 {
		t.Errorf("pending PR rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated'`, workID); n != 1 {
		t.Errorf("pr.gated events = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, idempotencyConsumer, msg.EventID()); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

func ledgerRow(t *testing.T, pool *pgxpool.Pool, msg *bus.Message) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`, idempotencyConsumer, msg.EventID())
}

// Precondition (deny-list): with every post-implementation step disabled,
// status-orchestrator moves the parent to passed on the same
// implementation.complete and wins the race against pr-coordinator's 3s
// re-check. The PR must still be opened and recorded.
func TestIntegration_Precondition_PassedParentStillGetsItsPR(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "passed")
	msg := completionMessage(t, pool, workID, taskID)

	if err := guardedCoordinator(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 1 {
		t.Errorf("pending PR rows = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'pr.gated'`, workID); n != 1 {
		t.Errorf("pr.gated events = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'passed'`, workID); n != 1 {
		t.Error("a passed parent must stay passed")
	}
	if n := ledgerRow(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// A cancelled parent gets no PR; it cannot be blocked, so it is left alone,
// and the stale event is recorded so it is not replayed.
func TestIntegration_Precondition_CancelledParentGetsNoPR(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "cancelled")
	msg := completionMessage(t, pool, workID, taskID)

	if err := guardedCoordinator(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, workID); n != 0 {
		t.Errorf("outbox events = %d, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE id = $1 AND state = 'cancelled'`, workID); n != 1 {
		t.Error("a cancelled parent must stay cancelled")
	}
	if n := ledgerRow(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1 (the stale event is recorded)", n)
	}
}

// A failed parent gets no PR and, having an edge to blocked, is blocked with
// a reason naming the observed state and the refused set.
func TestIntegration_Precondition_FailedParentIsBlockedNotGated(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "failed")
	msg := completionMessage(t, pool, workID, taskID)

	if err := guardedCoordinator(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want 0", n)
	}
	var state, reason string
	if err := pool.QueryRow(context.Background(), `SELECT w.state::text, t.reason FROM work_items w
		JOIN work_item_transitions t ON t.work_item_id = w.id AND t.to_state = 'blocked' WHERE w.id = $1`, workID).Scan(&state, &reason); err != nil {
		t.Fatalf("read block: %v", err)
	}
	if state != "blocked" || !strings.Contains(reason, "is failed, expected not one of cancelled, blocked, failed, rejected") {
		t.Errorf("state=%s reason=%q", state, reason)
	}
	if n := ledgerRow(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1 (committed with the block)", n)
	}
}
