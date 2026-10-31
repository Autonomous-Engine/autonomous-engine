package main

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRoundStore keeps rounds in memory with the semantics of pgRoundStore:
// Reusable returns the latest ok non-follow-up result per type for the item
// and hash; RecordResult applies a result under the store lock.
type fakeRoundStore struct {
	mu     sync.Mutex
	rounds []*researchRound
	seq    int
	order  map[string]int // task id -> completion sequence
}

func newFakeRoundStore() *fakeRoundStore { return &fakeRoundStore{order: map[string]int{}} }

func (f *fakeRoundStore) insert(r *researchRound) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.rounds {
		if o.WorkID == r.WorkID && (o.Status == roundOpen || o.Status == roundSynthesizing) {
			o.Status = roundSuperseded
		}
	}
	if r.Status == "" {
		r.Status = roundOpen
	}
	for i := range r.Tasks {
		if r.Tasks[i].Status == "" {
			r.Tasks[i].Status = taskPending
		}
		if r.Tasks[i].Status != taskPending {
			f.seq++
			f.order[r.Tasks[i].TaskID] = f.seq
		}
	}
	f.rounds = append(f.rounds, r)
}

func (f *fakeRoundStore) Reusable(_ context.Context, workID uuid.UUID, hash string) (map[string]roundTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]roundTask{}
	best := map[string]int{}
	for _, r := range f.rounds {
		if r.WorkID != workID || r.PRDHash != hash {
			continue
		}
		for _, t := range r.Tasks {
			if t.Status != ResearchStatusOK || t.Result == nil || t.Followup {
				continue
			}
			if seq := f.order[t.TaskID]; seq >= best[t.ResearchType] {
				best[t.ResearchType] = seq
				out[t.ResearchType] = t
			}
		}
	}
	return out, nil
}

func (f *fakeRoundStore) RecordResult(_ context.Context, res researchResultMessage, now time.Time, budget time.Duration) (resultDecision, *researchRound, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rounds {
		if r.task(res.TaskID) != nil {
			d := r.applyResult(res, now, budget)
			f.seq++
			f.order[res.TaskID] = f.seq
			return d, r, nil
		}
	}
	return resultDecision{}, nil, nil
}

func (f *fakeRoundStore) Close(_ context.Context, id uuid.UUID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rounds {
		if r.ID == id && (r.Status == roundOpen || r.Status == roundSynthesizing) {
			r.Status = status
		}
	}
	return nil
}

// dispatchRound plans and "commits" a round the way handlePRDReady does,
// returning it and the research types actually dispatched.
func dispatchRound(t *testing.T, store *fakeRoundStore, workID uuid.UUID, context_ string, selected []string, now time.Time) (*researchRound, []string) {
	t.Helper()
	hash := prdHash(context_)
	plan, err := prepareRound(context.Background(), store, workID, hash, selected)
	if err != nil {
		t.Fatalf("prepareRound: %v", err)
	}
	r := &researchRound{ID: uuid.New(), WorkID: workID, PRDHash: hash, PRDContent: context_, CreatedAt: now}
	for _, typ := range plan.Dispatch {
		r.Tasks = append(r.Tasks, roundTask{TaskID: uuid.NewString(), ResearchType: typ, Status: taskPending})
	}
	r.Tasks = append(r.Tasks, plan.Reused...)
	store.insert(r)
	return r, plan.Dispatch
}

func taskOf(r *researchRound, typ string) string {
	for _, t := range r.Tasks {
		if t.ResearchType == typ {
			return t.TaskID
		}
	}
	return ""
}

func result(r *researchRound, typ, status string) researchResultMessage {
	return researchResultMessage{TaskID: taskOf(r, typ), WorkID: r.WorkID.String(), Status: status, FindingType: typ, Evidence: typ + " evidence"}
}

