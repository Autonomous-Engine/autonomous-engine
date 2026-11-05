package adapter

import (
	"strings"
	"testing"
)

// Live (ciblame item 779aa0f7), qwen3.6:35b put the whole verdict object in
// its thinking with content empty. The extractor looked for the last JSON
// array before the last object, and the verdict's `"defects": []` is the
// last array: the answer the verifier parsed was "[]". The outermost
// block — the one that ends last — is the answer; an empty [] or {} never is.
func TestExtractStructuredFromThinkingTakesTheOutermostBlock(t *testing.T) {
	verdict := `{"schema_version":1,"per_criterion":[{"id":"AC-1","status":"met","evidence":["a.go:1"],"note":""}],"defects":[],"overall":"approve","summary":"all met"}`
	thinking := "Let me go through each criterion.\n\nAC-1 is met because the slice includes the value [1, 2].\n\nSo the verdict is:\n\n" + verdict
	if got := extractStructuredFromThinking(thinking); got != verdict {
		t.Fatalf("extracted %q, want the whole verdict object", got)
	}

	plan := `[{"title":"one","files":["a.go"]},{"title":"two","files":[]}]`
	thinking = "The decomposition, with the empty object {} noted above, is:\n" + plan
	if got := extractStructuredFromThinking(thinking); got != plan {
		t.Fatalf("extracted %q, want the whole task array", got)
	}

	trailing := "I checked everything.\n\n" + verdict + "\n\nThe defects list is [] and nothing else is {}."
	if got := extractStructuredFromThinking(trailing); got != verdict {
		t.Fatalf("extracted %q, want the verdict, not a trailing empty fragment", got)
	}
	if got := extractStructuredFromThinking("no structure here, just reasoning"); !strings.HasPrefix(got, "no structure") {
		t.Fatalf("extracted %q, want the raw thinking when nothing structured exists", got)
	}
}
