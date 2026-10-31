package workgraph

import (
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

func tr(from, to models.WorkItemState, reason string, at time.Time) *models.WorkItemTransition {
	return &models.WorkItemTransition{FromState: from, ToState: to, Reason: reason, Timestamp: at}
}

// One round definition for status-orchestrator and pr-coordinator: a round
// starts when the parent re-enters implementation from a later state, or
// when a codex re-plan sends it back to planning.
func TestRoundStart(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	first := []*models.WorkItemTransition{
		tr(models.WorkItemStateResearch, models.WorkItemStatePlanning, "planner", t0),
		tr(models.WorkItemStatePlanning, models.WorkItemStateImplementation, "child tasks running", t0.Add(time.Minute)),
	}
	if got := RoundStart(first); !got.IsZero() {
		t.Fatalf("first round start = %s, want zero", got)
	}
	fix := append(append([]*models.WorkItemTransition{}, first...),
		tr(models.WorkItemStateImplementation, models.WorkItemStateVerification, "", t0.Add(2*time.Minute)),
		tr(models.WorkItemStateVerification, models.WorkItemStateImplementation, "verification requested changes", t0.Add(3*time.Minute)))
	if got := RoundStart(fix); !got.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("fix round start = %s", got)
	}
	replan := append(append([]*models.WorkItemTransition{}, fix...),
		tr(models.WorkItemStateImplementation, models.WorkItemStatePlanning, "codex_changed: modules a (v1 → v2)", t0.Add(4*time.Minute)),
		tr(models.WorkItemStatePlanning, models.WorkItemStatePlanning, "implementation-planner decomposing tasks", t0.Add(5*time.Minute)),
		tr(models.WorkItemStatePlanning, models.WorkItemStateImplementation, "child tasks running", t0.Add(6*time.Minute)))
	if got := RoundStart(replan); !got.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("re-plan round start = %s, want the codex re-plan", got)
	}
	if got := LastCodexReplan(replan); !got.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("LastCodexReplan = %s", got)
	}
	if got := LastCodexReplan(fix); !got.IsZero() {
		t.Fatalf("LastCodexReplan without a re-plan = %s", got)
	}
}

func TestSuperseded(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	ts := []*models.WorkItemTransition{
		tr(models.WorkItemStateImplementation, models.WorkItemStateBlocked, "codex_changed: superseded by the re-plan of the parent (modules a (v1 → v2))", t0),
	}
	if !Superseded(models.WorkItemStateBlocked, ts) {
		t.Error("a subtask blocked by a codex re-plan is superseded")
	}
	if Superseded(models.WorkItemStateImplementation, ts) {
		t.Error("a retried subtask (back in implementation) is not superseded")
	}
	ts = append(ts, tr(models.WorkItemStateBlocked, models.WorkItemStateImplementation, "retry", t0.Add(time.Minute)),
		tr(models.WorkItemStateImplementation, models.WorkItemStateBlocked, "tests failed", t0.Add(2*time.Minute)))
	if Superseded(models.WorkItemStateBlocked, ts) {
		t.Error("a subtask whose latest block is not a codex re-plan is not superseded")
	}
}

// InRound: who takes part in the parent's current round.
func TestInRound(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	r := Round{Start: t0, CodexReplan: t0}
	cases := []struct {
		name string
		c    RoundChild
		want bool
	}{
		{"new plan's subtask", RoundChild{State: models.WorkItemStateImplementation, CreatedAt: t0.Add(time.Minute)}, true},
		{"new plan's passed subtask", RoundChild{State: models.WorkItemStatePassed, CreatedAt: t0.Add(time.Minute), PassedAt: t0.Add(2 * time.Minute)}, true},
		{"old plan's passed subtask", RoundChild{State: models.WorkItemStatePassed, CreatedAt: t0.Add(-time.Hour), PassedAt: t0.Add(-time.Minute)}, false},
		{"old plan's superseded subtask", RoundChild{State: models.WorkItemStateBlocked, CreatedAt: t0.Add(-time.Hour), Superseded: true}, false},
		{"old plan's subtask still running", RoundChild{State: models.WorkItemStateImplementation, CreatedAt: t0.Add(-time.Hour)}, false},
	}
	for _, tc := range cases {
		if got := r.InRound(tc.c); got != tc.want {
			t.Errorf("%s: InRound = %t, want %t", tc.name, got, tc.want)
		}
	}
	// Without a codex re-plan, a fix round keeps the earlier subtasks that
	// are still running.
	fix := Round{Start: t0}
	if !fix.InRound(RoundChild{State: models.WorkItemStateImplementation, CreatedAt: t0.Add(-time.Hour)}) {
		t.Error("a fix round drops a running subtask created before it")
	}
}
