package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFindingTypeConstants(t *testing.T) {
	cases := []struct {
		got      FindingType
		expected string
	}{
		{FindingTypeFact, "fact"},
		{FindingTypeInference, "inference"},
		{FindingTypeConcern, "concern"},
		{FindingTypeSuggestion, "suggestion"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.expected {
			t.Errorf("FindingType %q != %q", tc.got, tc.expected)
		}
	}
}

func TestConfidenceConstants(t *testing.T) {
	cases := []struct {
		got      Confidence
		expected string
	}{
		{ConfidenceHigh, "high"},
		{ConfidenceMedium, "medium"},
		{ConfidenceLow, "low"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.expected {
			t.Errorf("Confidence %q != %q", tc.got, tc.expected)
		}
	}
}

func TestResearchFindingJSONRoundTrip(t *testing.T) {
	id := uuid.New()
	taskID := uuid.New()
	workID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	original := ResearchFinding{
		ID:                  id,
		TaskID:              taskID,
		WorkID:              workID,
		FindingType:         FindingTypeConcern,
		Summary:             "A critical concern found",
		Evidence:            json.RawMessage(`{"file":"main.go","line":42}`),
		Confidence:          ConfidenceHigh,
		FollowupSuggestions: []string{"check tests", "review logs"},
		CreatedAt:           now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ResearchFinding
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID mismatch: got %v, want %v", decoded.ID, original.ID)
	}
	if decoded.FindingType != original.FindingType {
		t.Errorf("FindingType mismatch: got %v, want %v", decoded.FindingType, original.FindingType)
	}
	if decoded.Confidence != original.Confidence {
		t.Errorf("Confidence mismatch: got %v, want %v", decoded.Confidence, original.Confidence)
	}
	if len(decoded.FollowupSuggestions) != 2 {
		t.Errorf("FollowupSuggestions len: got %d, want 2", len(decoded.FollowupSuggestions))
	}
	if string(decoded.Evidence) != `{"file":"main.go","line":42}` {
		t.Errorf("Evidence mismatch: got %s", decoded.Evidence)
	}
}

func TestTechnicalBriefJSONRoundTrip(t *testing.T) {
	workID := uuid.New()
	prdRef := uuid.New()

	original := TechnicalBrief{
		WorkID:           workID,
		PRDReference:     prdRef,
		CodexVersion:     5,
		Confidence:       "high",
		Complexity:       "medium",
		ExecutiveSummary: "Summary text",
		CurrentState:     "Current state text",
		RootCause:        "Root cause text",
		ProposedApproach: "Approach text",
		FilesToModify: []FileChange{
			{Path: "internal/foo/bar.go", Description: "Update handler"},
		},
		FilesToCreate: []FileChange{
			{Path: "internal/foo/new.go", Description: "New service"},
		},
		PatternsToFollow: []string{"repository pattern"},
		AntiPatterns:     []string{"global state"},
		EdgeCases:        []string{"nil input"},
		OpenQuestions:    []string{"should we cache?"},
		TestStrategy:     "table-driven unit tests",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded TechnicalBrief
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.WorkID != original.WorkID {
		t.Errorf("WorkID mismatch: got %v, want %v", decoded.WorkID, original.WorkID)
	}
	if decoded.CodexVersion != original.CodexVersion {
		t.Errorf("CodexVersion mismatch: got %v, want %v", decoded.CodexVersion, original.CodexVersion)
	}
	if len(decoded.FilesToModify) != 1 || decoded.FilesToModify[0].Path != "internal/foo/bar.go" {
		t.Errorf("FilesToModify mismatch: %+v", decoded.FilesToModify)
	}
	if len(decoded.FilesToCreate) != 1 || decoded.FilesToCreate[0].Description != "New service" {
		t.Errorf("FilesToCreate mismatch: %+v", decoded.FilesToCreate)
	}
	if len(decoded.PatternsToFollow) != 1 {
		t.Errorf("PatternsToFollow len: got %d, want 1", len(decoded.PatternsToFollow))
	}
}

func TestResearchTaskTypeConstants(t *testing.T) {
	cases := []struct {
		got      ResearchTaskType
		expected string
	}{
		{ResearchTaskTypeCodeTrace, "code_trace"},
		{ResearchTaskTypeHistoryAnalysis, "history_analysis"},
		{ResearchTaskTypePatternMatch, "pattern_match"},
		{ResearchTaskTypeImpactAssessment, "impact_assessment"},
		{ResearchTaskTypeBugArchaeology, "bug_archaeology"},
	}
	for _, tc := range cases {
		if string(tc.got) != tc.expected {
			t.Errorf("ResearchTaskType %q != %q", tc.got, tc.expected)
		}
	}
}
