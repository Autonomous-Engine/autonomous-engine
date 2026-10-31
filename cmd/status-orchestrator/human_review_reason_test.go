package main

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// The gate reason must name the actual cause: partial outcomes and
// security-review paths read differently, and a mixed parent lists both.
func TestHumanReviewGateReasonNamesTheCause(t *testing.T) {
	partialID, securityID := uuid.New(), uuid.New()
	children := []*models.WorkItem{
		{ID: partialID, Title: "partial", Type: models.WorkItemTypeSubtask},
		{ID: securityID, Title: "security", Type: models.WorkItemTypeSubtask},
	}
	lookup := func(id uuid.UUID) []*models.WorkItemTransition {
		switch id {
		case partialID:
			return []*models.WorkItemTransition{{ToState: models.WorkItemStatePassed, Reason: partialVerificationMarker + " outcome partially_verified"}}
		case securityID:
			return []*models.WorkItemTransition{{ToState: models.WorkItemStatePassed, Reason: securityReviewMarker + " touches auth"}}
		}
		return nil
	}

	got := humanReviewGateReason(children[:1], lookup)
	if !strings.Contains(got, "1 subtask(s) completed on partially verified evidence and require mandatory human review before QA: partial (") || strings.Contains(got, "security-review") {
		t.Errorf("partial-only reason = %q", got)
	}
	got = humanReviewGateReason(children[1:], lookup)
	if !strings.Contains(got, "1 subtask(s) touch security-review paths and require mandatory human review before QA: security (") || strings.Contains(got, "partially verified") {
		t.Errorf("security-only reason = %q", got)
	}
	got = humanReviewGateReason(children, lookup)
	if !strings.Contains(got, "2 subtask(s) require mandatory human review before QA: 1 completed on partially verified evidence (partial (") || !strings.Contains(got, "1 touch security-review paths (security (") {
		t.Errorf("mixed reason = %q", got)
	}
}
