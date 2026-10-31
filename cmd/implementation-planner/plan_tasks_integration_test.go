package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1-06 integration tests: the planner's dependency state lives in plan_tasks
// and plan_task_dependencies (migration 048), and every release happens in the
// transaction that justifies it. Nothing here constructs any in-memory planner
// state, because there is none: each handler call, and the startup scan, is
// exactly what a freshly restarted process would run.
//
// Env-gated like the M1-03 tests (TEST_POSTGRES_DSN / TEST_DATABASE_URL).

// chainPlan builds an n-subtask decomposition in which subtask i depends on
// subtask i-1 (A -> B -> C ...), resolved through applyPlanDependencies
// exactly as handleTechnicalBrief resolves an LLM plan.
func (f fixture) chainPlan(t *testing.T, n int) ([]plannedSubtask, []uuid.UUID) {
	t.Helper()
	plan, ids := f.multiGroupPlan(n)
	var known []string
	for i := range plan {
		step := fmt.Sprintf("step %c", 'A'+i)
		plan[i].titles = []string{step}
		known = append(known, step)
		if i > 0 {
			plan[i].dependsOnTitles = []string{fmt.Sprintf("step %c", 'A'+i-1)}
		}
	}
	if _, err := applyPlanDependencies(plan, titleSteps(known...)); err != nil {
		t.Fatalf("applyPlanDependencies: %v", err)
	}
	return plan, ids
}

func planStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) planTaskStatus {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM plan_tasks WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("plan_tasks status of %s: %v", id, err)
	}
	return planTaskStatus(s)
}

func assertStatuses(t *testing.T, pool *pgxpool.Pool, ids []uuid.UUID, want ...planTaskStatus) {
	t.Helper()
	for i, id := range ids {
		if got := planStatus(t, pool, id); got != want[i] {
			t.Errorf("task %d (%s) status = %s, want %s", i, id, got, want[i])
		}
	}
}

// taskEvents returns the implementation.tasks rows of the plan, oldest first.
func taskEvents(t *testing.T, pool *pgxpool.Pool, parent uuid.UUID) []outboxRow {
	t.Helper()
	var out []outboxRow
	for _, r := range rowsForAggregate(t, pool, parent) {
		if r.Topic == bus.TopicImplementationTasks {
			out = append(out, r)
		}
	}
	return out
}

func eventKeys(rows []outboxRow) []string {
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	return keys
}

