package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R14 V2 against a real PostgreSQL (TEST_POSTGRES_DSN, migrated): the
// guarded handler main subscribes to verification.complete.

func verificationMessage(t *testing.T, pool *pgxpool.Pool, workID, taskID uuid.UUID, decision stage.Decision) *bus.Message {
	t.Helper()
	msg := completionMessage(t, pool, workID, taskID)
	impl := verifiedImplementation(t, pool, workID, taskID)
	var p implementationCompletePayload
	if err := json.Unmarshal(impl, &p); err != nil {
		t.Fatal(err)
	}
	var ev evidence.ImplementationEvidence
	if err := json.Unmarshal(p.Evidence, &ev); err != nil {
		t.Fatal(err)
	}
	msg.Value = verificationOf(t, impl, decision, ev.HeadSHA)
	return msg
}

// verifiedImplementation is the implementation.complete payload that
// verifiedCompletion wraps.
func verifiedImplementation(t *testing.T, pool *pgxpool.Pool, workID, taskID uuid.UUID) []byte {
	t.Helper()
	var m stage.Message
	if err := json.Unmarshal(verifiedCompletion(t, pool, workID, taskID), &m); err != nil {
		t.Fatal(err)
	}
	return m.Implementation
}

// escalate → no pending change, no pr.gated, no prompt from pr-coordinator
// (status-orchestrator moves the parent); the delivery is recorded.
func TestIntegration_Verification_EscalateOpensNoPR(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "verification")
	msg := verificationMessage(t, pool, workID, taskID, stage.DecisionEscalate)
	for i := 0; i < 3; i++ {
		if err := guardedCoordinator(pool)(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, workID); n != 0 {
		t.Errorf("outbox events = %d, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("prompts = %d, want 0", n)
	}
}

// approve_pending_second_pass → the pending change as before, its title
// labelled, its description carrying "## Verification", the gate prompt
// the verdict (V5).
func TestIntegration_Verification_ApproveGatesThePRWithTheVerdict(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "verification")
	msg := verificationMessage(t, pool, workID, taskID, stage.DecisionApprovePendingSecondPass)
	if err := guardedCoordinator(pool)(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	var title, desc string
	if err := pool.QueryRow(context.Background(), `SELECT pr_title, pr_description FROM pending_pr_changes WHERE work_item_id = $1`, workID).Scan(&title, &desc); err != nil {
		t.Fatalf("pending change: %v", err)
	}
	if !strings.HasPrefix(title, stage.SecondPassTitlePrefix) {
		t.Errorf("title = %q, want the second-pass label", title)
	}
	if !strings.Contains(desc, "## Verification") || !strings.Contains(desc, "**approve_pending_second_pass**") || !strings.Contains(desc, "| AC-1 | met |") {
		t.Errorf("description lacks the verification section:\n%s", desc)
	}
	var question string
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT question, context_payload FROM user_interactions WHERE work_item_id = $1 AND trigger = 'approval:pr_gate'`, workID).Scan(&question, &payload); err != nil {
		t.Fatalf("gate prompt: %v", err)
	}
	if !strings.Contains(question, "Independent verifier: approve_pending_second_pass") {
		t.Errorf("gate question lacks the verdict: %q", question)
	}
	var ctxPayload struct {
		Verification []map[string]any `json:"verification"`
	}
	if err := json.Unmarshal(payload, &ctxPayload); err != nil || len(ctxPayload.Verification) != 1 || ctxPayload.Verification[0]["decision"] != "approve_pending_second_pass" {
		t.Errorf("gate context_payload.verification = %s (%v)", payload, err)
	}
}

// Review M2 (all-or-nothing): two subtasks, one approved, the other
// escalated → no pending change, no pr.gated, no pr.ready, no prompt for
// either; status-orchestrator moves the parent to human_review (tested
// there).
func TestIntegration_Verification_OneEscalatedSiblingStopsEveryPR(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	workID, taskA := seedGatedParent(t, pool, "verification")
	var productID, repoID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT product_id, repository_id FROM work_items WHERE id = $1`, taskA).Scan(&productID, &repoID); err != nil {
		t.Fatal(err)
	}
	taskB := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id) VALUES ($1, 'subtask', 'passed', 'R14 sibling B', $2, $3, $4)`,
		taskB, productID, workID, repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, taskB) })
	h := guardedCoordinator(pool)
	// B's escalation arrives first (A not yet verified: the coordinator
	// waits), then A's approval completes the set.
	msgB := verificationMessage(t, pool, workID, taskB, stage.DecisionEscalate)
	msgA := verificationMessage(t, pool, workID, taskA, stage.DecisionApprove)
	for _, m := range []*bus.Message{msgB, msgA} {
		if err := h(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want 0", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic IN ('pr.gated', 'pr.ready', 'user_prompt.requested')`, workID); n != 0 {
		t.Errorf("gate/PR events = %d, want 0", n)
	}
}

