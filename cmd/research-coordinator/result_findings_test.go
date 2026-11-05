package main

import (
	"encoding/json"
	"testing"
)

// The research agent's result carries the findings it recorded with
// verified citations; the coordinator persists its copy of the result
// (research_round_tasks.result) and hands reused results back from it, so
// the copy must keep them (NEW-9).
func TestCoordinatorResultCopyKeepsFindings(t *testing.T) {
	wire := `{"task_id":"t1","work_id":"w1","status":"ok","finding_type":"code_trace","evidence":"e",
		"findings":[{"claim":"the parser trims","path":"a.go","start_line":3,"end_line":4,"excerpt":"x"}]}`
	var res researchResultMessage
	if err := json.Unmarshal([]byte(wire), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Claim != "the parser trims" || res.Findings[0].Path != "a.go" || res.Findings[0].StartLine != 3 || res.Findings[0].EndLine != 4 {
		t.Fatalf("decoded findings = %+v, want the agent's one", res.Findings)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var again researchResultMessage
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Findings) != 1 || again.Findings[0].Excerpt != "x" {
		t.Fatalf("stored copy = %s, findings dropped", raw)
	}
}
