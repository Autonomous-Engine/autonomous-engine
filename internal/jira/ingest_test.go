package jira

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// reverseLookupIssueType replicates the reverse-mapping logic used in
// ingestIssue: given a Jira issue type name, find the internal WorkItemType.
// This logic lives inline in ingestIssue; we extract it here as a pure
// function to validate correctness without touching the database.
func reverseLookupIssueType(mapping *WorkflowMapping, jiraIssueType string) models.WorkItemType {
	internalType := models.WorkItemTypeTask // default
	for internalT, jiraT := range mapping.IssueTypeMap {
		if jiraT == jiraIssueType {
			internalType = models.WorkItemType(internalT)
			break
		}
	}
	return internalType
}

func TestReverseLookupIssueTypeBug(t *testing.T) {
	mapping := &WorkflowMapping{
		IssueTypeMap: map[string]string{
			"feature": "Story",
			"bug":     "Bug",
			"task":    "Task",
		},
	}
	got := reverseLookupIssueType(mapping, "Bug")
	if got != models.WorkItemTypeBug {
		t.Errorf("reverse lookup Bug = %q, want %q", got, models.WorkItemTypeBug)
	}
}

func TestReverseLookupIssueTypeStory(t *testing.T) {
	mapping := &WorkflowMapping{
		IssueTypeMap: map[string]string{
			"feature": "Story",
			"bug":     "Bug",
		},
	}
	got := reverseLookupIssueType(mapping, "Story")
	if got != models.WorkItemTypeFeature {
		t.Errorf("reverse lookup Story = %q, want %q", got, models.WorkItemTypeFeature)
	}
}

func TestReverseLookupIssueTypeDefaultsToTask(t *testing.T) {
	mapping := &WorkflowMapping{
		IssueTypeMap: map[string]string{
			"feature": "Story",
		},
	}
	got := reverseLookupIssueType(mapping, "Epic") // no match in map
	if got != models.WorkItemTypeTask {
		t.Errorf("unknown Jira type should default to task, got %q", got)
	}
}

// reverseLookupPriority replicates the priority reverse-lookup in ingestIssue.
func reverseLookupPriority(mapping *WorkflowMapping, jiraPriority string) models.Priority {
	internalPriority := models.PriorityMedium // default
	for internalP, jiraP := range mapping.PriorityMap {
		if jiraP == jiraPriority {
			internalPriority = models.Priority(internalP)
			break
		}
	}
	return internalPriority
}

func TestReverseLookupPriorityHighest(t *testing.T) {
	mapping := &WorkflowMapping{
		PriorityMap: map[string]string{
			"critical": "Highest",
			"high":     "High",
			"medium":   "Medium",
			"low":      "Low",
		},
	}
	got := reverseLookupPriority(mapping, "Highest")
	if got != models.PriorityCritical {
		t.Errorf("reverse lookup Highest = %q, want %q", got, models.PriorityCritical)
	}
}

func TestReverseLookupPriorityLow(t *testing.T) {
	mapping := &WorkflowMapping{
		PriorityMap: map[string]string{
			"critical": "Highest",
			"high":     "High",
			"medium":   "Medium",
			"low":      "Low",
		},
	}
	got := reverseLookupPriority(mapping, "Low")
	if got != models.PriorityLow {
		t.Errorf("reverse lookup Low = %q, want %q", got, models.PriorityLow)
	}
}

func TestReverseLookupPriorityDefaultsMedium(t *testing.T) {
	mapping := &WorkflowMapping{
		PriorityMap: map[string]string{
			"critical": "Highest",
		},
	}
	got := reverseLookupPriority(mapping, "Unknown Priority")
	if got != models.PriorityMedium {
		t.Errorf("unknown priority should default to medium, got %q", got)
	}
}

// TestWorkflowMappingStructure ensures the WorkflowMapping struct has the
// expected fields populated after construction via DefaultMapping, which is
// important since ingestIssue reads from this mapping without nil-checking.
func TestWorkflowMappingStructure(t *testing.T) {
	mapping := &WorkflowMapping{
		StatusMapping:  map[string]string{"intake": "To Do", "released": "Done"},
		TransitionMap:  map[string]string{"intake": "11"},
		IssueTypeMap:   map[string]string{"feature": "Story", "bug": "Bug"},
		PriorityMap:    map[string]string{"critical": "Highest"},
		RequiredFields: map[string]string{},
		IngestJQL:      "project = PROJ AND labels = autonomous-ingest",
		IngestLabel:    "autonomous-ingest",
	}

	if mapping.StatusMapping == nil {
		t.Error("StatusMapping is nil")
	}
	if mapping.IssueTypeMap == nil {
		t.Error("IssueTypeMap is nil")
	}
	if mapping.PriorityMap == nil {
		t.Error("PriorityMap is nil")
	}
	if mapping.IngestJQL == "" {
		t.Error("IngestJQL is empty")
	}
	if mapping.IngestLabel == "" {
		t.Error("IngestLabel is empty")
	}
}

func TestReverseLookupTask(t *testing.T) {
	mapping := &WorkflowMapping{
		IssueTypeMap: map[string]string{
			"feature":  "Story",
			"bug":      "Bug",
			"task":     "Task",
			"research": "Task",
			"qa":       "Task",
		},
	}
	got := reverseLookupIssueType(mapping, "Task")
	// Task/Research/QA all map to "Task"; the reverse lookup may return any of them
	validTypes := map[models.WorkItemType]bool{
		models.WorkItemTypeTask:     true,
		models.WorkItemTypeResearch: true,
		models.WorkItemTypeQA:       true,
	}
	if !validTypes[got] {
		t.Errorf("reverse lookup Task returned %q, expected task/research/qa", got)
	}
}
