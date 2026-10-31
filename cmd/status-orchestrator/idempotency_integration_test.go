package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-05: each workflow event is replayed into the real status-orchestrator
// handler (the one main subscribes, wrapped by Ledger.Handler) against a
// real PostgreSQL, and exactly one logical side effect must result. The bus
// is a recorder: state.changed is the only direct publish.

// nopBus swallows the state.changed telemetry.
type nopBus struct{ bus.MessageBus }

func (nopBus) Publish(context.Context, string, string, []byte) error { return nil }

func guardedOrchestrator(t *testing.T, pool *pgxpool.Pool) bus.Handler {
	t.Helper()
	inner := buildHandler(workgraph.New(pool), nopBus{}, agentregistry.New(pool), products.NewStore(pool),
		evidence.CompatWindowLegacy, 3, true)
	return idempotency.NewLedger(pool).Handler(idempotencyConsumer, inner)
}

func eventMessage(t *testing.T, topic string, payload any) *bus.Message {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	return &bus.Message{Topic: topic, Key: "k", Value: b, Headers: map[string]string{bus.HeaderEventID: id.String()}}
}

func replay(t *testing.T, h bus.Handler, msg *bus.Message, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := h(context.Background(), msg); err != nil {
			t.Fatalf("delivery %d of %s: %v", i+1, msg.Topic, err)
		}
	}
}

func ledgerRowsFor(t *testing.T, pool *pgxpool.Pool, msg *bus.Message) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM processed_events WHERE consumer = $1 AND event_id = $2`,
		idempotencyConsumer, msg.EventID())
}

func cleanupLedgerRow(t *testing.T, pool *pgxpool.Pool, msg *bus.Message) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM processed_events WHERE event_id = $1`, msg.EventID())
	})
}

