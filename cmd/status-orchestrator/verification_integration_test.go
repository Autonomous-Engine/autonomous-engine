package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R14 V4 against a real PostgreSQL (TEST_POSTGRES_DSN, migrated): the
// parent moves implementation → verification → (review | human_review |
// implementation | blocked) on the recorded results, through the guarded
// handler main subscribes.

// recordVerification commits what the verification stage commits for sub
// (the result row, with a verdict artifact for a verdict decision) and
// returns the verification.complete to deliver.
func recordVerification(t *testing.T, pool *pgxpool.Pool, parent, sub uuid.UUID, decision stage.Decision, legacy bool) *bus.Message {
	t.Helper()
	ctx := context.Background()
	head := strings.Repeat("f", 40)
	o := &stage.Outcome{
		Message: stage.Message{WorkID: parent.String(), TaskID: sub.String(), HeadSHA: head, Decision: decision, Reason: "verifier said " + string(decision), Legacy: legacy},
		Result:  stage.Result{WorkID: parent, TaskID: sub, HeadSHA: head, Decision: decision, Reason: "verifier said " + string(decision), Legacy: legacy, VerifierVersion: verifier.Version},
	}
	if decision.FromVerdict() {
		v := verifier.Verdict{SchemaVersion: 1, Overall: verifier.OverallRequestChanges, Defects: []verifier.Defect{
			{Severity: verifier.SeverityMajor, File: "a.go", Line: 3, Category: "logic", Description: "DEFECT-MARKER off by one"}},
			PerCriterion: []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusNotMet, Evidence: []verifier.Ref{"a.go:3"}, Note: "boundary"}}}
		g := verifier.Gate(v)
		o.Artifact = stage.NewArtifact(parent, "review-agent", stage.NewArtifactContent(v, g, string(decision), "agent/x", head, head, sub.String()))
	}
	if err := stage.Commit(ctx, pool, workgraph.New(pool), "review-agent", o); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM verification_results WHERE work_item_id = $1`, parent)
		_, _ = pool.Exec(c, `DELETE FROM work_item_artifacts WHERE work_item_id = $1`, parent)
	})
	msg := eventMessage(t, bus.TopicVerificationComplete, o.Message)
	cleanupLedgerRow(t, pool, msg)
	return msg
}

// seedVerifyingParent is a parent in verification with one passed subtask.
func seedVerifyingParent(t *testing.T, pool *pgxpool.Pool) (parent, sub uuid.UUID) {
	t.Helper()
	parent, sub, msg := seedParentWithSubtask(t, pool, models.WorkItemStateImplementation)
	replay(t, guardedOrchestrator(t, pool), msg, 1)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("parent state = %s, want verification after the last implementation.complete", s)
	}
	return parent, sub
}

func latestReason(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, to string) string {
	t.Helper()
	var reason string
	if err := pool.QueryRow(context.Background(), `SELECT reason FROM work_item_transitions WHERE work_item_id = $1 AND to_state = $2 ORDER BY "timestamp" DESC LIMIT 1`, id, to).Scan(&reason); err != nil {
		t.Fatalf("transition of %s to %s: %v", id, to, err)
	}
	return reason
}

// escalate → human_review with the verdict as the reason and the
// approval:human_review prompt; replays change nothing.
func TestIntegration_Verification_EscalateMovesTheParentToHumanReview(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	msg := recordVerification(t, pool, parent, sub, stage.DecisionEscalate, false)
	replay(t, guardedOrchestrator(t, pool), msg, 5)

	if n := transitionsTo(t, pool, parent, "verification", "human_review"); n != 1 {
		t.Fatalf("verification→human_review transitions = %d, want 1", n)
	}
	if r := latestReason(t, pool, parent, "human_review"); !strings.Contains(r, "independent verifier escalated before the PR") || !strings.Contains(r, "verifier said escalate") {
		t.Errorf("reason = %q", r)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'approval:human_review' AND question LIKE 'No PR was opened.%'`, parent); n != 1 {
		t.Errorf("human-review prompts = %d, want 1", n)
	}
}