// R2 deliverable 3: 2 of 3 completed -> the retry dispatches exactly 1.
func TestRetryReusesCompletedResearch(t *testing.T) {
	store := newFakeRoundStore()
	ctx := context.Background()
	workID := uuid.New()
	now := time.Now()
	types := []string{"code_trace", "bug_archaeology", "impact_assessment"}

	first, dispatched := dispatchRound(t, store, workID, "the PRD", types, now)
	if !reflect.DeepEqual(dispatched, types) {
		t.Fatalf("first round dispatched %v, want %v", dispatched, types)
	}
	store.RecordResult(ctx, result(first, "code_trace", ResearchStatusOK), now, defaultTimeBudget)
	store.RecordResult(ctx, result(first, "bug_archaeology", ResearchStatusError), now, defaultTimeBudget)
	d, _, _ := store.RecordResult(ctx, result(first, "impact_assessment", ResearchStatusOK), now, defaultTimeBudget)
	if !d.Claimed {
		t.Fatal("the last result did not complete the first round")
	}
	store.Close(ctx, first.ID, roundBlocked) //nolint:errcheck

	second, dispatched := dispatchRound(t, store, workID, "the PRD", types, now)
	if !reflect.DeepEqual(dispatched, []string{"bug_archaeology"}) {
		t.Fatalf("retry dispatched %v, want exactly [bug_archaeology]", dispatched)
	}
	if len(second.Tasks) != 3 || second.pending() != 1 {
		t.Fatalf("retry round has %d tasks, %d pending; want 3 with 1 pending", len(second.Tasks), second.pending())
	}
	for _, typ := range []string{"code_trace", "impact_assessment"} {
		var got *roundTask
		for i := range second.Tasks {
			if second.Tasks[i].ResearchType == typ {
				got = &second.Tasks[i]
			}
		}
		if got == nil || got.Status != ResearchStatusOK || got.ReusedFrom != taskOf(first, typ) || got.Result == nil || got.Result.Evidence != typ+" evidence" {
			t.Fatalf("%s not reused from the first round: %+v", typ, got)
		}
	}

	// The one dispatched task converges the retry round, and synthesis sees
	// all three results.
	d, r, _ := store.RecordResult(ctx, result(second, "bug_archaeology", ResearchStatusOK), now, defaultTimeBudget)
	if !d.Claimed || !d.Converged {
		t.Fatalf("decision %+v, want claimed and converged", d)
	}
	var got []string
	for _, res := range r.results() {
		got = append(got, res.FindingType)
	}
	sort.Strings(got)
	if want := []string{"bug_archaeology", "code_trace", "impact_assessment"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("synthesis results = %v, want %v", got, want)
	}

	// A third round reuses the reused result's origin, not the copy.
	store.Close(ctx, second.ID, roundBlocked) //nolint:errcheck
	third, dispatched := dispatchRound(t, store, workID, "the PRD", types, now)
	if len(dispatched) != 0 {
		t.Fatalf("third round dispatched %v, want nothing", dispatched)
	}
	for _, tk := range third.Tasks {
		if tk.ResearchType == "code_trace" && tk.ReusedFrom != taskOf(first, "code_trace") {
			t.Fatalf("code_trace reused from %s, want the original %s", tk.ReusedFrom, taskOf(first, "code_trace"))
		}
	}
}

func TestRetryWithChangedPRDReusesNothing(t *testing.T) {
	store := newFakeRoundStore()
	ctx := context.Background()
	workID := uuid.New()
	now := time.Now()
	types := []string{"code_trace", "impact_assessment"}
	first, _ := dispatchRound(t, store, workID, "PRD v1", types, now)
	store.RecordResult(ctx, result(first, "code_trace", ResearchStatusOK), now, defaultTimeBudget)
	store.RecordResult(ctx, result(first, "impact_assessment", ResearchStatusLimitHit), now, defaultTimeBudget)

	_, dispatched := dispatchRound(t, store, workID, "PRD v2", types, now)
	if !reflect.DeepEqual(dispatched, types) {
		t.Fatalf("changed PRD dispatched %v, want all of %v", dispatched, types)
	}
	// Another work item never borrows results.
	_, dispatched = dispatchRound(t, store, uuid.New(), "PRD v1", types, now)
	if !reflect.DeepEqual(dispatched, types) {
		t.Fatalf("other work item dispatched %v, want all of %v", dispatched, types)
	}
}

