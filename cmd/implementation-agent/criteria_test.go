package main

import (
	"encoding/json"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

func TestTaskMessageCriteriaInSummary(t *testing.T) {
	raw := `{"task_id":"t1","work_id":"w","task_spec":{"schema_version":1,"criterion_ids":["AC-1","AC-3"],"deliverable":"form validates","verification":[]},
	  "criteria":[{"id":"AC-1","text":"x","source":{"kind":"derived","reason":"r"},"risk":"low"}]}`
	var task implementationTaskMessage
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatal(err)
	}
	got := criteriaSummaryFields(task)
	if got["criterion_ids"] != "AC-1,AC-3" || got["deliverable"] != "form validates" {
		t.Fatalf("got %v", got)
	}
	logTaskCriteria(task)

	// Criteria without a spec still name their ids.
	if got := criteriaSummaryFields(implementationTaskMessage{Criteria: []plan.Criterion{{ID: "AC-2"}}}); got["criterion_ids"] != "AC-2" {
		t.Fatalf("got %v", got)
	}
	// A legacy message records an empty list and no deliverable.
	legacy := criteriaSummaryFields(implementationTaskMessage{TaskID: "t"})
	if legacy["criterion_ids"] != "" {
		t.Fatalf("got %v", legacy)
	}
	if _, ok := legacy["deliverable"]; ok {
		t.Fatal("no deliverable without a spec")
	}
	logTaskCriteria(implementationTaskMessage{TaskID: "t"})
}
