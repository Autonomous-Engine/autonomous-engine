package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 1 review: pr-coordinator knows the codex re-plan
// round (workgraph.RoundStart, shared with status-orchestrator).

func mustExecSQL(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// transitionAt records a transition at an explicit time.
func transitionAt(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, from, to, reason string, at time.Time) {
	t.Helper()
	mustExecSQL(t, pool, `INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason, "timestamp") VALUES ($1, $2, $3, $4, 'test', $5, $6)`,
		uuid.New(), id, from, to, reason, at)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_transitions WHERE work_item_id = $1`, id)
	})
}

// sibling adds a subtask of workID in the same repository.
func sibling(t *testing.T, pool *pgxpool.Pool, workID, like uuid.UUID, state string, created time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecSQL(t, pool, `INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id, created_at)
		SELECT $1, 'subtask', $2, 'codex re-plan sibling', product_id, parent_id, repository_id, $4 FROM work_items WHERE id = $3`, id, state, like, created)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, id) })
	return id
}

func pendingBranches(t *testing.T, pool *pgxpool.Pool, workID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT branch FROM pending_pr_changes WHERE work_item_id = $1 ORDER BY branch`, workID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

func branchOf(workID, taskID uuid.UUID) string {
	return "agent/" + workID.String() + "/" + taskID.String()[:8]
}

const codexReplanReason = "codex_changed: modules a (v1 → v2)"

// Scenario A: the parent was re-planned and is in planning; a superseded
// subtask of the old plan still finishes and the verifier approves it. No
// PR or pending change is opened for it.
func TestIntegration_CodexReplan_StaleSubtaskApprovedWhileParentPlanningOpensNoPR(t *testing.T) {
	pool := integrationPool(t)
	now := time.Now().UTC()
	workID, stale := seedGatedParent(t, pool, "planning")
	mustExecSQL(t, pool, `UPDATE work_items SET state = 'blocked', created_at = $2 WHERE id = $1`, stale, now.Add(-2*time.Hour))
	transitionAt(t, pool, workID, "implementation", "planning", codexReplanReason, now.Add(-time.Minute))
	transitionAt(t, pool, stale, "implementation", "blocked", "codex_changed: superseded by the re-plan of the parent (modules a (v1 → v2))", now.Add(-time.Minute))

	if err := guardedCoordinator(pool)(context.Background(), verificationMessage(t, pool, workID, stale, stage.DecisionApprove)); err != nil {
		t.Fatal(err)
	}
	if b := pendingBranches(t, pool, workID); len(b) != 0 {
		t.Fatalf("pending changes %v for a superseded subtask", b)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic IN ('pr.gated', 'pr.ready')`, workID); n != 0 {
		t.Fatalf("PR events = %d, want 0", n)
	}
}

// Scenario B: the old plan's PR was opened in the first round; after the
// codex re-plan only the new plan's subtask is in the round. The old passed
// subtasks are not in the new PR set (even though this process saw one of
// them) and do not take part in all-or-nothing (one of them was escalated);
// the per-round latch lets the new round open its PR.
func TestIntegration_CodexReplan_OldRoundIsExcludedFromTheNewRoundsPRSet(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	workID, old := seedGatedParent(t, pool, "verification")
	mustExecSQL(t, pool, `UPDATE work_items SET created_at = $2 WHERE id = $1`, old, now.Add(-3*time.Hour))
	transitionAt(t, pool, old, "implementation", "passed", "verified", now.Add(-2*time.Hour))

	// One process for both rounds (the in-process latch).
	h := idempotency.NewLedger(pool).Handler(idempotencyConsumer, buildHandler(workgraph.New(pool), interactions.NewPublisher(nil), newTracker(), "http://github-service.invalid",
		products.NewStore(pool), evidence.NewStore(pool), evidence.CompatWindowLegacy))
	if err := h(ctx, verificationMessage(t, pool, workID, old, stage.DecisionApprove)); err != nil {
		t.Fatal(err)
	}
	if b := pendingBranches(t, pool, workID); len(b) != 1 || b[0] != branchOf(workID, old) {
		t.Fatalf("first round pending changes = %v", b)
	}

	// Another old-plan subtask passed but was escalated by the verifier.
	escalated := sibling(t, pool, workID, old, "passed", now.Add(-3*time.Hour))
	transitionAt(t, pool, escalated, "implementation", "passed", "verified", now.Add(-2*time.Hour))
	if err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return stage.InsertResultTx(ctx, tx, &stage.Result{WorkID: workID, TaskID: escalated, HeadSHA: strings.Repeat("e", 40), Decision: stage.DecisionEscalate, Reason: "old plan escalated", CreatedAt: now.Add(-2 * time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM verification_results WHERE work_item_id = $1`, workID)
	})

	// The codex re-plan, and the new plan's subtask, verified and approved.
	transitionAt(t, pool, workID, "implementation", "planning", codexReplanReason, now.Add(-time.Hour))
	transitionAt(t, pool, workID, "planning", "implementation", "child tasks running", now.Add(-50*time.Minute))
	fresh := sibling(t, pool, workID, old, "passed", now.Add(-40*time.Minute))
	transitionAt(t, pool, fresh, "implementation", "passed", "verified", now.Add(-10*time.Minute))
	if err := h(ctx, verificationMessage(t, pool, workID, fresh, stage.DecisionApprove)); err != nil {
		t.Fatal(err)
	}
	got := pendingBranches(t, pool, workID)
	want := []string{branchOf(workID, old), branchOf(workID, fresh)}
	if len(got) != 2 || !(got[0] == want[0] && got[1] == want[1] || got[0] == want[1] && got[1] == want[0]) {
		t.Fatalf("pending changes = %v, want the first round's %s and the new round's %s only", got, want[0], want[1])
	}
}

// Finding 3: while the parent has a pending codex:replan escalation (parked
// in awaiting_user, subtasks still running), no PR is opened.
func TestIntegration_CodexReplan_PendingEscalationHoldsThePR(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "implementation")
	var productID uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT product_id FROM work_items WHERE id = $1`, workID).Scan(&productID); err != nil {
		t.Fatal(err)
	}
	prompt := uuid.New()
	mustExecSQL(t, pool, `INSERT INTO user_interactions (id, work_item_id, product_id, asking_agent, trigger, prompt_type, headline, question, state)
		VALUES ($1, $2, $3, 'status-orchestrator', $4, 'escalation', 'Codex changed', 'q', 'pending')`, prompt, workID, productID, string(interactions.TriggerCodexReplan))
	mustExecSQL(t, pool, `UPDATE work_items SET previous_state = state, state = 'awaiting_user', awaiting_prompt_id = $2 WHERE id = $1`, workID, prompt)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE work_items SET awaiting_prompt_id = NULL WHERE id = $1`, workID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_interactions WHERE id = $1`, prompt)
	})
	msg := verificationMessage(t, pool, workID, taskID, stage.DecisionApprove)
	if err := guardedCoordinator(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if b := pendingBranches(t, pool, workID); len(b) != 0 {
		t.Fatalf("pending changes %v opened while the codex re-plan escalation is pending", b)
	}
}