// complete delivers one implementation.complete to the handler, as the
// consumer would. Legacy window and no evidence: success=true is a release
// decision, success=false a block decision (decideDependentRelease).
func complete(t *testing.T, pool *pgxpool.Pool, f fixture, taskID uuid.UUID, success bool) error {
	t.Helper()
	value, err := json.Marshal(implementationCompletePayload{
		TaskID:  taskID.String(),
		WorkID:  f.parentWorkID.String(),
		Success: success,
		Reason:  "integration test",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &bus.Message{Topic: bus.TopicImplementationComplete, Key: taskID.String(), Value: value}
	return handleImplementationComplete(context.Background(), pool, workgraph.New(pool), nil, evidence.CompatWindowLegacy, msg)
}

func mustComplete(t *testing.T, pool *pgxpool.Pool, f fixture, taskID uuid.UUID, success bool) {
	t.Helper()
	if err := complete(t, pool, f, taskID, success); err != nil {
		t.Fatalf("handleImplementationComplete(%s, success=%t): %v", taskID, success, err)
	}
}

// TestIntegration_ChainReleasesInOrderAcrossRestart: a 3-task plan A -> B -> C
// is committed with only A enqueued; each completion releases exactly the
// next task; and the middle of the run happens in a "new process" that only
// has the database to go on.
func TestIntegration_ChainReleasesInOrderAcrossRestart(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.chainPlan(t, 3)
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	// The decomposition committed the plan rows and edges, and enqueued A only.
	assertStatuses(t, pool, ids, planTaskReleased, planTaskHeld, planTaskHeld)
	var edges int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM plan_task_dependencies d JOIN plan_tasks t ON t.id = d.task_id WHERE t.parent_work_id = $1`,
		f.parentWorkID).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 2 {
		t.Errorf("dependency edges = %d, want 2 (B->A, C->B)", edges)
	}
	if got := eventKeys(taskEvents(t, pool, f.parentWorkID)); len(got) != 1 || got[0] != ids[0].String() {
		t.Fatalf("events after decomposition = %v, want only A (%s)", got, ids[0])
	}

	// Process 1: A completes and releases B, in one transaction.
	mustComplete(t, pool, f, ids[0], true)
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskReleased, planTaskHeld)
	evs := taskEvents(t, pool, f.parentWorkID)
	if len(evs) != 2 || evs[1].Key != ids[1].String() {
		t.Fatalf("events after A = %v, want [A B]", eventKeys(evs))
	}
	if evs[1].ID != releaseEventID(ids[1]) {
		t.Errorf("B's release event id = %s, want the deterministic %s", evs[1].ID, releaseEventID(ids[1]))
	}
	var released implementationTaskMessage
	if err := json.Unmarshal(evs[1].Payload, &released); err != nil {
		t.Fatal(err)
	}
	if released.TaskID != ids[1].String() || len(released.Dependencies) != 1 || released.Dependencies[0] != ids[0].String() {
		t.Errorf("B released with task=%s dependencies=%v, want the stored message (depends on A)", released.TaskID, released.Dependencies)
	}

	// Process 1 dies. Process 2 boots: startup reconstruction finds C held
	// behind B (released, not completed) and must leave it alone.
	n, err := releaseReadyOnStartup(context.Background(), pool)
	if err != nil {
		t.Fatalf("releaseReadyOnStartup: %v", err)
	}
	if n != 0 {
		t.Errorf("startup released %d task(s) with B still running, want 0", n)
	}
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskReleased, planTaskHeld)

	// Process 2 handles B's completion with nothing but the table to go on.
	mustComplete(t, pool, f, ids[1], true)
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskCompleted, planTaskReleased)
	mustComplete(t, pool, f, ids[2], true)
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskCompleted, planTaskCompleted)

	evs = taskEvents(t, pool, f.parentWorkID)
	want := []string{ids[0].String(), ids[1].String(), ids[2].String()}
	if got := eventKeys(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}

	// And the dispatcher delivers them in plan order.
	fb := &fakeBus{}
	d := outbox.NewDispatcher(outbox.NewStore(pool), fb, outbox.Options{ClaimedBy: "m106-chain"})
	for i := 0; i < len(want); i++ {
		if _, err := d.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}
	var sent []string
	for _, m := range fb.messages() {
		if m.Topic == bus.TopicImplementationTasks {
			sent = append(sent, m.Key)
		}
	}
	if fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Errorf("published %v, want %v", sent, want)
	}
}

// TestIntegration_StartupReleasesReadyHeldTasks: a held task whose
// dependencies are all completed in the table — however it got there — is
// released by a new process on boot, and only once.
func TestIntegration_StartupReleasesReadyHeldTasks(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.chainPlan(t, 3)
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}
	// State left behind by something other than this process's completion
	// path (an operator repair, an older planner build): A completed, B
	// still held.
	if _, err := pool.Exec(context.Background(),
		`UPDATE plan_tasks SET status = 'completed', completed_at = now() WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}

	n, err := releaseReadyOnStartup(context.Background(), pool)
	if err != nil {
		t.Fatalf("releaseReadyOnStartup: %v", err)
	}
	if n < 1 {
		t.Fatalf("startup released %d tasks, want B", n)
	}
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskReleased, planTaskHeld)
	evs := taskEvents(t, pool, f.parentWorkID)
	if len(evs) != 2 || evs[1].ID != releaseEventID(ids[1]) {
		t.Fatalf("events = %v, want A and B's deterministic release", eventKeys(evs))
	}

	// A second boot (or a second replica) releases nothing more.
	if _, err := releaseReadyOnStartup(context.Background(), pool); err != nil {
		t.Fatalf("releaseReadyOnStartup again: %v", err)
	}
	if got := taskEvents(t, pool, f.parentWorkID); len(got) != 2 {
		t.Errorf("events after a second startup = %v, want still 2", eventKeys(got))
	}
}