// Re-review minor 1: a sibling that becomes blocked during the 3s re-check
// is judged from the re-read children: it is not part of the release, so
// it does not refuse the approved subtask's gate (all-or-nothing must not
// count it as unverified from the stale first read).
func TestIntegration_Verification_SiblingBlockedDuringRecheckDoesNotRefuse(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	workID, taskA := seedGatedParent(t, pool, "verification")
	var productID, repoID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT product_id, repository_id FROM work_items WHERE id = $1`, taskA).Scan(&productID, &repoID); err != nil {
		t.Fatal(err)
	}
	taskB := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id, parent_id, repository_id) VALUES ($1, 'subtask', 'implementation', 'R14 sibling blocked later', $2, $3, $4)`,
		taskB, productID, workID, repoID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, taskB) })
	// B's releasable evidence is stored but it has no verification result:
	// judged from the stale read (B "in implementation") it would count as
	// an unverified participant and refuse A's gate.
	var p implementationCompletePayload
	if err := json.Unmarshal(verifiedImplementation(t, pool, workID, taskB), &p); err != nil {
		t.Fatal(err)
	}
	var evB evidence.ImplementationEvidence
	if err := json.Unmarshal(p.Evidence, &evB); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.NewStore(pool).Insert(ctx, &evB); err != nil {
		t.Fatal(err)
	}
	// The handler's first read sees B still in implementation; B is
	// blocked during its 3s re-check sleep.
	go func() {
		time.Sleep(time.Second)
		_, _ = pool.Exec(context.Background(), `UPDATE work_items SET state = 'blocked' WHERE id = $1`, taskB)
	}()
	if err := guardedCoordinator(pool)(ctx, verificationMessage(t, pool, workID, taskA, stage.DecisionApprove)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 1 {
		t.Errorf("pending PR rows = %d, want 1 (the blocked sibling must not refuse the approved one)", n)
	}
}

// Restart fallback: the coordinator never saw the subtask's
// verification.complete; a stored escalate result keeps the PR closed.
func TestIntegration_Verification_StoredEscalationBlocksAnUnseenSubtask(t *testing.T) {
	pool := integrationPool(t)
	workID, taskID := seedGatedParent(t, pool, "verification")
	ctx := context.Background()
	// The subtask's evidence is stored (the decideUnseenSubtask input) and
	// its verification recorded as escalated.
	impl := verifiedImplementation(t, pool, workID, taskID)
	var p implementationCompletePayload
	_ = json.Unmarshal(impl, &p)
	var ev evidence.ImplementationEvidence
	if err := json.Unmarshal(p.Evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.NewStore(pool).Insert(ctx, &ev); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO verification_results (id, work_item_id, task_id, head_sha, decision, reason) VALUES ($1, $2, $3, $4, 'escalate', 'stored escalation')`,
		uuid.New(), workID, taskID, ev.HeadSHA); err != nil {
		t.Fatal(err)
	}
	// A sibling's (approved) message triggers the PR loop.
	sibling := uuid.New()
	var productID, repoID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT product_id, repository_id FROM work_items WHERE id = $1`, taskID).Scan(&productID, &repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id, parent_id) VALUES ($1, 'subtask', 'blocked', 'sibling', $2, $3)`, sibling, productID, workID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, sibling) })
	sib := completionMessage(t, pool, workID, sibling)
	if err := guardedCoordinator(pool)(ctx, sib); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, `SELECT count(*) FROM pending_pr_changes WHERE work_item_id = $1`, workID); n != 0 {
		t.Errorf("pending PR rows = %d, want 0 (the stored escalation must keep the PR closed)", n)
	}
}