// request_changes → implementation, with a fix subtask whose feedback is
// the verdict's defects.
func TestIntegration_Verification_RequestChangesDispatchesAFix(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	msg := recordVerification(t, pool, parent, sub, stage.DecisionRequestChanges, false)
	replay(t, guardedOrchestrator(t, pool), msg, 3)

	if n := transitionsTo(t, pool, parent, "verification", "implementation"); n != 1 {
		t.Fatalf("verification→implementation transitions = %d, want 1", n)
	}
	if r := latestReason(t, pool, parent, "implementation"); r != "verification requested changes" {
		t.Errorf("reason = %q", r)
	}
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks'`, parent).Scan(&payload); err != nil {
		t.Fatalf("fix dispatch: %v", err)
	}
	var fix map[string]any
	_ = json.Unmarshal(payload, &fix)
	if fb, _ := fix["review_feedback"].(string); !strings.Contains(fb, "DEFECT-MARKER") {
		t.Errorf("fix feedback lacks the verdict's defects: %v", fix)
	}
	// The next round: the fix subtask passes and is approved; the earlier
	// round's request_changes does not send the parent back again.
	var fixID uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM work_items WHERE parent_id = $1 AND title = 'Address review feedback'`, parent).Scan(&fixID); err != nil {
		t.Fatal(err)
	}
	done := eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": fixID.String(), "work_id": parent.String(), "success": true})
	cleanupLedgerRow(t, pool, done)
	replay(t, guardedOrchestrator(t, pool), done, 1)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("parent after the fix = %s, want verification (waiting for the fix's verdict)", s)
	}
	replay(t, guardedOrchestrator(t, pool), recordVerification(t, pool, parent, fixID, stage.DecisionApprove, false), 1)
	if s := itemState(t, pool, parent); s != "review" {
		t.Fatalf("parent after the fix's approval = %s, want review", s)
	}
}

// The stage was faster than status-orchestrator: the result is recorded
// before the last implementation.complete is consumed. That delivery walks
// the parent through verification to review at once.
func TestIntegration_Verification_ResultBeforeImplementationComplete(t *testing.T) {
	pool := integrationPool(t)
	parent, sub, done := seedParentWithSubtask(t, pool, models.WorkItemStateImplementation)
	_ = recordVerification(t, pool, parent, sub, stage.DecisionApprovePendingSecondPass, false)
	replay(t, guardedOrchestrator(t, pool), done, 3)
	if n := transitionsTo(t, pool, parent, "implementation", "verification"); n != 1 {
		t.Errorf("implementation→verification = %d, want 1", n)
	}
	if n := transitionsTo(t, pool, parent, "verification", "review"); n != 1 {
		t.Errorf("verification→review = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, done); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// park moves the parent verification → awaiting_user the way a prompt
// does (TransitionToAwaitingUser: previous_state = verification).
func park(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE work_items SET previous_state = state, state = 'awaiting_user' WHERE id = $1 AND state = 'verification'`, parent); err != nil {
		t.Fatal(err)
	}
	if s := itemState(t, pool, parent); s != "awaiting_user" {
		t.Fatalf("parent = %s, want awaiting_user", s)
	}
}

func previous(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var p *string
	if err := pool.QueryRow(context.Background(), `SELECT previous_state FROM work_items WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if p == nil {
		return ""
	}
	return *p
}

func answered(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) *bus.Message {
	t.Helper()
	msg := eventMessage(t, bus.TopicUserPromptAnswered, map[string]any{"prompt_id": uuid.NewString(), "work_item_id": parent.String()})
	cleanupLedgerRow(t, pool, msg)
	return msg
}

// Review M1, path A: the gate prompt parks the parent before the settle.
// The settle records the decision on previous_state (and the transition
// row), and answering the prompt restores the parent to review.
func TestIntegration_Verification_PromptBeforeSettleRecordsTheTarget(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	park(t, pool, parent)
	replay(t, guardedOrchestrator(t, pool), recordVerification(t, pool, parent, sub, stage.DecisionApprovePendingSecondPass, false), 3)

	if s, p := itemState(t, pool, parent), previous(t, pool, parent); s != "awaiting_user" || p != "review" {
		t.Fatalf("parent = %s (previous %s), want awaiting_user with previous review", s, p)
	}
	if n := transitionsTo(t, pool, parent, "verification", "review"); n != 1 {
		t.Errorf("verification→review rows = %d, want 1", n)
	}
	replay(t, guardedOrchestrator(t, pool), answered(t, pool, parent), 1)
	if s := itemState(t, pool, parent); s != "review" {
		t.Errorf("parent after the answer = %s, want review", s)
	}
}

// Re-review minor 2: the exit from verification decides under the row
// lock. A prompt that parked the parent after the settle read its state
// (simulated by parking it before the call) is taken as parked —
// previous_state rewritten, no awaiting_user→review transition, the
// prompt's awaiting state intact; a parent another consumer already moved
// is left alone; a parent still in verification transitions.
func TestIntegration_Verification_ExitDecidesUnderTheRowLock(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)

	parked, _ := seedVerifyingParent(t, pool)
	park(t, pool, parked)
	if err := settleOutOfVerification(ctx, pool, wg, nopBus{}, parked, models.WorkItemStateReview, "all child tasks complete"); err != nil {
		t.Fatal(err)
	}
	if s, p := itemState(t, pool, parked), previous(t, pool, parked); s != "awaiting_user" || p != "review" {
		t.Errorf("parked parent = %s (previous %s), want awaiting_user with previous review", s, p)
	}
	if n := transitionsTo(t, pool, parked, "awaiting_user", "review"); n != 0 {
		t.Errorf("awaiting_user→review transitions = %d, want 0 (previous_state would be stranded)", n)
	}

	moved, _ := seedVerifyingParent(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE work_items SET state = 'blocked' WHERE id = $1`, moved); err != nil {
		t.Fatal(err)
	}
	if err := settleOutOfVerification(ctx, pool, wg, nopBus{}, moved, models.WorkItemStateReview, "x"); err != nil {
		t.Fatal(err)
	}
	if s := itemState(t, pool, moved); s != "blocked" {
		t.Errorf("a parent moved by another consumer = %s, want it left blocked", s)
	}

	plain, _ := seedVerifyingParent(t, pool)
	if err := settleOutOfVerification(ctx, pool, wg, nopBus{}, plain, models.WorkItemStateReview, "x"); err != nil {
		t.Fatal(err)
	}
	if n := transitionsTo(t, pool, plain, "verification", "review"); n != 1 {
		t.Errorf("verification→review = %d, want 1", n)
	}
}

