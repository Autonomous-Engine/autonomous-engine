package main

import (
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/google/uuid"
)

func subtask(state models.WorkItemState) *models.WorkItem {
	return &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeSubtask, State: state}
}

func result(id uuid.UUID, d stage.Decision, at time.Time) stage.Result {
	return stage.Result{TaskID: id, Decision: d, Reason: "r-" + string(d), CreatedAt: at}
}

// V4: the parent leaves verification on the most conservative decision of
// the round, and only once every passed subtask of the round has one.
func TestDecideVerificationRound(t *testing.T) {
	now := time.Now()
	a, b := subtask(models.WorkItemStatePassed), subtask(models.WorkItemStatePassed)
	blocked := subtask(models.WorkItemStateBlocked)
	running := subtask(models.WorkItemStateImplementation)
	res := func(pairs ...any) map[uuid.UUID]stage.Result {
		m := map[uuid.UUID]stage.Result{}
		for i := 0; i < len(pairs); i += 2 {
			r := pairs[i+1].(stage.Result)
			m[pairs[i].(*models.WorkItem).ID] = r
		}
		return m
	}
	legacy := result(a.ID, stage.DecisionNotRun, now)
	legacy.Legacy = true

	cases := []struct {
		name     string
		subs     []*models.WorkItem
		results  map[uuid.UUID]stage.Result
		running  bool
		wait     bool
		empty    bool
		decision stage.Decision
	}{
		{name: "a sibling still running", subs: []*models.WorkItem{a, running}, results: res(a, result(a.ID, stage.DecisionApprove, now)), running: true},
		{name: "a passed subtask unverified", subs: []*models.WorkItem{a, b}, results: res(a, result(a.ID, stage.DecisionApprove, now)), wait: true},
		{name: "all blocked", subs: []*models.WorkItem{blocked}, empty: true},
		{name: "approve", subs: []*models.WorkItem{a, blocked}, results: res(a, result(a.ID, stage.DecisionApprovePendingSecondPass, now)), decision: stage.DecisionApprovePendingSecondPass},
		{name: "escalate wins over approve", subs: []*models.WorkItem{a, b}, results: res(a, result(a.ID, stage.DecisionApprove, now), b, result(b.ID, stage.DecisionEscalate, now)), decision: stage.DecisionEscalate},
		{name: "request changes over approve", subs: []*models.WorkItem{a, b}, results: res(a, result(a.ID, stage.DecisionRequestChanges, now), b, result(b.ID, stage.DecisionApprove, now)), decision: stage.DecisionRequestChanges},
		{name: "escalate over request changes", subs: []*models.WorkItem{a, b}, results: res(a, result(a.ID, stage.DecisionRequestChanges, now), b, result(b.ID, stage.DecisionEscalate, now)), decision: stage.DecisionEscalate},
		{name: "error over everything", subs: []*models.WorkItem{a, b}, results: res(a, result(a.ID, stage.DecisionError, now), b, result(b.ID, stage.DecisionEscalate, now)), decision: stage.DecisionError},
		{name: "not_run on evidence escalates", subs: []*models.WorkItem{a}, results: res(a, result(a.ID, stage.DecisionNotRun, now)), decision: stage.DecisionNotRun},
		{name: "legacy not_run proceeds", subs: []*models.WorkItem{a}, results: res(a, legacy), decision: stage.DecisionApprove},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideVerificationRound(c.subs, nil, c.results, time.Time{})
			if got.Running != c.running || got.Wait != c.wait || got.Empty != c.empty {
				t.Fatalf("running=%t wait=%t empty=%t, want %t %t %t (%+v)", got.Running, got.Wait, got.Empty, c.running, c.wait, c.empty, got)
			}
			if !c.running && !c.wait && !c.empty && got.Decision != c.decision {
				t.Fatalf("decision = %s, want %s (%s)", got.Decision, c.decision, got.Reason)
			}
		})
	}
}

// A new round (the parent went back to implementation) judges only the
// subtasks that passed since, and only results recorded since: an earlier
// round's request_changes does not loop the parent forever.
func TestDecideVerificationRoundIgnoresEarlierRounds(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	start := t0.Add(30 * time.Minute)
	old, fix := subtask(models.WorkItemStatePassed), subtask(models.WorkItemStatePassed)
	passed := map[uuid.UUID]time.Time{old.ID: t0, fix.ID: start.Add(time.Minute)}
	results := map[uuid.UUID]stage.Result{old.ID: result(old.ID, stage.DecisionRequestChanges, t0)}
	got := decideVerificationRound([]*models.WorkItem{old, fix}, passed, results, start)
	if !got.Wait || len(got.Waiting) != 1 || !strings.Contains(got.Waiting[0], fix.ID.String()) {
		t.Fatalf("got %+v, want to wait for the fix subtask only", got)
	}
	results[fix.ID] = result(fix.ID, stage.DecisionApprove, start.Add(2*time.Minute))
	if got := decideVerificationRound([]*models.WorkItem{old, fix}, passed, results, start); got.Wait || got.Decision != stage.DecisionApprove {
		t.Fatalf("got %+v, want approve from the fix subtask alone", got)
	}
	// A stale result for a subtask of this round is not this round's.
	results[fix.ID] = result(fix.ID, stage.DecisionApprove, t0)
	if got := decideVerificationRound([]*models.WorkItem{old, fix}, passed, results, start); !got.Wait {
		t.Fatalf("a result older than the round must not count: %+v", got)
	}
}

// Review minor 5: a target verification cannot reach (passed, qa) becomes
// human_review with a prompt; a reachable one is kept as it is.
func TestReachableFromVerification(t *testing.T) {
	keep := postReviewTarget{ToState: models.WorkItemStateReview, Reason: "all child tasks complete"}
	if got := reachableFromVerification(keep); got != keep {
		t.Errorf("review changed to %+v", got)
	}
	for _, to := range []models.WorkItemState{models.WorkItemStatePassed, models.WorkItemStateQA, models.WorkItemStateReleased} {
		got := reachableFromVerification(postReviewTarget{ToState: to, Reason: "all child tasks complete"})
		if got.ToState != models.WorkItemStateHumanReview || !got.PublishHumanReviewPrompt || !strings.Contains(got.Reason, "not reachable from verification") {
			t.Errorf("%s → %+v, want human_review with a prompt", to, got)
		}
	}
}

func TestRoundStart(t *testing.T) {
	t1 := time.Now().Add(-2 * time.Hour)
	t2 := t1.Add(time.Hour)
	ts := []*models.WorkItemTransition{
		{FromState: models.WorkItemStatePlanning, ToState: models.WorkItemStateImplementation, Timestamp: t1.Add(-time.Hour)},
		{FromState: models.WorkItemStateImplementation, ToState: models.WorkItemStateVerification, Timestamp: t1.Add(-time.Minute)},
		{FromState: models.WorkItemStateVerification, ToState: models.WorkItemStateImplementation, Timestamp: t1},
		{FromState: models.WorkItemStateAwaitingUser, ToState: models.WorkItemStateImplementation, Timestamp: t2.Add(time.Minute)},
		{FromState: models.WorkItemStateReview, ToState: models.WorkItemStateImplementation, Timestamp: t2},
	}
	if got := roundStart(ts); !got.Equal(t2) {
		t.Fatalf("round start = %v, want %v (the last re-entry from a later state; planning and awaiting_user do not start a round)", got, t2)
	}
	if got := roundStart(ts[:2]); !got.IsZero() {
		t.Fatalf("first round start = %v, want zero", got)
	}
}
