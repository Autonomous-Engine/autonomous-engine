package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// A partially verified run in a human_review zone (product opted in) keeps
// the partial marker — the gate the pr-coordinator also labels partial
// first — and its transition reason names the zone, as the PR does.
func TestPartialZoneReviewMarkerAndReason(t *testing.T) {
	p := implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true,
		Evidence: evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
			ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindLint, Command: "lint", Status: evidence.CheckSkipped, SkipReason: "no linter"})
			ev.Outcome = evidence.OutcomePartiallyVerified
			ev.Flags = []string{evidence.FlagZoneReviewRequired}
			ev.Zones = []evidence.ZoneTouch{{Name: "payments", Policy: evidence.ZonePolicyHumanReview}}
		})}
	state, reason := decideSubtaskTransition(p, optedIn(), evidence.CompatWindowStrict)
	if state != models.WorkItemStateHumanReview || !strings.Contains(reason, "zone payments") {
		t.Fatalf("partial in zone = %s %q", state, reason)
	}
	if got := humanReviewMarkerFor(p); got != partialVerificationMarker {
		t.Fatalf("marker = %q, want the partial marker", got)
	}
	if !zoneReviewTouched(p.Evidence) || isZoneReviewEvidence(p.Evidence) {
		t.Fatal("partial evidence in a zone: zoneReviewTouched must be true, isZoneReviewEvidence (verified only) false")
	}
	if state, _ := decideSubtaskTransition(p, notOptedIn(), evidence.CompatWindowStrict); state != models.WorkItemStateBlocked {
		t.Fatalf("not opted in = %s, want blocked", state)
	}
}

// Codex zones (migration 063): verified evidence carrying
// zone_review_required sends the subtask to human review under its own
// marker, the marker gates the parent, and the parent lands in
// human_review after review even when HUMAN_REVIEW_REQUIRED=false (both the
// payload's and the configuration's flag false) and the agent definition
// skips the human-review step.
func TestZoneReviewLandsInHumanReviewWithGlobalFlagOff(t *testing.T) {
	flagged := implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true,
		Evidence: evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
			ev.Flags = []string{evidence.FlagZoneReviewRequired}
			ev.Zones = []evidence.ZoneTouch{{Name: "payments", Policy: evidence.ZonePolicyHumanReview, Files: []string{"payments/charge.go"}}}
		})}
	for _, policy := range []evidence.ReleasePolicy{notOptedIn(), optedIn()} {
		state, reason := decideSubtaskTransition(flagged, policy, evidence.CompatWindowStrict)
		if state != models.WorkItemStateHumanReview || !strings.Contains(reason, "zone payments") {
			t.Fatalf("flagged verified (opt-in %v) = %s %q, want human_review naming the zone", policy.PartialVerificationOptIn, state, reason)
		}
	}
	marker := humanReviewMarkerFor(flagged)
	if marker != zoneReviewMarker {
		t.Fatalf("marker = %q, want zoneReviewMarker", marker)
	}
	if r := noParentHumanReviewReason(flagged, "x"); !strings.Contains(r, "zone") || strings.Contains(r, "partially verified") {
		t.Fatalf("no-parent reason = %q", r)
	}

	// The subtask's pass carries the marker; the parent is gated.
	store, parentID := newParentFixture(models.WorkItemStateReview, false)
	for id := range store.transitions {
		store.transitions[id] = []*models.WorkItemTransition{{ToState: models.WorkItemStatePassed, Reason: marker + ": outcome verified"}}
	}
	noHumanReviewStep := &agentregistry.AgentDefinition{
		StepResearch: true, StepPlanning: true, StepImplementation: true,
		StepReview: true, StepHumanReview: false, StepQA: true,
	}
	got, err := resolveReviewApprovedTarget(context.Background(), store, parentID, noHumanReviewStep, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ToState != models.WorkItemStateHumanReview || !got.Gated || !got.PublishHumanReviewPrompt {
		t.Fatalf("target = %+v, want gated human_review with the prompt", got)
	}
	if !strings.Contains(got.GateReason, "human_review codex zones") {
		t.Fatalf("gate reason = %q, want the zone cause named", got.GateReason)
	}
	if strings.Contains(got.GateReason, "partially verified") || strings.Contains(got.GateReason, "security-review") {
		t.Fatalf("gate reason = %q names the wrong cause", got.GateReason)
	}
}
