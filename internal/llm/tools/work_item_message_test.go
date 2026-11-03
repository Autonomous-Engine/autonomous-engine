package tools

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The chat confirmation linked the new work item to "#" because the tool
// result carried no path for the model to use (F33).
func TestWorkItemCreatedMessageCarriesTheDashboardPath(t *testing.T) {
	id := uuid.New()
	got := workItemCreatedMessage(id, "Add parser option", "feature", "medium", nil)
	if !strings.Contains(got, "/work-items/"+id.String()) {
		t.Fatalf("message %q lacks the dashboard path", got)
	}
	pid := uuid.New()
	if got := workItemCreatedMessage(id, "t", "feature", "medium", &pid); !strings.Contains(got, pid.String()) {
		t.Fatalf("message %q lacks the product", got)
	}
}
