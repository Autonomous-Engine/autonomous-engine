package stage

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// Codex coherence Part 3: an ungrounded run is never approved
// automatically. An approving decision becomes an escalation (the parent
// goes to human review; no PR); other decisions keep theirs. The evidence
// carried on verification.complete gets the ungrounded flag either way.
func TestMarkUngrounded(t *testing.T) {
	impl := json.RawMessage(`{"task_id":"t","work_id":"w","success":true,"evidence":{"schema_version":1,"outcome":"verified","flags":["security_review_required"]}}`)
	for _, tc := range []struct {
		in, want Decision
	}{
		{DecisionApprove, DecisionEscalate},
		{DecisionApprovePendingSecondPass, DecisionEscalate},
		{DecisionRequestChanges, DecisionRequestChanges},
		{DecisionEscalate, DecisionEscalate},
		{DecisionNotRun, DecisionNotRun},
	} {
		o := &Outcome{Message: Message{Decision: tc.in, Reason: "gate", Implementation: impl}, Result: Result{Decision: tc.in, Reason: "gate"}}
		o.MarkUngrounded("the implementation run recorded 6 empty codex lookups")
		if o.Message.Decision != tc.want || o.Result.Decision != tc.want {
			t.Errorf("%s → message %s result %s, want %s", tc.in, o.Message.Decision, o.Result.Decision, tc.want)
		}
		if tc.in.Approves() {
			if !strings.Contains(o.Message.Reason, "6 empty codex lookups") || o.Result.Reason != o.Message.Reason {
				t.Errorf("%s: reason %q / %q", tc.in, o.Message.Reason, o.Result.Reason)
			}
			if o.Message.Decision.Approves() {
				t.Errorf("%s still approves", tc.in)
			}
		}
		var m struct {
			Evidence evidence.ImplementationEvidence `json:"evidence"`
			TaskID   string                          `json:"task_id"`
		}
		if err := json.Unmarshal(o.Message.Implementation, &m); err != nil {
			t.Fatal(err)
		}
		if !m.Evidence.HasFlag(evidence.FlagUngrounded) || !m.Evidence.HasFlag(evidence.FlagSecurityReviewRequired) || m.TaskID != "t" {
			t.Errorf("%s: implementation = %s", tc.in, o.Message.Implementation)
		}
	}

	// A message without evidence is left as it is.
	o := &Outcome{Message: Message{Decision: DecisionNotRun, Implementation: json.RawMessage(`{"task_id":"t"}`)}}
	o.MarkUngrounded("x")
	if string(o.Message.Implementation) != `{"task_id":"t"}` {
		t.Errorf("implementation = %s", o.Message.Implementation)
	}
}
