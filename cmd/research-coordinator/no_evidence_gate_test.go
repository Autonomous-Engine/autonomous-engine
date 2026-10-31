package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// R18 E1: research-agent's no_evidence result (the model answered twice
// without running a tool; the prose kept as note) is a failed task for the
// honest-failure gate like every other non-ok status: no new block path,
// and it never counts as transient.
func TestHonestFailureGateCountsNoEvidenceAsFailed(t *testing.T) {
	wire := `{"task_id":"fb4393c9","work_id":"59bd88f8-3b7f-4bb3-9739-636b21b40d67","status":"no_evidence","finding_type":"code_trace",` +
		`"evidence":"Research tool loop failed: no evidence: the model answered 2 times without running a tool call","confidence":0,` +
		`"error_detail":"no evidence: the model answered 2 times without running a tool call","note":"### Plan\n1. **Read the specified files** …"}`
	var r researchResultMessage
	if err := json.Unmarshal([]byte(wire), &r); err != nil {
		t.Fatal(err)
	}
	ok := researchResultMessage{TaskID: "42c9b43a", FindingType: "impact_assessment", Status: ResearchStatusOK}
	failed, transient, okCount := judgeResearchResults([]researchResultMessage{r, ok})
	if len(failed) != 1 || transient != 0 || okCount != 1 {
		t.Fatalf("failed = %v, transient = %d, ok = %d", failed, transient, okCount)
	}
	if !strings.Contains(failed[0], "task fb4393c9 [code_trace] status=no_evidence: no evidence: the model answered 2 times without running a tool call") {
		t.Fatalf("summary = %q", failed[0])
	}
	// The other statuses keep their behaviour.
	failed, transient, okCount = judgeResearchResults([]researchResultMessage{
		{TaskID: "a", Status: "loop_stuck", ErrorDetail: "LoopStuck (cycle)"},
		{TaskID: "b", Status: ResearchStatusError, ErrorDetail: "status 429"},
		{TaskID: "c"},
	})
	if len(failed) != 3 || transient != 1 || okCount != 0 || !strings.Contains(failed[2], "status=missing: (no error detail)") {
		t.Fatalf("failed = %v, transient = %d, ok = %d", failed, transient, okCount)
	}
}
