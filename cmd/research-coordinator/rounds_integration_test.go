package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// Env-gated (TEST_POSTGRES_DSN / TEST_DATABASE_URL) checks of pgRoundStore
// against migration 058: the round commits with the dispatch, convergence is
// judged from the persisted set, and a retry with the same PRD hash
// dispatches only the type that failed.

func roundFor(workID uuid.UUID, hash string, tasks []researchTaskMessage, reused []roundTask) *researchRound {
	r := &researchRound{ID: uuid.New(), WorkID: workID, PRDHash: hash, WorkType: "bug", PRDContent: "the PRD", AggregateID: workID, Status: roundOpen, CreatedAt: time.Now()}
	for _, t := range tasks {
		r.Tasks = append(r.Tasks, roundTask{TaskID: t.TaskID, ResearchType: t.ResearchType, Status: taskPending})
	}
	r.Tasks = append(r.Tasks, reused...)
	return r
}

func TestIntegration_RoundPersistedJudgedAndReusedOnRetry(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	ctx := context.Background()
	store := &pgRoundStore{pool: pool}
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)
	hash := prdHash("the PRD")

	types := []string{"code_trace", "bug_archaeology", "impact_assessment"}
	var tasks []researchTaskMessage
	for _, typ := range types {
		tasks = append(tasks, researchTaskMessage{TaskID: uuid.NewString(), WorkID: workID.String(), ProductID: productID.String(), ResearchType: typ})
	}
	first := roundFor(workID, hash, tasks, nil)
	if err := commitResearchDispatch(ctx, pool, wg, workID, workID, tasks, first); err != nil {
		t.Fatalf("commitResearchDispatch: %v", err)
	}

	res := func(i int, status string) researchResultMessage {
		return researchResultMessage{TaskID: tasks[i].TaskID, WorkID: workID.String(), Status: status, FindingType: tasks[i].ResearchType, Evidence: "e"}
	}
	// A fresh store (as after a restart) judges against the persisted set.
	d, r, err := (&pgRoundStore{pool: pool}).RecordResult(ctx, res(0, ResearchStatusOK), time.Now(), defaultTimeBudget)
	if err != nil || !d.Known || d.Claimed || d.Pending != 2 || d.Total != 3 {
		t.Fatalf("1 of 3: decision %+v err %v, want known, 2 pending of 3, unclaimed", d, err)
	}
	if r.PRDContent != "the PRD" || r.AggregateID != workID {
		t.Fatalf("round not rehydrated: prd=%q aggregate=%s", r.PRDContent, r.AggregateID)
	}
	if d, _, _ = store.RecordResult(ctx, res(1, ResearchStatusError), time.Now(), defaultTimeBudget); d.Claimed {
		t.Fatal("2 of 3 claimed")
	}
	if d, _, _ = store.RecordResult(ctx, res(2, ResearchStatusOK), time.Now(), defaultTimeBudget); !d.Claimed || !d.Converged {
		t.Fatalf("3 of 3: %+v, want claimed", d)
	}
	if d, _, _ = store.RecordResult(ctx, res(2, ResearchStatusOK), time.Now(), defaultTimeBudget); !d.Claimed {
		t.Fatal("redelivered claimant could not reclaim")
	}
	if d, _, _ = store.RecordResult(ctx, res(0, ResearchStatusOK), time.Now(), defaultTimeBudget); d.Claimed {
		t.Fatal("non-claimant claimed a round in synthesis")
	}
	if err := store.Close(ctx, first.ID, roundBlocked); err != nil {
		t.Fatalf("Close: %v", err)
	}

	plan, err := prepareRound(ctx, store, workID, hash, types)
	if err != nil {
		t.Fatalf("prepareRound: %v", err)
	}
	if !reflect.DeepEqual(plan.Dispatch, []string{"bug_archaeology"}) || len(plan.Reused) != 2 {
		t.Fatalf("retry plan = dispatch %v, %d reused; want [bug_archaeology] and 2", plan.Dispatch, len(plan.Reused))
	}
	retryTasks := []researchTaskMessage{{TaskID: uuid.NewString(), WorkID: workID.String(), ResearchType: "bug_archaeology"}}
	second := roundFor(workID, hash, retryTasks, plan.Reused)
	if err := commitResearchDispatch(ctx, pool, wg, workID, workID, retryTasks, second); err != nil {
		t.Fatalf("retry dispatch: %v", err)
	}
	if n := len(rowsFor(t, pool, workID)); n != 4 {
		t.Fatalf("outbox rows = %d, want 3 + 1", n)
	}
	d, r, err = store.RecordResult(ctx, researchResultMessage{TaskID: retryTasks[0].TaskID, WorkID: workID.String(), Status: ResearchStatusOK, FindingType: "bug_archaeology"}, time.Now(), defaultTimeBudget)
	if err != nil || !d.Claimed || len(r.results()) != 3 {
		t.Fatalf("retry round: decision %+v, %d results, err %v; want claimed with 3", d, len(r.results()), err)
	}

	// A different PRD reuses nothing.
	if plan, _ := prepareRound(ctx, store, workID, prdHash("another PRD"), types); len(plan.Reused) != 0 {
		t.Fatalf("changed PRD reused %d results", len(plan.Reused))
	}

	// A third dispatch supersedes the round still in synthesis.
	third := roundFor(workID, hash, nil, nil)
	if err := commitResearchDispatch(ctx, pool, wg, workID, workID, nil, third); err != nil {
		t.Fatalf("third dispatch: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM research_rounds WHERE id = $1`, second.ID).Scan(&status); err != nil || status != roundSuperseded {
		t.Fatalf("second round status = %q (%v), want superseded", status, err)
	}
}

func TestIntegration_ResultForUnknownTaskIsDropped(t *testing.T) {
	pool := integrationPool(t)
	d, r, err := (&pgRoundStore{pool: pool}).RecordResult(context.Background(), researchResultMessage{TaskID: uuid.NewString(), Status: ResearchStatusOK}, time.Now(), defaultTimeBudget)
	if err != nil || d.Known || r != nil {
		t.Fatalf("unknown task: %+v %v %v", d, r, err)
	}
}