func TestPlanRoundIgnoresFollowupsAndFailures(t *testing.T) {
	store := newFakeRoundStore()
	workID := uuid.New()
	ok := &researchResultMessage{Status: ResearchStatusOK}
	store.insert(&researchRound{ID: uuid.New(), WorkID: workID, PRDHash: prdHash("p"), Status: roundBlocked, Tasks: []roundTask{
		{TaskID: "a", ResearchType: "code_trace", Status: ResearchStatusOK, Result: ok, Followup: true},
		{TaskID: "b", ResearchType: "pattern_match", Status: ResearchStatusTimedOut, Result: &researchResultMessage{Status: ResearchStatusTimedOut}},
	}})
	plan, _ := prepareRound(context.Background(), store, workID, prdHash("p"), []string{"code_trace", "pattern_match"})
	if len(plan.Reused) != 0 || !reflect.DeepEqual(plan.Dispatch, []string{"code_trace", "pattern_match"}) {
		t.Fatalf("plan = %+v, want everything dispatched", plan)
	}
}

func TestPRDHashNormalisesCRLF(t *testing.T) {
	if prdHash("a\r\nb") != prdHash("a\nb") {
		t.Fatal("CRLF changes the PRD hash")
	}
	if prdHash("a") == prdHash("b") {
		t.Fatal("different PRDs share a hash")
	}
}

// B2: after a restart the coordinator has no in-memory session. Round
// completion must still be judged against the persisted dispatched set: one
// result out of three does not converge, and nothing is synthesized until
// the third.
func TestRestartJudgesConvergenceAgainstPersistedRound(t *testing.T) {
	store := newFakeRoundStore()
	ctx := context.Background()
	workID := uuid.New()
	now := time.Now()
	round, _ := dispatchRound(t, store, workID, "the PRD", []string{"code_trace", "bug_archaeology", "impact_assessment"}, now)

	// "Restart": a fresh coordinator knows nothing about the work item.
	coord := newCoordinator()
	if len(coord.sessions) != 0 {
		t.Fatal("fresh coordinator has sessions")
	}

	d, r, err := store.RecordResult(ctx, result(round, "code_trace", ResearchStatusOK), now, defaultTimeBudget)
	if err != nil || !d.Known {
		t.Fatalf("first result not matched to its persisted round: %+v %v", d, err)
	}
	if d.Converged || d.Claimed || d.Pending != 2 || d.Total != 3 {
		t.Fatalf("decision after 1 of 3 = %+v, want 2 pending of 3 and no claim", d)
	}
	if r.PRDContent != "the PRD" {
		t.Fatalf("round PRD = %q: synthesis after a restart must not run on an empty PRD", r.PRDContent)
	}
	d, _, _ = store.RecordResult(ctx, result(round, "bug_archaeology", ResearchStatusOK), now, defaultTimeBudget)
	if d.Claimed {
		t.Fatal("2 of 3 claimed the round")
	}
	d, _, _ = store.RecordResult(ctx, result(round, "impact_assessment", ResearchStatusOK), now, defaultTimeBudget)
	if !d.Claimed || !d.Converged {
		t.Fatalf("3 of 3 = %+v, want claimed", d)
	}
}

func TestResultForUnknownTaskIsNotAConvergedRound(t *testing.T) {
	store := newFakeRoundStore()
	d, r, err := store.RecordResult(context.Background(), researchResultMessage{TaskID: uuid.NewString(), Status: ResearchStatusOK}, time.Now(), defaultTimeBudget)
	if err != nil || d.Known || d.Claimed || r != nil {
		t.Fatalf("unknown task: decision %+v round %v err %v, want unknown", d, r, err)
	}
}

