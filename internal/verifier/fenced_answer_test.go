package verifier

import "testing"

// Live (ciblame item 779aa0f7), the verifier's closing answer was its
// verdict in a ```json fence with a sentence after it, and the parser
// rejected the whole verdict: "answer opens a code fence but is not exactly
// one fenced JSON object". A fenced JSON object is the answer whatever
// prose a local model puts around it.
func TestParseVerdictTakesAFencedObjectWithProseAroundIt(t *testing.T) {
	for _, content := range []string{
		"Here is my verdict:\n\n```json\n" + approveAC1 + "\n```\n\nAll criteria are satisfied.",
		"```json\n" + approveAC1 + "\n```\nDone.",
		"Final answer:\n```\n" + approveAC1 + "\n```",
	} {
		v, err := ParseVerdict(content, []string{"AC-1"}, Grounding{})
		if err != nil || v.Overall != OverallApprove {
			t.Fatalf("content %q: err=%v verdict=%+v, want the fenced object parsed", content, err, v)
		}
	}
	// Two fenced objects are still not one answer.
	if _, err := ParseVerdict("```json\n"+approveAC1+"\n```\nor\n```json\n"+approveAC1+"\n```", []string{"AC-1"}, Grounding{}); err == nil {
		t.Fatalf("two fenced objects parsed as one answer")
	}
}
