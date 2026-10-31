package main

import (
	"strings"
	"testing"
)

const oneCriterion = `{"schema_version":1,"criteria":[{"id":"AC-1","text":"brace } and \" quote {","source":{"kind":"derived","reason":"r"},"risk":"low"}]}`

// TestParseCriteriaOutputTolerantEnvelope is live run attempt 6: the model
// emitted a complete criteria object followed by an extra "}", and the
// first-{ to last-} slice failed with "invalid character '}' after
// top-level value", costing a re-prompt. The first balanced top-level
// object is used; anything after it is ignored.
func TestParseCriteriaOutputTolerantEnvelope(t *testing.T) {
	second := strings.Replace(oneCriterion, "AC-1", "AC-9", 1)
	cases := map[string]string{
		"trailing brace":      oneCriterion + "}",
		"trailing prose":      oneCriterion + "\n\nThat covers every requirement {see the PRD}.",
		"two objects":         oneCriterion + "\n" + second,
		"fenced":              "Here you go:\n```json\n" + oneCriterion + "\n```\n",
		"think tags":          "<think>maybe {\"criteria\": []} is wrong</think>\n" + oneCriterion,
		"prose brace first":   "Using {the PRD} as the source: " + oneCriterion,
		"trailing brace+text": oneCriterion + "}\nDone.",
	}
	for name, content := range cases {
		bc, err := parseCriteriaOutput(content)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(bc.Criteria) != 1 || bc.Criteria[0].ID != "AC-1" || bc.Criteria[0].Text != `brace } and " quote {` {
			t.Errorf("%s: criteria = %+v, want the first object's AC-1", name, bc.Criteria)
		}
	}
}

// A malformed object still reports why it is invalid (the re-prompt quotes
// the error), not a vaguer "no criteria".
func TestParseCriteriaOutputReportsSyntaxError(t *testing.T) {
	_, err := parseCriteriaOutput(`{"criteria":[{"id":"AC-1",}]}`)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("err = %v, want an invalid JSON error", err)
	}
	_, err = parseCriteriaOutput(`{"criteria":[{"id":"AC-1"`)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("unterminated: err = %v, want an invalid JSON error", err)
	}
}
