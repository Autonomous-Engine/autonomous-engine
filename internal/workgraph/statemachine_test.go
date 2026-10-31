package workgraph

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

func TestIsValidTransition_ValidTransitions(t *testing.T) {
	cases := []struct {
		from models.WorkItemState
		to   models.WorkItemState
	}{
		{models.WorkItemStateIntake, models.WorkItemStatePRD},
		{models.WorkItemStatePRD, models.WorkItemStateResearch},
		{models.WorkItemStateResearch, models.WorkItemStatePlanning},
		{models.WorkItemStatePlanning, models.WorkItemStateImplementation},
		{models.WorkItemStateImplementation, models.WorkItemStateReview},
		{models.WorkItemStateReview, models.WorkItemStateHumanReview},
		{models.WorkItemStateReview, models.WorkItemStateImplementation}, // changes requested
		{models.WorkItemStateHumanReview, models.WorkItemStateQA},
		{models.WorkItemStateHumanReview, models.WorkItemStateImplementation}, // changes requested
		{models.WorkItemStateHumanReview, models.WorkItemStateRejected},
		{models.WorkItemStateQA, models.WorkItemStatePassed},
		{models.WorkItemStateQA, models.WorkItemStateFailed},
		{models.WorkItemStateFailed, models.WorkItemStateResearch},
		{models.WorkItemStateBlocked, models.WorkItemStatePRD},
		{models.WorkItemStateBlocked, models.WorkItemStateResearch},
		{models.WorkItemStateBlocked, models.WorkItemStatePlanning},
		{models.WorkItemStatePassed, models.WorkItemStateReleased},
		// R14: verification before the PR.
		{models.WorkItemStateImplementation, models.WorkItemStateVerification},
		{models.WorkItemStateVerification, models.WorkItemStateReview},
		{models.WorkItemStateVerification, models.WorkItemStateHumanReview},
		{models.WorkItemStateVerification, models.WorkItemStateImplementation},
		{models.WorkItemStateVerification, models.WorkItemStateBlocked},
		{models.WorkItemStateVerification, models.WorkItemStateAwaitingUser},
		{models.WorkItemStateAwaitingUser, models.WorkItemStateVerification},
		{models.WorkItemStateBlocked, models.WorkItemStateVerification},
		// Codex coherence Part 1: a codex change sends in-flight work back
		// to planning, and planning can be re-entered.
		{models.WorkItemStateImplementation, models.WorkItemStatePlanning},
		{models.WorkItemStatePlanning, models.WorkItemStatePlanning},
	}

	for _, tc := range cases {
		if !IsValidTransition(tc.from, tc.to) {
			t.Errorf("expected valid transition %s -> %s", tc.from, tc.to)
		}
	}
}

func TestIsValidTransition_InvalidTransitions(t *testing.T) {
	cases := []struct {
		from models.WorkItemState
		to   models.WorkItemState
	}{
		// Can't skip states
		{models.WorkItemStateIntake, models.WorkItemStateImplementation},
		// Can't go backward arbitrarily
		{models.WorkItemStateQA, models.WorkItemStateReview},
		// Released is terminal
		{models.WorkItemStateReleased, models.WorkItemStatePassed},
		// Rejected is terminal
		{models.WorkItemStateRejected, models.WorkItemStateHumanReview},
		// Can't go from passed to failed
		{models.WorkItemStatePassed, models.WorkItemStateFailed},
		// Can't skip from intake to qa
		{models.WorkItemStateIntake, models.WorkItemStateQA},
		// Can't go from planning to review directly
		{models.WorkItemStatePlanning, models.WorkItemStateReview},
		// R14: verification never releases: no qa, passed or released.
		{models.WorkItemStateVerification, models.WorkItemStateQA},
		{models.WorkItemStateVerification, models.WorkItemStatePassed},
		{models.WorkItemStateVerification, models.WorkItemStateReleased},
		{models.WorkItemStatePlanning, models.WorkItemStateVerification},
		// Codex coherence Part 1: verified work is never rolled back to
		// planning by a codex change.
		{models.WorkItemStateVerification, models.WorkItemStatePlanning},
		{models.WorkItemStateReview, models.WorkItemStatePlanning},
		{models.WorkItemStateHumanReview, models.WorkItemStatePlanning},
		{models.WorkItemStateQA, models.WorkItemStatePlanning},
		{models.WorkItemStateImplementation, models.WorkItemStateImplementation},
	}

	for _, tc := range cases {
		if IsValidTransition(tc.from, tc.to) {
			t.Errorf("expected invalid transition %s -> %s", tc.from, tc.to)
		}
	}
}

func TestIsValidTransition_AnyActiveStateCanTransitionToBlocked(t *testing.T) {
	// All active (non-terminal) states that should be blockable
	activeStates := []models.WorkItemState{
		models.WorkItemStateIntake,
		models.WorkItemStatePRD,
		models.WorkItemStateResearch,
		models.WorkItemStatePlanning,
		models.WorkItemStateImplementation,
		models.WorkItemStateReview,
		models.WorkItemStateHumanReview,
		models.WorkItemStateQA,
		models.WorkItemStateFailed,
	}

	for _, state := range activeStates {
		if !IsValidTransition(state, models.WorkItemStateBlocked) {
			t.Errorf("expected state %s to be transitionable to blocked", state)
		}
	}
}

func TestIsValidTransition_AwaitingUser(t *testing.T) {
	// Any active-working state can transition to awaiting_user while the
	// agent yields for user input.
	activeStates := []models.WorkItemState{
		models.WorkItemStateResearch,
		models.WorkItemStatePlanning,
		models.WorkItemStateImplementation,
		models.WorkItemStateReview,
		models.WorkItemStateHumanReview,
		models.WorkItemStateQA,
		models.WorkItemStatePRDApproval,
		models.WorkItemStatePRD,
	}
	for _, s := range activeStates {
		if !IsValidTransition(s, models.WorkItemStateAwaitingUser) {
			t.Errorf("IsValidTransition(%s → awaiting_user) = false, want true", s)
		}
	}

	// awaiting_user → back to each active state (resume after answer).
	for _, s := range activeStates {
		if !IsValidTransition(models.WorkItemStateAwaitingUser, s) {
			t.Errorf("IsValidTransition(awaiting_user → %s) = false, want true", s)
		}
	}

	// awaiting_user → blocked (user escalated stop).
	if !IsValidTransition(models.WorkItemStateAwaitingUser, models.WorkItemStateBlocked) {
		t.Error("awaiting_user → blocked should be allowed")
	}
	// awaiting_user → cancelled (user cancelled).
	if !IsValidTransition(models.WorkItemStateAwaitingUser, models.WorkItemStateCancelled) {
		t.Error("awaiting_user → cancelled should be allowed")
	}
}

func TestIsValidTransition_AwaitingUser_RejectsIllegal(t *testing.T) {
	// Cannot transition to a terminal state like passed/failed/released
	// directly from awaiting_user — must resume to an active state first.
	illegal := []models.WorkItemState{
		models.WorkItemStatePassed,
		models.WorkItemStateFailed,
		models.WorkItemStateReleased,
		models.WorkItemStateRejected,
		models.WorkItemStateIntake,
	}
	for _, s := range illegal {
		if IsValidTransition(models.WorkItemStateAwaitingUser, s) {
			t.Errorf("awaiting_user → %s should NOT be allowed", s)
		}
	}
}
