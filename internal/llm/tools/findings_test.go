package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// findingsExecutor is an executor over a temp tree holding a.go (10 lines)
// and long.go (100 lines).
func findingsExecutor(t *testing.T) *Executor {
	t.Helper()
	dir := t.TempDir()
	var a, long strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&a, "line %d of a\n", i)
	}
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&long, "line %d of long\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(a.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "long.go"), []byte(long.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewExecutor(dir)
}

func recordCall(claim, path string, start, end int) adapter.ToolCall {
	return adapter.ToolCall{ID: "f1", Name: "record_finding", Arguments: fmt.Sprintf(`{"claim":%q,"path":%q,"start_line":%d,"end_line":%d}`, claim, path, start, end)}
}

// A finding is a claim with a citation the executor can verify: the file is
// in the tree, the lines exist, and the loop has read them. Anything else
// is refused with the reason, so the model cannot cite what it never saw.
func TestRecordFindingRefusesUnreadMissingAndBadClaims(t *testing.T) {
	ex := findingsExecutor(t)
	ctx := context.Background()
	refuse := func(name string, call adapter.ToolCall, want string) {
		t.Helper()
		res := ex.Execute(ctx, call)
		if !res.IsError || !strings.Contains(res.Content, want) {
			t.Errorf("%s: result = %+v, want an error mentioning %q", name, res, want)
		}
	}
	refuse("unread range", recordCall("x", "a.go", 1, 2), "read")
	ex.markRead("a.go", 1, 10, 10)
	ex.markRead("long.go", 1, 200, 200)
	refuse("missing file", recordCall("x", "missing.go", 1, 1), "missing.go")
	refuse("empty claim", recordCall("   ", "a.go", 1, 1), "claim")
	refuse("long claim", recordCall(strings.Repeat("c", findingClaimMax+1), "a.go", 1, 1), "claim")
	refuse("line 0", recordCall("x", "a.go", 0, 1), "start_line")
	refuse("past the end", recordCall("x", "a.go", 9, 11), "10 lines")
	refuse("too wide", recordCall("x", "long.go", 1, findingRangeMax+10), fmt.Sprint(findingRangeMax))
	if n := len(ex.Findings()); n != 0 {
		t.Fatalf("refused findings were kept: %d", n)
	}
}

func TestRecordFindingKeepsTheExecutorsOwnExcerpt(t *testing.T) {
	ex := findingsExecutor(t)
	ctx := context.Background()
	var stored []CitedFinding
	ex.SetFindingStore(func(_ context.Context, f CitedFinding) error { stored = append(stored, f); return nil })
	ex.markRead("a.go", 3, 5, 10)

	res := ex.Execute(ctx, recordCall("a is read in lines 3-4", "a.go", 3, 4))
	if res.IsError || res.Content != "recorded finding #1: a is read in lines 3-4 (a.go:3-4)" {
		t.Fatalf("result = %+v", res)
	}
	// end_line defaults to start_line.
	res = ex.Execute(ctx, adapter.ToolCall{ID: "f2", Name: "record_finding", Arguments: `{"claim":"line five","path":"./a.go","start_line":5}`})
	if res.IsError || !strings.HasPrefix(res.Content, "recorded finding #2: line five (a.go:5-5)") {
		t.Fatalf("second result = %+v", res)
	}
	fs := ex.Findings()
	if len(fs) != 2 || fs[0].Excerpt != "line 3 of a\nline 4 of a" || fs[1].Excerpt != "line 5 of a" {
		t.Fatalf("findings = %+v", fs)
	}
	if fs[0].Path != "a.go" || fs[0].StartLine != 3 || fs[0].EndLine != 4 || fs[1].Path != "a.go" {
		t.Fatalf("citations = %+v", fs)
	}
	if len(stored) != 2 || stored[1].Claim != "line five" {
		t.Fatalf("store received %+v", stored)
	}
	// The excerpt is taken by the executor and bounded.
	ex.markRead("long.go", 1, 60, 100)
	res = ex.Execute(ctx, recordCall("wide", "long.go", 1, 60))
	if res.IsError {
		t.Fatal(res.Content)
	}
	if n := len([]rune(ex.Findings()[2].Excerpt)); n > findingExcerptMax || !strings.HasPrefix(ex.Findings()[2].Excerpt, "line 1 of long\nline 2 of long") {
		t.Fatalf("excerpt not the bounded cited lines: %d runes, %q", n, ex.Findings()[2].Excerpt[:40])
	}
}

func TestResearchToolsOfferRecordFindingOnlyWhenAsked(t *testing.T) {
	has := func(ts []adapter.Tool) bool {
		for _, tl := range ts {
			if tl.Name == "record_finding" {
				return true
			}
		}
		return false
	}
	if has(ResearchToolsFor("code_trace", ResearchToolOptions{})) {
		t.Fatal("record_finding offered without Findings")
	}
	if !has(ResearchToolsFor("code_trace", ResearchToolOptions{Findings: true})) {
		t.Fatal("record_finding not offered with Findings")
	}
}
