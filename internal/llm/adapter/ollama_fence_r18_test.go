package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// R18 review M1: live run attempt 15's research answers wrote their calls
// as several JSON objects on separate lines in one ```json fence (9 for
// code_trace, 15 over three fences for impact_assessment). The fence regex
// took each block as one candidate that does not unmarshal, and the inline
// scan was skipped because a candidate existed: 0 calls ran.
func TestAttempt15MultiObjectFencesAreExtracted(t *testing.T) {
	b, err := os.ReadFile("../../../cmd/research-agent/testdata/attempt15/research_responses.json")
	if err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Loops map[string]struct {
			Response string `json:"response_content"`
		} `json:"loops"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	for loop, want := range map[string]int{"code_trace": 9, "impact_assessment": 15} {
		calls := extractEmbeddedToolCalls(tr.Loops[loop].Response)
		if len(calls) != want {
			t.Fatalf("%s: %d calls, want %d", loop, len(calls), want)
		}
		for i, c := range calls {
			if c.ID != fmt.Sprintf("call_%d", i) || c.Name == "" || !json.Valid([]byte(c.Arguments)) {
				t.Fatalf("%s call %d = %+v", loop, i, c)
			}
		}
		if calls[0].Name != "codex_read_file" || !strings.Contains(calls[0].Arguments, `"public/index.html"`) {
			t.Fatalf("%s first call = %+v", loop, calls[0])
		}
	}
	ia := extractEmbeddedToolCalls(tr.Loops["impact_assessment"].Response)
	if ia[9].Name != "codex_dependents_of" || ia[14].Name != "codex_similar_code" {
		t.Fatalf("impact_assessment calls 9/14 = %s / %s", ia[9].Name, ia[14].Name)
	}
}

// A synthetic fence of 9 call objects, one per line, gives 9 calls; a fence
// that mixes a call with a non-call object keeps only the call; a fenced
// example payload (no name/arguments) is still not a call.
func TestMultiObjectFenceFallback(t *testing.T) {
	var b strings.Builder
	b.WriteString("### Plan\nRead them all.\n```json\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, `{"name": "read_file", "arguments": {"path": "f%d.go"}}`+"\n", i)
	}
	b.WriteString("```")
	calls := extractEmbeddedToolCalls(b.String())
	if len(calls) != 9 {
		t.Fatalf("calls = %d, want 9", len(calls))
	}
	for i, c := range calls {
		if c.ID != fmt.Sprintf("call_%d", i) || c.Name != "read_file" || !strings.Contains(c.Arguments, fmt.Sprintf("f%d.go", i+1)) {
			t.Fatalf("call %d = %+v", i, c)
		}
	}
	mixed := "```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}\n{\"result\": {\"ok\": true}, \"note\": \"example\"}\n```"
	if calls := extractEmbeddedToolCalls(mixed); len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("mixed fence = %+v", calls)
	}
	example := "Response shape:\n```json\n{\"status\": \"ok\", \"items\": [1, 2]}\n{\"status\": \"error\"}\n```"
	if calls := extractEmbeddedToolCalls(example); len(calls) != 0 {
		t.Fatalf("example payload became calls: %+v", calls)
	}
	// Several calls inside one <tool_call> tag split the same way.
	tag := "<tool_call>\n{\"name\": \"a\", \"arguments\": {}}\n{\"name\": \"b\", \"arguments\": {}}\n</tool_call>"
	if calls := extractEmbeddedToolCalls(tag); len(calls) != 2 || calls[1].Name != "b" || calls[1].ID != "call_1" {
		t.Fatalf("tag = %+v", calls)
	}
}

// The single-call shapes keep their behaviour.
func TestSingleCallShapesUnchanged(t *testing.T) {
	for name, content := range map[string]string{
		"fence":  "```json\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}\n```",
		"array":  "```json\n[{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}]\n```",
		"tools":  "<tools>{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}</tools>",
		"inline": "### Step 1: read\n\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"a.go\"}}",
	} {
		calls := extractEmbeddedToolCalls(content)
		if len(calls) != 1 || calls[0].ID != "call_0" || calls[0].Name != "read_file" || !strings.Contains(calls[0].Arguments, "a.go") {
			t.Errorf("%s: %+v", name, calls)
		}
	}
	if calls := extractEmbeddedToolCalls("All done. AC-1 is met at a.go:3."); len(calls) != 0 {
		t.Errorf("prose became calls: %+v", calls)
	}
}
