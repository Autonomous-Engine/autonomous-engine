package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/google/uuid"
)

// M3-06 (c) against a real PostgreSQL: the token budget — counted from
// inference_logs exactly as RunToolLoop counts it — runs out inside the
// repair loop. The attempt's outcome transaction commits failed evidence
// (gate test check not_run "budget exhausted", a failed budget check), the
// escalation prompt listing the gathered failure record, the block, the
// failed implementation.complete and the run's blocked state together;
// the run's events carry the failure records and detail carries the
// repair and budget state the dashboard reads. Needs git and sh (run in
// golang:1.22, not -alpine). Skips without TEST_POSTGRES_DSN.
func TestIntegration_TokenBudgetExhaustionBlocksWithEvidence(t *testing.T) {
	requireSh(t)
	pool := integrationPool(t)
	dbf := newDBFixture(t, pool)
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "1000")
	ctx := context.Background()
	logger := logging.New("implementation-agent-test")

	taskID := uuid.New()
	store := controller.NewStore(pool)
	run := startControllerRun(ctx, store, logger, dbf.workID, taskID)
	if run == nil {
		t.Fatal("run not started")
	}
	model := &queueAdapter{tokens: 200, steps: []step{
		readStep(), writeStep("alpha"), textStep("Implemented."),
		readStep(), writeStep("beta"), readStep(), writeStep("gamma"), readStep(), writeStep("delta"),
	}}
	h := newRepairHarness(t, model, nil, run)
	h.product = dbf.productID
	h.evidenceStore = evidence.NewStore(pool)
	sink := dbf.sink(pool, taskID.String())
	sink.run = outcomeHookFor(run, logger)

	gen := h.attempt(t, pool, sink)
	if gen.stopped == nil || gen.stopped.budget == nil || gen.stopped.budget.kind != budgetKindTokens {
		t.Fatalf("stopped = %+v, want a token budget stop", gen.stopped)
	}

	// Token accounting came from inference_logs: 5 calls of 200 tokens.
	var logged int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`, dbf.workID).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != 1000 {
		t.Fatalf("inference_logs tokens = %d, want 1000 (the loop stopped before a sixth call)", logged)
	}

	// Evidence: failed, gate test check not_run "budget exhausted".
	var outcome string
	var evJSON []byte
	if err := pool.QueryRow(ctx, `SELECT outcome, evidence FROM implementation_verifications WHERE work_item_id = $1`, dbf.workID).Scan(&outcome, &evJSON); err != nil {
		t.Fatalf("evidence row: %v", err)
	}
	ev, err := evidence.Parse(evJSON)
	if err != nil || outcome != "failed" {
		t.Fatalf("evidence outcome=%s err=%v", outcome, err)
	}
	if c := ev.Checks[0]; c.Status != evidence.CheckNotRun || c.SkipReason != budgetSkipReason {
		t.Fatalf("gate check = %+v", c)
	}

	// Block, escalation prompt, failed completion, blocked run: one tx.
	state, status := stateAndAssignment(t, pool, dbf)
	if state != "blocked" || status != "failed" {
		t.Fatalf("work item %s / assignment %s, want blocked / failed", state, status)
	}
	var question, trigger string
	if err := pool.QueryRow(ctx, `SELECT question, trigger FROM user_interactions WHERE work_item_id = $1`, dbf.workID).Scan(&question, &trigger); err != nil {
		t.Fatalf("escalation prompt row: %v", err)
	}
	if trigger != string(triggerBudgetExhausted) || !strings.Contains(question, "Top failure records") || !strings.Contains(question, "Test_alpha") {
		t.Fatalf("prompt trigger=%s question:\n%s", trigger, question)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND topic = 'implementation.complete'`, dbf.workID).Scan(&payload); err != nil {
		t.Fatalf("implementation.complete: %v", err)
	}
	var msg implementationCompleteMessage
	_ = json.Unmarshal(payload, &msg)
	if msg.Success || !strings.HasPrefix(msg.Reason, "budget_exhausted: tokens") {
		t.Fatalf("implementation.complete = %+v", msg)
	}
	if s := runState(t, pool, run.ID); s != "blocked" {
		t.Fatalf("run state = %s", s)
	}

	// The check run's event carries the failure records; detail carries
	// the repair and budget state the dashboard shows.
	events, err := store.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recs []failures.Record
	for _, e := range events {
		if e.Tool == preCommitTool && len(e.Detail) > 0 {
			var d struct {
				Records []failures.Record `json:"records"`
			}
			_ = json.Unmarshal(e.Detail, &d)
			recs = append(recs, d.Records...)
		}
	}
	if len(recs) != 1 || recs[0].Test != "Test_alpha" || recs[0].Category != failures.CategoryTestAssert {
		t.Fatalf("recorded failure records = %+v", recs)
	}
	sums, err := store.LatestRuns(ctx, dbf.workID)
	if err != nil || len(sums) != 1 {
		t.Fatalf("LatestRuns = %+v %v", sums, err)
	}
	var rs repairState
	var bs budgetState
	if json.Unmarshal(sums[0].Repair, &rs) != nil || json.Unmarshal(sums[0].Budget, &bs) != nil {
		t.Fatalf("summary repair=%s budget=%s", sums[0].Repair, sums[0].Budget)
	}
	if rs.StopReason != stopBudgetExhausted || len(rs.Failures) != 1 || rs.Failures[0].Count != 1 || bs.Exhausted != budgetKindTokens || bs.TokensUsed != 1000 || bs.TokenLimit != 1000 {
		t.Fatalf("repair %+v budget %+v", rs, bs)
	}
	t.Logf("failure record: %s %s at %s in %s (%s) fp=%s", recs[0].Category, recs[0].Message, recs[0].Location(), recs[0].Test, recs[0].Ownership, failures.Short(recs[0].Fingerprint))
	t.Logf("block reason: %s", msg.Reason)
}
