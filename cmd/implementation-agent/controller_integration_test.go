package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M3-01/02 against a real PostgreSQL: a scripted tool loop over a real git
// worktree persists its transition sequence in implementation_run_events,
// and the outcome transaction commits the terminal state with the outcome.
// Skips without TEST_POSTGRES_DSN / TEST_DATABASE_URL.

func persistedTransitions(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []string {
	t.Helper()
	events, err := controller.NewStore(pool).Events(context.Background(), runID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []string
	for _, e := range events {
		if e.Kind == controller.KindTransition {
			out = append(out, string(e.FromState)+"→"+string(e.ToState))
		}
	}
	return out
}

func runState(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM implementation_runs WHERE id = $1`, runID).Scan(&s); err != nil {
		t.Fatalf("run state: %v", err)
	}
	return s
}

func TestIntegration_Controller_ScriptedRunPersistsTransitionsAndFinalVerified(t *testing.T) {
	pool := integrationPool(t)
	f := newDBFixture(t, pool)
	// No git here (the integration container has none): the diff review
	// is a read_file of the changed file, which counts the same after green.
	repo := newShellDir(t)
	taskID := uuid.New()
	logger := logging.New("implementation-agent-test")

	run := startControllerRun(context.Background(), controller.NewStore(pool), logger, f.workID, taskID)
	if run == nil {
		t.Fatal("run not started")
	}
	run.SetTestCommand(testRunner)
	ex, _ := scriptedExecutor(t, repo, f.workID, taskID, run)
	script := tddScript()
	script[len(script)-1] = call("read_file", map[string]any{"path": "calc.sh"})
	runScriptedWith(t, ex, script, f.workID)

	want := "inspect→test_authored,test_authored→red_observed,red_observed→implement,implement→green_observed,green_observed→diff_reviewed"
	if got := strings.Join(persistedTransitions(t, pool, run.ID), ","); got != want {
		t.Fatalf("persisted transitions:\n got  %s\n want %s", got, want)
	}
	if s := runState(t, pool, run.ID); s != "diff_reviewed" {
		t.Fatalf("run state = %s", s)
	}

	// The verified outcome commits final_verified with implementation.complete.
	sink := f.sink(pool, taskID.String())
	sink.run = outcomeHookFor(run, logger)
	verified := &evidence.ImplementationEvidence{Outcome: evidence.OutcomeVerified}
	if err := sink.Report(completionOutcome{
		msg:        &implementationCompleteMessage{TaskID: taskID.String(), WorkID: f.workID.String(), Success: true, Verified: true, Evidence: verified},
		assignment: models.AssignmentStatusCompleted,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if s := runState(t, pool, run.ID); s != "final_verified" {
		t.Fatalf("run state after verified outcome = %s", s)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.complete'`, f.workID); n != 1 {
		t.Fatalf("implementation.complete rows = %d", n)
	}

	// The dashboard summary reads state and evidence from the rows.
	sums, err := controller.NewStore(pool).LatestRuns(context.Background(), f.workID)
	if err != nil || len(sums) != 1 {
		t.Fatalf("LatestRuns = %+v, %v", sums, err)
	}
	s := sums[0]
	d := run.Detail()
	if s.State != controller.StateFinalVerified || s.Red == nil || s.Red.ToolCallID != d.Red.CallID || s.Green == nil || s.Green.ToolCallID != d.Green.CallID || *s.Green.ExitCode != 0 {
		t.Fatalf("summary = %+v (detail red %+v green %+v)", s, d.Red, d.Green)
	}
}

// An outcome that cannot commit records nothing for the run either; the
// fallback tier that commits records blocked, never final_verified.
func TestIntegration_Controller_FallbackOutcomeRecordsBlocked(t *testing.T) {
	pool := integrationPool(t)
	f := newDBFixture(t, pool)
	taskID := uuid.New()
	logger := logging.New("implementation-agent-test")
	run := startControllerRun(context.Background(), controller.NewStore(pool), logger, f.workID, taskID)

	sink := f.sink(pool, taskID.String())
	sink.run = outcomeHookFor(run, logger)
	// Evidence that fails validation makes the first tier fail; reportOutcome
	// then commits tier 1 (a block).
	bad := &evidence.ImplementationEvidence{Outcome: evidence.OutcomeVerified}
	ok := reportOutcome(sink, completionOutcome{
		evidence: bad, evidenceStore: evidence.NewStore(pool),
		msg:        &implementationCompleteMessage{TaskID: taskID.String(), WorkID: f.workID.String(), Success: true, Evidence: bad},
		assignment: models.AssignmentStatusCompleted,
	})
	if ok {
		t.Fatal("invalid evidence must not commit")
	}
	if s := runState(t, pool, run.ID); s != "blocked" {
		t.Fatalf("run state = %s, want blocked (the committed fallback), never final_verified", s)
	}
	if got := strings.Join(persistedTransitions(t, pool, run.ID), ","); got != "inspect→blocked" {
		t.Fatalf("transitions = %s", got)
	}
}

// The guarded handler's failure path (no repository) ends the run blocked
// in the outcome transaction; replays create no further runs.
func TestIntegration_Controller_HandlerFailurePathRecordsBlockedRun(t *testing.T) {
	pool := integrationPool(t)
	f := newDBFixture(t, pool)
	taskID := seedSubtask(t, pool, f, "implementation")
	msg := taskMessage(t, pool, f, taskID)

	replayAgent(t, guardedAgent(pool), msg, 3)

	var attempt int
	var state string
	var runID uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id, attempt, state FROM implementation_runs WHERE task_id = $1`, taskID.String()).Scan(&runID, &attempt, &state); err != nil {
		t.Fatalf("run row: %v", err)
	}
	if attempt != 1 || state != "blocked" {
		t.Fatalf("run attempt=%d state=%s, want 1 blocked", attempt, state)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM implementation_runs WHERE task_id = $1`, taskID.String()); n != 1 {
		t.Fatalf("runs = %d, want 1 (duplicates are acknowledged before a run starts)", n)
	}
	if got := strings.Join(persistedTransitions(t, pool, runID), ","); got != "inspect→blocked" {
		t.Fatalf("transitions = %s", got)
	}
}
