package jira

import (
	"testing"
)

func sampleProject() *JiraProject {
	return &JiraProject{
		Key:  "PROJ",
		Name: "Sample Project",
		Statuses: []JiraStatus{
			{ID: "1", Name: "To Do", Category: "new"},
			{ID: "2", Name: "In Progress", Category: "indeterminate"},
			{ID: "3", Name: "Done", Category: "done"},
		},
		IssueTypes: []JiraIssueType{
			{ID: "10001", Name: "Story", Subtask: false},
			{ID: "10002", Name: "Bug", Subtask: false},
			{ID: "10003", Name: "Task", Subtask: false},
			{ID: "10004", Name: "Sub-task", Subtask: true},
		},
		Priorities: []JiraPriority{
			{ID: "1", Name: "Highest"},
			{ID: "2", Name: "High"},
			{ID: "3", Name: "Medium"},
			{ID: "4", Name: "Low"},
		},
	}
}

func TestDefaultMappingReturnsNonNilMapping(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	if mapping == nil {
		t.Fatal("DefaultMapping returned nil")
	}
}

func TestDefaultMappingStatusCategoryNew(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	// "new" category statuses should map to "intake"
	if _, ok := mapping.StatusMapping["intake"]; !ok {
		t.Error("expected 'intake' key in StatusMapping from 'new' category status")
	}
}

func TestDefaultMappingStatusCategoryDone(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	// "done" category statuses should map to "released"
	if _, ok := mapping.StatusMapping["released"]; !ok {
		t.Error("expected 'released' key in StatusMapping from 'done' category status")
	}
	if mapping.StatusMapping["released"] != "Done" {
		t.Errorf("StatusMapping[released] = %q, want %q", mapping.StatusMapping["released"], "Done")
	}
}

func TestDefaultMappingStatusCategoryIndeterminate(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	// "indeterminate" maps to implementation
	if _, ok := mapping.StatusMapping["implementation"]; !ok {
		t.Error("expected 'implementation' key in StatusMapping from 'indeterminate' status")
	}
}

func TestDefaultMappingIssueTypesBug(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	// Bug should map directly (lowercase match)
	if mapping.IssueTypeMap["bug"] != "Bug" {
		t.Errorf("IssueTypeMap[bug] = %q, want %q", mapping.IssueTypeMap["bug"], "Bug")
	}
}

func TestDefaultMappingIssueTypesTask(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	if mapping.IssueTypeMap["task"] != "Task" {
		t.Errorf("IssueTypeMap[task] = %q, want %q", mapping.IssueTypeMap["task"], "Task")
	}
}

func TestDefaultMappingFeatureFallsBackToStory(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	// "feature" → no direct match, but "story" heuristic should apply
	if mapping.IssueTypeMap["feature"] != "Story" {
		t.Errorf("IssueTypeMap[feature] = %q, want %q", mapping.IssueTypeMap["feature"], "Story")
	}
}

func TestDefaultMappingPriorityMap(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	// critical → Highest (index 0), high → High (index 1)
	if mapping.PriorityMap["critical"] != "Highest" {
		t.Errorf("PriorityMap[critical] = %q, want %q", mapping.PriorityMap["critical"], "Highest")
	}
	if mapping.PriorityMap["high"] != "High" {
		t.Errorf("PriorityMap[high] = %q, want %q", mapping.PriorityMap["high"], "High")
	}
	if mapping.PriorityMap["medium"] != "Medium" {
		t.Errorf("PriorityMap[medium] = %q, want %q", mapping.PriorityMap["medium"], "Medium")
	}
	if mapping.PriorityMap["low"] != "Low" {
		t.Errorf("PriorityMap[low] = %q, want %q", mapping.PriorityMap["low"], "Low")
	}
}

func TestDefaultMappingIngestJQLContainsProjectKey(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	if mapping.IngestJQL == "" {
		t.Error("IngestJQL should not be empty")
	}
	// Should reference the project key
	found := false
	for i := 0; i+4 <= len(mapping.IngestJQL); i++ {
		if mapping.IngestJQL[i:i+4] == "PROJ" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("IngestJQL %q does not contain project key 'PROJ'", mapping.IngestJQL)
	}
}

func TestDefaultMappingIngestLabel(t *testing.T) {
	mapping := DefaultMapping(sampleProject())
	if mapping.IngestLabel == "" {
		t.Error("IngestLabel should not be empty")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{`{"key":"value"}`, `{"key":"value"}`},
		{`Here is the JSON: {"a":1} end`, `{"a":1}`},
		{`{"nested":{"b":2}}`, `{"nested":{"b":2}}`},
		{`no json here`, ``},
		{``, ``},
	}
	for _, tc := range cases {
		got := extractJSON(tc.input)
		if got != tc.expected {
			t.Errorf("extractJSON(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}