// Review M1, path B: the parent comes back to verification unsettled (its
// verdict was recorded while the settle could not run); the answer settles
// it.
func TestIntegration_Verification_AnswerSettlesAParentBackInVerification(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	park(t, pool, parent)
	_ = recordVerification(t, pool, parent, sub, stage.DecisionApprove, false) // committed, not delivered
	replay(t, guardedOrchestrator(t, pool), answered(t, pool, parent), 1)
	if s := itemState(t, pool, parent); s != "review" {
		t.Fatalf("parent after the answer = %s, want review (settled on return to verification)", s)
	}
	if n := transitionsTo(t, pool, parent, "verification", "review"); n != 1 {
		t.Errorf("verification→review = %d, want 1", n)
	}
}

// Review M2 (all-or-nothing), status-orchestrator side: one subtask
// approved, its sibling escalated → the parent goes to human_review.
func TestIntegration_Verification_OneEscalatedSiblingSendsTheParentToHumanReview(t *testing.T) {
	pool := integrationPool(t)
	parent, a, doneA := seedParentWithSubtask(t, pool, models.WorkItemStateImplementation)
	var product uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT product_id FROM work_items WHERE id = $1`, parent).Scan(&product); err != nil {
		t.Fatal(err)
	}
	b := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO work_items (id, type, state, title, description, parent_id, product_id)
		VALUES ($1, 'subtask', 'implementation', 'R14 sibling', 'd', $2, $3)`, b, parent, product); err != nil {
		t.Fatal(err)
	}
	doneB := eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": b.String(), "work_id": parent.String(), "success": true})
	cleanupLedgerRow(t, pool, doneB)
	h := guardedOrchestrator(t, pool)
	replay(t, h, doneA, 1)
	replay(t, h, doneB, 1)
	replay(t, h, recordVerification(t, pool, parent, a, stage.DecisionApprove, false), 1)
	if s := itemState(t, pool, parent); s != "verification" {
		t.Fatalf("parent = %s, want verification (waiting for the sibling)", s)
	}
	replay(t, h, recordVerification(t, pool, parent, b, stage.DecisionEscalate, false), 1)
	if s := itemState(t, pool, parent); s != "human_review" {
		t.Fatalf("parent = %s, want human_review", s)
	}
}

// Review minor 5: a definition with review, human review and QA disabled
// makes the completion target passed, which verification cannot reach; the
// parent goes to human_review with a prompt instead of sticking.
func TestIntegration_Verification_UnreachableTargetFallsBackToHumanReview(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	ctx := context.Background()
	def := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (id, name, step_review, step_human_review, step_qa, enabled) VALUES ($1, $2, false, false, false, false)`, def, "r14-no-steps-"+def.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE work_items SET agent_definition_id = NULL WHERE id = $1`, parent)
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_definitions WHERE id = $1`, def)
	})
	if _, err := pool.Exec(ctx, `UPDATE work_items SET agent_definition_id = $2 WHERE id = $1`, parent, def); err != nil {
		t.Fatal(err)
	}
	replay(t, guardedOrchestrator(t, pool), recordVerification(t, pool, parent, sub, stage.DecisionApprove, false), 1)
	if s := itemState(t, pool, parent); s != "human_review" {
		t.Fatalf("parent = %s, want human_review", s)
	}
	if r := latestReason(t, pool, parent, "human_review"); !strings.Contains(r, "not reachable from verification") {
		t.Errorf("reason = %q", r)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND trigger = 'approval:human_review'`, parent); n != 1 {
		t.Errorf("human-review prompts = %d, want 1", n)
	}
}

// A stage failure already blocked the parent: its error result changes
// nothing more.
func TestIntegration_Verification_ErrorLeavesTheStagesBlock(t *testing.T) {
	pool := integrationPool(t)
	parent, sub := seedVerifyingParent(t, pool)
	ctx := context.Background()
	if err := transitionAndPublish(ctx, pool, workgraph.New(pool), nopBus{}, parent, models.WorkItemStateBlocked, "Verification before the PR could not run"); err != nil {
		t.Fatal(err)
	}
	msg := recordVerification(t, pool, parent, sub, stage.DecisionError, false)
	replay(t, guardedOrchestrator(t, pool), msg, 2)
	if s := itemState(t, pool, parent); s != "blocked" {
		t.Errorf("parent = %s, want blocked", s)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'blocked'`, parent); n != 1 {
		t.Errorf("blocked transitions = %d, want the stage's one", n)
	}
}