func TestIntegration_Idempotency_ReviewChangesRequestedReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	item, _ := seedItem(t, pool, models.WorkItemStateReview)
	msg := eventMessage(t, bus.TopicReviewComplete, reviewCompletePayload{WorkID: item.String(), Decision: "changes_requested", Feedback: "rename the flag"})
	cleanupLedgerRow(t, pool, msg)

	replay(t, guardedOrchestrator(t, pool), msg, 10)

	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE parent_id = $1 AND title = 'Address review feedback'`, item); n != 1 {
		t.Errorf("fix subtasks = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND reason = 'review requested changes'`, item); n != 1 {
		t.Errorf("review→implementation transitions = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.tasks'`, item); n != 1 {
		t.Errorf("implementation.tasks events = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// A crash after review→implementation committed but before the fix subtask
// did: that transition is unguarded, so the redelivery is not a duplicate and
// still dispatches the fix, once.
func TestIntegration_Idempotency_ReviewChangesRequestedCrashBeforeFix(t *testing.T) {
	pool := integrationPool(t)
	item, _ := seedItem(t, pool, models.WorkItemStateReview)
	msg := eventMessage(t, bus.TopicReviewComplete, reviewCompletePayload{WorkID: item.String(), Decision: "changes_requested", Feedback: "rename the flag"})
	cleanupLedgerRow(t, pool, msg)

	// The first delivery's first transaction, then the crash.
	if err := transitionAndPublish(context.Background(), pool, workgraph.New(pool), nopBus{}, item,
		models.WorkItemStateImplementation, "review requested changes"); err != nil {
		t.Fatal(err)
	}
	replay(t, guardedOrchestrator(t, pool), msg, 3)

	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE parent_id = $1 AND title = 'Address review feedback'`, item); n != 1 {
		t.Errorf("fix subtasks = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

func TestIntegration_Idempotency_QABugsReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	item, _ := seedItem(t, pool, models.WorkItemStateQA)
	msg := eventMessage(t, bus.TopicQABugs, qaBugsPayload{WorkID: item.String(), Bugs: "FAIL: login"})
	cleanupLedgerRow(t, pool, msg)

	replay(t, guardedOrchestrator(t, pool), msg, 10)

	// Pre-existing (not M1-05): the state machine has no failed →
	// implementation edge, so the retry path stops at failed and dispatches
	// no fix subtask. What this test pins is that ten deliveries produce the
	// same single outcome as one.
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'failed'`, item); n != 1 {
		t.Errorf("transitions to failed = %d, want 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_items WHERE parent_id = $1`, item); n > 1 {
		t.Errorf("fix subtasks = %d, want at most 1", n)
	}
	if s := itemState(t, pool, item); s != "failed" {
		t.Errorf("state = %s, want failed (see above)", s)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// qa.bugs past the retry limit: blocked with one escalation prompt.
func TestIntegration_Idempotency_QABugsCircuitBreakerReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	item, _ := seedItem(t, pool, models.WorkItemStateQA)
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(context.Background(), `INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason, timestamp)
			VALUES ($1, $2, 'qa', 'failed', 'm105-test', 'earlier QA failure', now())`, uuid.New(), item); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_transitions WHERE work_item_id = $1`, item)
	})
	msg := eventMessage(t, bus.TopicQABugs, qaBugsPayload{WorkID: item.String(), Bugs: "FAIL: login"})
	cleanupLedgerRow(t, pool, msg)

	replay(t, guardedOrchestrator(t, pool), msg, 10)

	if s := itemState(t, pool, item); s != "blocked" {
		t.Errorf("state = %s, want blocked", s)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND prompt_type = 'escalation'`, item); n != 1 {
		t.Errorf("escalation prompts = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

func TestIntegration_Idempotency_WorkBlockedReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	item, _ := seedItem(t, pool, models.WorkItemStateImplementation)
	msg := eventMessage(t, bus.TopicWorkBlocked, workBlockedPayload{WorkID: item.String(), Reason: "external block"})
	cleanupLedgerRow(t, pool, msg)

	replay(t, guardedOrchestrator(t, pool), msg, 10)

	if s := itemState(t, pool, item); s != "blocked" {
		t.Errorf("state = %s, want blocked", s)
	}
	if n := count(t, pool, `SELECT count(*) FROM user_interactions WHERE work_item_id = $1 AND prompt_type = 'escalation'`, item); n != 1 {
		t.Errorf("escalation prompts = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// implementation.complete for the last subtask: the subtask's own transition
// is unguarded (idempotent by itself) and the guard rides on the parent's,
// which (R14) is the move to verification; verification.complete then moves
// it to review, once over ten deliveries of each.
func TestIntegration_Idempotency_ImplementationCompleteReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	parent, product := seedItem(t, pool, models.WorkItemStateImplementation)
	sub := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO work_items (id, type, state, title, description, parent_id, product_id)
		VALUES ($1, 'subtask', 'implementation', 'M1-05 subtask', 'd', $2, $3)`, sub, parent, product); err != nil {
		t.Fatal(err)
	}
	msg := eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": sub.String(), "work_id": parent.String(), "success": true})
	cleanupLedgerRow(t, pool, msg)

	replay(t, guardedOrchestrator(t, pool), msg, 10)

	if s := itemState(t, pool, sub); s != "passed" {
		t.Errorf("subtask state = %s, want passed", s)
	}
	if s := itemState(t, pool, parent); s != "verification" {
		t.Errorf("parent state = %s, want verification", s)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'verification'`, parent); n != 1 {
		t.Errorf("parent transitions to verification = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}

	vmsg := recordVerification(t, pool, parent, sub, "approve", false)
	replay(t, guardedOrchestrator(t, pool), vmsg, 10)
	if s := itemState(t, pool, parent); s != "review" {
		t.Errorf("parent state = %s, want review", s)
	}
	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'review'`, parent); n != 1 {
		t.Errorf("parent transitions to review = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, vmsg); n != 1 {
		t.Errorf("verification.complete ledger rows = %d, want 1", n)
	}
}

// seedParentWithSubtask creates a parent in parentState with one subtask in
// implementation, and the implementation.complete message for the subtask.
func seedParentWithSubtask(t *testing.T, pool *pgxpool.Pool, parentState models.WorkItemState) (parent, sub uuid.UUID, msg *bus.Message) {
	t.Helper()
	parent, product := seedItem(t, pool, parentState)
	sub = uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO work_items (id, type, state, title, description, parent_id, product_id)
		VALUES ($1, 'subtask', 'implementation', 'M1-05 subtask', 'd', $2, $3)`, sub, parent, product); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_item_transitions WHERE work_item_id IN ($1, $2)`, parent, sub)
	})
	msg = eventMessage(t, bus.TopicImplementationComplete, map[string]any{"task_id": sub.String(), "work_id": parent.String(), "success": true})
	cleanupLedgerRow(t, pool, msg)
	return parent, sub, msg
}

func transitionsTo(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, from, to string) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND from_state = $2 AND to_state = $3`, id, from, to)
}

// Crash point 1 (guard on the second write): the subtask's unguarded
// implementation→passed committed, then the process died before the parent's
// guarded transition. The redelivery is not a duplicate, skips the subtask
// and moves the parent to verification (R14) — exactly once over three
// deliveries.
func TestIntegration_Idempotency_ImplementationCompleteCrashAfterSubtaskTransition(t *testing.T) {
	pool := integrationPool(t)
	parent, sub, msg := seedParentWithSubtask(t, pool, models.WorkItemStateImplementation)
	if err := transitionAndPublish(context.Background(), pool, workgraph.New(pool), nopBus{}, sub,
		models.WorkItemStatePassed, "implementation complete (first delivery, before the crash)"); err != nil {
		t.Fatal(err)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 0 {
		t.Fatalf("ledger rows before the redelivery = %d, want 0 (the subtask write is unguarded)", n)
	}

	replay(t, guardedOrchestrator(t, pool), msg, 3)

	if s := itemState(t, pool, parent); s != "verification" {
		t.Errorf("parent state = %s, want verification", s)
	}
	if n := transitionsTo(t, pool, parent, "implementation", "verification"); n != 1 {
		t.Errorf("parent implementation→verification transitions = %d, want 1", n)
	}
	if n := transitionsTo(t, pool, sub, "implementation", "passed"); n != 1 {
		t.Errorf("subtask implementation→passed transitions = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// A parent still in planning: the planning→implementation step and the
// final →verification step (R14) each happen once over ten deliveries.
func TestIntegration_Idempotency_ImplementationCompleteFromPlanningReplayedTenTimes(t *testing.T) {
	pool := integrationPool(t)
	parent, sub, msg := seedParentWithSubtask(t, pool, models.WorkItemStatePlanning)

	replay(t, guardedOrchestrator(t, pool), msg, 10)

	if s := itemState(t, pool, sub); s != "passed" {
		t.Errorf("subtask state = %s, want passed", s)
	}
	if s := itemState(t, pool, parent); s != "verification" {
		t.Errorf("parent state = %s, want verification", s)
	}
	if n := transitionsTo(t, pool, parent, "planning", "implementation"); n != 1 {
		t.Errorf("parent planning→implementation transitions = %d, want 1", n)
	}
	if n := transitionsTo(t, pool, parent, "implementation", "verification"); n != 1 {
		t.Errorf("parent implementation→verification transitions = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// Crash point 2: the subtask's transition and the parent's unguarded
// planning→implementation both committed, then the process died before the
// guarded →verification. The redelivery skips both done steps and finishes
// once.
func TestIntegration_Idempotency_ImplementationCompleteCrashAfterParentPlanningStep(t *testing.T) {
	pool := integrationPool(t)
	parent, sub, msg := seedParentWithSubtask(t, pool, models.WorkItemStatePlanning)
	ctx := context.Background()
	wg := workgraph.New(pool)
	if err := transitionAndPublish(ctx, pool, wg, nopBus{}, sub, models.WorkItemStatePassed, "implementation complete (first delivery)"); err != nil {
		t.Fatal(err)
	}
	if err := transitionAndPublish(ctx, pool, wg, nopBus{}, parent, models.WorkItemStateImplementation, "child tasks running"); err != nil {
		t.Fatal(err)
	}

	replay(t, guardedOrchestrator(t, pool), msg, 3)

	if s := itemState(t, pool, parent); s != "verification" {
		t.Errorf("parent state = %s, want verification", s)
	}
	if n := transitionsTo(t, pool, parent, "planning", "implementation"); n != 1 {
		t.Errorf("parent planning→implementation transitions = %d, want 1", n)
	}
	if n := transitionsTo(t, pool, parent, "implementation", "verification"); n != 1 {
		t.Errorf("parent implementation→verification transitions = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}

// The restart-between-receipt-and-ack case: the handler's transaction
// committed (QA passed) and the process died before the offset was
// committed. Simulated by a wrapper that runs the real handler and then
// fails: Ledger.Handler returns that error (the bus would not commit the
// offset), and the redelivery is acknowledged without a second transition.
func TestIntegration_Idempotency_RestartBetweenCommitAndAck(t *testing.T) {
	pool := integrationPool(t)
	item, _ := seedItem(t, pool, models.WorkItemStateQA)
	msg := eventMessage(t, bus.TopicQAResult, qaResultPayload{WorkID: item.String(), Result: "pass"})
	cleanupLedgerRow(t, pool, msg)

	inner := buildHandler(workgraph.New(pool), nopBus{}, agentregistry.New(pool), products.NewStore(pool),
		evidence.CompatWindowLegacy, 3, true)
	ledger := idempotency.NewLedger(pool)
	errCrash := errors.New("process killed before the offset commit")
	crashing := ledger.Handler(idempotencyConsumer, func(ctx context.Context, m *bus.Message) error {
		if err := inner(ctx, m); err != nil {
			return err
		}
		return errCrash
	})
	if err := crashing(context.Background(), msg); !errors.Is(err, errCrash) {
		t.Fatalf("first delivery err = %v, want the crash (offset not committed)", err)
	}
	if s := itemState(t, pool, item); s != "passed" {
		t.Fatalf("state after first delivery = %s, want passed (the transaction committed)", s)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Fatalf("ledger rows after the committed transaction = %d, want 1", n)
	}

	// Redelivery after the restart, to the normal handler.
	replay(t, ledger.Handler(idempotencyConsumer, inner), msg, 1)

	if n := count(t, pool, `SELECT count(*) FROM work_item_transitions WHERE work_item_id = $1 AND to_state = 'passed'`, item); n != 1 {
		t.Errorf("transitions to passed = %d, want 1", n)
	}
	if n := ledgerRowsFor(t, pool, msg); n != 1 {
		t.Errorf("ledger rows = %d, want 1", n)
	}
}