// TestIntegration_FailedTaskNeverReleasesDependents is D1: a block decision
// marks A failed, blocks the parent with the reason, and B stays held — on
// the completion path, on startup, and after a late success for A.
func TestIntegration_FailedTaskNeverReleasesDependents(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.chainPlan(t, 2)
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	mustComplete(t, pool, f, ids[0], false)
	assertStatuses(t, pool, ids, planTaskFailed, planTaskHeld)

	parent, err := wg.GetWorkItem(context.Background(), f.parentWorkID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.State != "blocked" {
		t.Errorf("parent state = %s, want blocked", parent.State)
	}
	var blocked []outboxRow
	for _, r := range rowsForAggregate(t, pool, f.parentWorkID) {
		if r.Topic == bus.TopicWorkBlocked {
			blocked = append(blocked, r)
		}
	}
	if len(blocked) != 1 {
		t.Fatalf("work.blocked events = %d, want 1 committed with the failure", len(blocked))
	}
	var bp struct {
		WorkID string `json:"work_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(blocked[0].Payload, &bp); err != nil {
		t.Fatal(err)
	}
	if bp.WorkID != f.parentWorkID.String() || bp.Reason == "" {
		t.Errorf("work.blocked payload = %+v, want the parent and a reason", bp)
	}

	if _, err := releaseReadyOnStartup(context.Background(), pool); err != nil {
		t.Fatalf("releaseReadyOnStartup: %v", err)
	}
	// A late (or replayed) success for the failed task must not revive it.
	mustComplete(t, pool, f, ids[0], true)
	// A repeated failure is a no-op too.
	mustComplete(t, pool, f, ids[0], false)

	assertStatuses(t, pool, ids, planTaskFailed, planTaskHeld)
	if got := taskEvents(t, pool, f.parentWorkID); len(got) != 1 || got[0].Key != ids[0].String() {
		t.Errorf("implementation.tasks events = %v, want only A's", eventKeys(got))
	}
}

// TestIntegration_DoubleCompletionIsANoOp: a duplicated implementation.complete
// changes nothing the first one did not, and a direct second enqueue of the
// same release is swallowed by the outbox.
func TestIntegration_DoubleCompletionIsANoOp(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.chainPlan(t, 3)
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	mustComplete(t, pool, f, ids[0], true)
	var releasedAt time.Time
	if err := pool.QueryRow(context.Background(), `SELECT released_at FROM plan_tasks WHERE id = $1`, ids[1]).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}

	mustComplete(t, pool, f, ids[0], true)
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskReleased, planTaskHeld)
	if got := taskEvents(t, pool, f.parentWorkID); len(got) != 2 {
		t.Errorf("events after a duplicate completion = %v, want [A B]", eventKeys(got))
	}
	var again time.Time
	if err := pool.QueryRow(context.Background(), `SELECT released_at FROM plan_tasks WHERE id = $1`, ids[1]).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if !again.Equal(releasedAt) {
		t.Errorf("released_at moved from %s to %s on a duplicate completion", releasedAt, again)
	}

	// Even if the status guard were bypassed, the deterministic id makes a
	// second release event a no-op insert.
	var stored []byte
	if err := pool.QueryRow(context.Background(), `SELECT message FROM plan_tasks WHERE id = $1`, ids[1]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	ev, err := releaseEvent(f.parentWorkID, ids[1], stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := workgraph.WithTx(context.Background(), pool, func(tx pgx.Tx) error {
		return outbox.Enqueue(context.Background(), tx, ev)
	}); err != nil {
		t.Fatalf("re-enqueue release: %v", err)
	}
	if got := taskEvents(t, pool, f.parentWorkID); len(got) != 2 {
		t.Errorf("events after re-enqueueing B's release = %v, want still 2", eventKeys(got))
	}
}

// TestIntegration_ConcurrentCompletionsReleaseTheJoinOnce: C depends on both
// A and B; A and B complete at the same moment. The plan lock serialises the
// two transactions, so exactly one of them sees both completed and releases
// C — neither misses it, and it is not released twice.
//
// planTxHook makes the overlap certain rather than likely: neither
// transaction commits until the other has begun (it is inside its
// transaction, about to take the plan lock), and each waits — up to a grace
// period — for the other to reach its own pre-commit point. Without the
// FOR UPDATE both reach it, both have run the release query against a
// snapshot missing the other's completion, and C is never released; with the
// lock the second is blocked, the first times out of the grace wait and
// commits, and the second then sees both completions.
func TestIntegration_ConcurrentCompletionsReleaseTheJoinOnce(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.multiGroupPlan(3)
	plan[0].titles = []string{"A"}
	plan[1].titles = []string{"B"}
	plan[2].titles = []string{"C"}
	plan[2].dependsOnTitles = []string{"A", "B"}
	if _, err := applyPlanDependencies(plan, titleSteps("A", "B", "C")); err != nil {
		t.Fatal(err)
	}
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}
	assertStatuses(t, pool, ids, planTaskReleased, planTaskReleased, planTaskHeld)

	began := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	atCommit := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var once [2][2]sync.Once
	index := func(id uuid.UUID) int {
		if id == ids[0] {
			return 0
		}
		return 1
	}
	planTxHook = func(stage string, id uuid.UUID) {
		i := index(id)
		switch stage {
		case "begin":
			once[i][0].Do(func() { close(began[i]) })
		case "commit":
			once[i][1].Do(func() { close(atCommit[i]) })
			select {
			case <-began[1-i]:
			case <-time.After(10 * time.Second):
				t.Errorf("transaction %d never began", 1-i)
			}
			select {
			case <-atCommit[1-i]:
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	t.Cleanup(func() { planTxHook = nil })

	var wgrp sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wgrp.Add(1)
		go func(i int) {
			defer wgrp.Done()
			_, errs[i] = completePlanTask(context.Background(), pool, ids[i])
		}(i)
	}
	wgrp.Wait()
	planTxHook = nil
	for i, err := range errs {
		if err != nil {
			t.Fatalf("completePlanTask %d: %v", i, err)
		}
	}
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskCompleted, planTaskReleased)
	if got := taskEvents(t, pool, f.parentWorkID); len(got) != 3 {
		t.Errorf("events = %v, want A, B and one release of C", eventKeys(got))
	}
}

// TestIntegration_FailedReleaseRollsBackTheCompletion: the completion and the
// release share one transaction, so an outbox failure leaves A released (not
// completed) and B held, and the redelivered message does the whole thing.
func TestIntegration_FailedReleaseRollsBackTheCompletion(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	wg := workgraph.New(pool)

	plan, ids := f.chainPlan(t, 2)
	if err := commitDecomposition(context.Background(), pool, wg, f.parentWorkID, nil, plan); err != nil {
		t.Fatalf("commitDecomposition: %v", err)
	}

	original := outboxEnqueueAll
	outboxEnqueueAll = func(context.Context, pgx.Tx, ...outbox.Event) error { return errors.New("simulated outbox failure") }
	if err := complete(t, pool, f, ids[0], true); err == nil {
		t.Error("handler returned nil despite a failed release; the offset would advance past it")
	}
	outboxEnqueueAll = original
	assertStatuses(t, pool, ids, planTaskReleased, planTaskHeld)

	mustComplete(t, pool, f, ids[0], true)
	assertStatuses(t, pool, ids, planTaskCompleted, planTaskReleased)
}