func TestRoundClaimIsExclusiveButARedeliveredClaimantMayReclaim(t *testing.T) {
	store := newFakeRoundStore()
	ctx := context.Background()
	now := time.Now()
	round, _ := dispatchRound(t, store, uuid.New(), "p", []string{"code_trace", "impact_assessment"}, now)
	store.RecordResult(ctx, result(round, "code_trace", ResearchStatusOK), now, defaultTimeBudget)
	last := result(round, "impact_assessment", ResearchStatusOK)
	if d, _, _ := store.RecordResult(ctx, last, now, defaultTimeBudget); !d.Claimed {
		t.Fatal("last result did not claim")
	}
	// A redelivery of the claimant (crash before the brief committed) may
	// claim again; any other result may not.
	if d, _, _ := store.RecordResult(ctx, last, now, defaultTimeBudget); !d.Claimed {
		t.Fatal("redelivered claimant could not reclaim")
	}
	if d, _, _ := store.RecordResult(ctx, result(round, "code_trace", ResearchStatusOK), now, defaultTimeBudget); d.Claimed {
		t.Fatal("a second result claimed a round already in synthesis")
	}
	store.Close(ctx, round.ID, roundCompleted) //nolint:errcheck
	if d, _, _ := store.RecordResult(ctx, last, now, defaultTimeBudget); d.Claimed {
		t.Fatal("a closed round was claimed again")
	}
}

func TestRoundTimeBudgetClaimsWithPendingTasks(t *testing.T) {
	store := newFakeRoundStore()
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	round, _ := dispatchRound(t, store, uuid.New(), "p", []string{"code_trace", "impact_assessment"}, start)
	d, r, _ := store.RecordResult(ctx, result(round, "code_trace", ResearchStatusOK), time.Now(), defaultTimeBudget)
	if !d.TimeExceeded || !d.Claimed || d.Converged {
		t.Fatalf("decision = %+v, want time exceeded and claimed while not converged", d)
	}
	if n := len(r.results()); n != 1 {
		t.Fatalf("results = %d, want the 1 recorded", n)
	}
	if d, _, _ := store.RecordResult(ctx, result(round, "impact_assessment", ResearchStatusOK), time.Now(), defaultTimeBudget); d.Claimed {
		t.Fatal("a later result claimed the round again after the time budget")
	}
}

func TestSupersededRoundRecordsButDoesNotConverge(t *testing.T) {
	store := newFakeRoundStore()
	ctx := context.Background()
	workID := uuid.New()
	now := time.Now()
	old, _ := dispatchRound(t, store, workID, "p", []string{"code_trace"}, now)
	dispatchRound(t, store, workID, "p", []string{"code_trace"}, now) // supersedes old
	d, _, _ := store.RecordResult(ctx, result(old, "code_trace", ResearchStatusOK), now, defaultTimeBudget)
	if !d.Known || d.Claimed {
		t.Fatalf("late result of a superseded round = %+v, want recorded, not claimed", d)
	}
	// ...and it is reusable by the next round.
	_, dispatched := dispatchRound(t, store, workID, "p", []string{"code_trace"}, now)
	if len(dispatched) != 0 {
		t.Fatalf("dispatched %v, want the late ok result reused", dispatched)
	}
}

func TestBudgetHintTokens(t *testing.T) {
	cases := []struct {
		budget, spent int64
		tasks         int
		want          int64
	}{
		{500_000, 0, 1, 500_000},
		{500_000, 100_000, 2, 200_000},
		{500_000, 499_999, 3, 0},
		{500_000, 600_000, 2, 0},
		{500_000, 0, 0, 0},
	}
	for _, tc := range cases {
		if got := budgetHintTokens(tc.budget, tc.spent, tc.tasks); got != tc.want {
			t.Errorf("budgetHintTokens(%d, %d, %d) = %d, want %d", tc.budget, tc.spent, tc.tasks, got, tc.want)
		}
	}
}
