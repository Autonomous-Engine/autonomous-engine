package main

import (
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// The action for a stale parent: re-plan only in planning or implementation
// without an open PR, within the cap and the budget; everything else past
// planning is only noted.
func TestDecideCodexReplan(t *testing.T) {
	cases := []struct {
		name       string
		state      models.WorkItemState
		openPR     bool
		automatic  int
		max        int
		budgetLeft bool
		want       replanAction
	}{
		{"implementation re-plans", models.WorkItemStateImplementation, false, 0, 2, true, actionReplan},
		{"planning re-plans", models.WorkItemStatePlanning, false, 1, 2, true, actionReplan},
		{"cap reached escalates", models.WorkItemStateImplementation, false, 2, 2, true, actionEscalateCap},
		{"budget gone escalates", models.WorkItemStatePlanning, false, 0, 2, false, actionEscalateBudget},
		{"cap before budget", models.WorkItemStatePlanning, false, 2, 2, false, actionEscalateCap},
		{"open PR is only noted", models.WorkItemStateImplementation, true, 0, 2, true, actionNote},
		{"verification is only noted", models.WorkItemStateVerification, false, 0, 2, true, actionNote},
		{"review is only noted", models.WorkItemStateReview, false, 0, 2, true, actionNote},
		{"human review is only noted", models.WorkItemStateHumanReview, false, 0, 2, true, actionNote},
		{"qa is only noted", models.WorkItemStateQA, false, 0, 2, true, actionNote},
		{"blocked is only noted", models.WorkItemStateBlocked, false, 0, 2, true, actionNote},
		{"awaiting user is only noted", models.WorkItemStateAwaitingUser, false, 0, 2, true, actionNote},
		{"passed is left alone", models.WorkItemStatePassed, false, 0, 2, true, actionNone},
		{"cancelled is left alone", models.WorkItemStateCancelled, false, 0, 2, true, actionNone},
		{"released is left alone", models.WorkItemStateReleased, false, 0, 2, true, actionNone},
		{"cap 0 escalates the first time", models.WorkItemStateImplementation, false, 0, 0, true, actionEscalateCap},
		{"failed is left alone (M5)", models.WorkItemStateFailed, false, 0, 2, true, actionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideCodexReplan(tc.state, tc.openPR, false, tc.automatic, tc.max, tc.budgetLeft); got != tc.want {
				t.Fatalf("decideCodexReplan = %v, want %v", got, tc.want)
			}
		})
	}
	// Review ruling 5: verified work in the round is never re-planned
	// automatically; it escalates before the cap and budget are judged.
	for _, st := range []models.WorkItemState{models.WorkItemStateImplementation, models.WorkItemStatePlanning} {
		if got := decideCodexReplan(st, false, true, 0, 2, true); got != actionEscalateVerified {
			t.Errorf("%s with verified work = %v, want escalate(verified)", st, got)
		}
		if got := decideCodexReplan(st, false, true, 5, 2, false); got != actionEscalateVerified {
			t.Errorf("%s with verified work over cap and budget = %v, want escalate(verified)", st, got)
		}
	}
	if got := decideCodexReplan(models.WorkItemStateImplementation, true, true, 0, 2, true); got != actionNote {
		t.Errorf("open PR with verified work = %v, want note", got)
	}
}

func TestReplanConfigFromEnv(t *testing.T) {
	t.Setenv("CODEX_REPLAN_DEBOUNCE", "")
	t.Setenv("CODEX_REPLAN_MAX", "")
	t.Setenv("WORK_ITEM_TOKEN_BUDGET", "")
	t.Setenv("CODEX_REPLAN_MIN_TOKENS", "")
	c := replanConfigFromEnv()
	if c.Debounce != 10*time.Minute || c.Max != 2 || c.TokenBudget != 500_000 || c.MinPlanTokens != 50_000 {
		t.Fatalf("defaults = %+v", c)
	}
	t.Setenv("CODEX_REPLAN_DEBOUNCE", "90s")
	t.Setenv("CODEX_REPLAN_MAX", "0")
	t.Setenv("CODEX_REPLAN_MIN_TOKENS", "1234")
	c = replanConfigFromEnv()
	if c.Debounce != 90*time.Second || c.Max != 0 || c.MinPlanTokens != 1234 {
		t.Fatalf("overrides = %+v", c)
	}
	t.Setenv("CODEX_REPLAN_DEBOUNCE", "soon")
	t.Setenv("CODEX_REPLAN_MAX", "-1")
	c = replanConfigFromEnv()
	if c.Debounce != 10*time.Minute || c.Max != 2 {
		t.Fatalf("invalid values must fall back to the defaults: %+v", c)
	}
}

// A codex re-plan starts a new verification round: subtasks of the plan it
// replaced that had passed do not take part in the new plan's round.
func TestRoundStartCountsACodexReplan(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	ts := []*models.WorkItemTransition{
		{FromState: models.WorkItemStatePlanning, ToState: models.WorkItemStateImplementation, Reason: childTasksRunningReason, Timestamp: t0},
		{FromState: models.WorkItemStateImplementation, ToState: models.WorkItemStatePlanning, Reason: "codex_changed: modules a (v1 → v2)", Timestamp: t0.Add(time.Hour)},
		{FromState: models.WorkItemStatePlanning, ToState: models.WorkItemStateImplementation, Reason: childTasksRunningReason, Timestamp: t0.Add(2 * time.Hour)},
	}
	if got := roundStart(ts); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("roundStart = %s, want the re-plan at %s", got, t0.Add(time.Hour))
	}
	// A planning re-entry that is not a codex re-plan starts no round.
	ts[1].Reason = "implementation-planner decomposing tasks"
	if got := roundStart(ts); !got.IsZero() {
		t.Fatalf("roundStart = %s, want zero", got)
	}
}
