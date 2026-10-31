package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWorkItemTypeConstants(t *testing.T) {
	cases := []struct {
		name     string
		got      WorkItemType
		expected string
	}{
		{"Feature", WorkItemTypeFeature, "feature"},
		{"Bug", WorkItemTypeBug, "bug"},
		{"Task", WorkItemTypeTask, "task"},
		{"Subtask", WorkItemTypeSubtask, "subtask"},
		{"Research", WorkItemTypeResearch, "research"},
		{"QA", WorkItemTypeQA, "qa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.expected {
				t.Errorf("WorkItemType %s = %q, want %q", tc.name, tc.got, tc.expected)
			}
		})
	}
}

func TestWorkItemStateConstants(t *testing.T) {
	cases := []struct {
		got      WorkItemState
		expected string
	}{
		{WorkItemStateIntake, "intake"},
		{WorkItemStatePRD, "prd"},
		{WorkItemStateResearch, "research"},
		{WorkItemStatePlanning, "planning"},
		{WorkItemStateImplementation, "implementation"},
		{WorkItemStateVerification, "verification"},
		{WorkItemStateReview, "review"},
		{WorkItemStateHumanReview, "human_review"},
		{WorkItemStateQA, "qa"},
		{WorkItemStatePassed, "passed"},
		{WorkItemStateFailed, "failed"},
		{WorkItemStateBlocked, "blocked"},
		{WorkItemStateRejected, "rejected"},
		{WorkItemStateReleased, "released"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.expected {
			t.Errorf("WorkItemState %q != %q", tc.got, tc.expected)
		}
	}
}

func TestPriorityConstants(t *testing.T) {
	cases := []struct {
		got      Priority
		expected string
	}{
		{PriorityCritical, "critical"},
		{PriorityHigh, "high"},
		{PriorityMedium, "medium"},
		{PriorityLow, "low"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.expected {
			t.Errorf("Priority %q != %q", tc.got, tc.expected)
		}
	}
}

func TestArtifactTypeConstants(t *testing.T) {
	cases := []struct {
		got      ArtifactType
		expected string
	}{
		{ArtifactTypePRD, "prd"},
		{ArtifactTypeTechnicalBrief, "technical_brief"},
		{ArtifactTypeResearchFinding, "research_finding"},
		{ArtifactTypeQAReport, "qa_report"},
		{ArtifactTypeReviewComments, "review_comments"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.expected {
			t.Errorf("ArtifactType %q != %q", tc.got, tc.expected)
		}
	}
}

func TestWorkItemJSONRoundTrip(t *testing.T) {
	id := uuid.New()
	parentID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	original := WorkItem{
		ID:           id,
		ExternalRef:  "PROJ-123",
		Type:         WorkItemTypeFeature,
		State:        WorkItemStateIntake,
		Title:        "My feature",
		Description:  "A detailed description",
		Priority:     PriorityHigh,
		CodexVersion: 3,
		ParentID:     &parentID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded WorkItem
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID mismatch: got %v, want %v", decoded.ID, original.ID)
	}
	if decoded.ExternalRef != original.ExternalRef {
		t.Errorf("ExternalRef mismatch: got %v, want %v", decoded.ExternalRef, original.ExternalRef)
	}
	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: got %v, want %v", decoded.Type, original.Type)
	}
	if decoded.State != original.State {
		t.Errorf("State mismatch: got %v, want %v", decoded.State, original.State)
	}
	if decoded.Priority != original.Priority {
		t.Errorf("Priority mismatch: got %v, want %v", decoded.Priority, original.Priority)
	}
	if decoded.ParentID == nil || *decoded.ParentID != parentID {
		t.Errorf("ParentID mismatch: got %v, want %v", decoded.ParentID, parentID)
	}
}

func TestWorkItemJSONOmitsNilParentID(t *testing.T) {
	item := WorkItem{
		ID:    uuid.New(),
		Type:  WorkItemTypeBug,
		State: WorkItemStatePRD,
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal to map failed: %v", err)
	}

	if _, ok := m["parent_id"]; ok {
		t.Error("expected parent_id to be omitted when nil, but it was present")
	}
}
