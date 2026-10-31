package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// attempt9Dir holds the R10 replay fixture: a synthetic app
// (testdata/sample-app, made up for these tests) and the tool calls of
// testdata/attempt9_calls.json, written to reproduce the edits a live run
// (attempt 9) made — the same edit shapes, numbered by that run's calls.
const attempt9Dir = "testdata"

type attempt9Call struct {
	Call      int `json:"call"`
	ToolCalls []struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"tool_calls"`
}

// attempt9ToolCall is the k-th tool call of recorded call n.
func attempt9ToolCall(t *testing.T, n, k int) adapter.ToolCall {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(attempt9Dir, "attempt9_calls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Calls []attempt9Call `json:"calls"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	for _, c := range tr.Calls {
		if c.Call == n {
			tc := c.ToolCalls[k]
			return adapter.ToolCall{ID: "call_0", Name: tc.Name, Arguments: tc.Arguments}
		}
	}
	t.Fatalf("no call %d", n)
	return adapter.ToolCall{}
}

// attempt9Tree copies the sample-app files into a temp tree (LF, whatever
// the checkout did to them).
func attempt9Tree(t *testing.T) string {
	t.Helper()
	src := filepath.Join(attempt9Dir, "sample-app")
	root := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, []byte(strings.ReplaceAll(string(b), "\r\n", "\n")), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func runEdit(t *testing.T, ex *Executor, call adapter.ToolCall) adapter.ToolResult {
	t.Helper()
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	p, _ := args["path"].(string)
	ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "read_file", Arguments: `{"path":"` + p + `"}`})
	return ex.Execute(context.Background(), call)
}

// R10 W1: attempt 9's call-35 edit, a clean insertion of a new describe
// block before `describe('GET /shelves/:id/cover'`, repeats a common
// assertion (6 -> 8 occurrences). It is correct and must be applied.
func TestDuplicateGuardAcceptsAttempt9Call35(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	res := runEdit(t, ex, attempt9ToolCall(t, 35, 0))
	if res.IsError {
		t.Fatalf("call-35 edit rejected: %s", res.Content)
	}
	b, _ := os.ReadFile(filepath.Join(root, "tests/unit/server.test.ts"))
	if !strings.Contains(string(b), "describe('GET /api/settings'") {
		t.Fatal("the new describe block was not written")
	}
}

// R10 W1: attempt 9's call-11 (and 13) edit re-types file lines 22-29
// (`let app…`, `beforeAll…`) right after its old_string. It is rejected
// and the message names those lines and where both copies would be.
func TestDuplicateGuardRejectsAttempt9Call11WithTheRepeatedLines(t *testing.T) {
	for _, n := range []int{11, 13} {
		root := attempt9Tree(t)
		ex := NewExecutor(root)
		res := runEdit(t, ex, attempt9ToolCall(t, n, 0))
		if !res.IsError {
			t.Fatalf("call-%d edit was applied", n)
		}
		t.Logf("call-%d: %s", n, res.Content)
		for _, want := range []string{"file lines 22-29", "let app: ReturnType<typeof createApp>['app'];", "beforeAll(() => {"} {
			if !strings.Contains(res.Content, want) {
				t.Errorf("call-%d rejection does not name %q:\n%s", n, want, res.Content)
			}
		}
		if strings.Contains(res.Content, "expect(res.status).toBe(200);") {
			t.Errorf("call-%d rejection names the wrong line:\n%s", n, res.Content)
		}
	}
}

// The PR #9 case (an import typed 13 times) is still rejected.
func TestDuplicateGuardStillRejectsRepeatedImport(t *testing.T) {
	root := t.TempDir()
	orig := "import { a } from 'a';\n\nexport const x = 1;\n"
	if err := os.WriteFile(filepath.Join(root, "f.ts"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(root)
	newStr := strings.Repeat("import * as z from \"zod\";\n", 13) + "export const x = 1;"
	args, _ := json.Marshal(map[string]string{"path": "f.ts", "old_string": "export const x = 1;", "new_string": newStr})
	res := runEdit(t, ex, adapter.ToolCall{ID: "e", Name: "edit_file", Arguments: string(args)})
	if !res.IsError || !strings.Contains(res.Content, `import * as z from \"zod\";`) && !strings.Contains(res.Content, `import * as z from "zod";`) {
		t.Fatalf("13x import not rejected with the line named: %v %s", res.IsError, res.Content)
	}
}

// A line that already appeared twice and appears a third time is not
// suspicious (the documented ≤1 -> ≥3 rule).
func TestDuplicateGuardOnlyFlagsLinesThatWereRare(t *testing.T) {
	old := "expect(res.status).toBe(200);\nfoo();\nexpect(res.status).toBe(200);\n"
	nu := old + "expect(res.status).toBe(200);\nexpect(res.status).toBe(200);\n"
	if msg := detectSuspiciousDuplicates(old, nu); msg != "" {
		t.Fatalf("2 -> 4 flagged: %s", msg)
	}
	if msg := detectSuspiciousDuplicates("x := computeSomething()\n", strings.Repeat("x := computeSomething()\n", 3)); msg == "" {
		t.Fatal("1 -> 3 not flagged")
	}
}
