package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// TestSecurityReviewMarker (M3-03, D3): verified evidence carrying
// security_review_required goes to human review under its own marker and
// wording; partially verified evidence keeps the partial marker; both gate
// the parent.
func TestSecurityReviewMarker(t *testing.T) {
	flagged := implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true,
		Evidence: evidenceJSON(t, true, func(ev *evidence.ImplementationEvidence) {
			ev.Flags = []string{evidence.FlagSecurityReviewRequired}
		})}
	partial := implementationCompletePayload{TaskID: testTaskID, WorkID: testWorkID, Success: true, Evidence: partiallyVerifiedJSON(t)}

	for _, policy := range []evidence.ReleasePolicy{notOptedIn(), optedIn()} {
		state, reason := decideSubtaskTransition(flagged, policy, evidence.CompatWindowStrict)
		if state != models.WorkItemStateHumanReview || !strings.Contains(reason, "security-review paths") {
			t.Fatalf("flagged verified (opt-in %v) = %s %q, want human_review naming security review", policy.PartialVerificationOptIn, state, reason)
		}
	}
	if got := humanReviewMarkerFor(flagged); got != securityReviewMarker {
		t.Fatalf("marker for flagged verified = %q, want securityReviewMarker", got)
	}
	if got := humanReviewMarkerFor(partial); got != partialVerificationMarker {
		t.Fatalf("marker for partial = %q, want partialVerificationMarker", got)
	}
	if r := noParentHumanReviewReason(flagged, "x"); !strings.Contains(r, "security-review paths") || strings.Contains(r, "partially verified") {
		t.Fatalf("no-parent reason for flagged = %q", r)
	}
	if r := noParentHumanReviewReason(partial, "x"); !strings.Contains(r, "partially verified") {
		t.Fatalf("no-parent reason for partial = %q", r)
	}

	// The security marker gates the parent like the partial one.
	child := uuid.New()
	gated := partiallyVerifiedSubtasks(
		[]*models.WorkItem{{ID: child, Type: models.WorkItemTypeSubtask, Title: "auth change"}},
		func(uuid.UUID) []*models.WorkItemTransition {
			return []*models.WorkItemTransition{{ToState: models.WorkItemStatePassed, Reason: securityReviewMarker + ": outcome verified"}}
		})
	if len(gated) != 1 || gated[0].ID != child {
		t.Fatalf("gated = %v, want the security-flagged subtask", gated)
	}
}
