package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// R11 S2: each insertion tool's schema stays under 600 chars.
func TestInsertToolSchemasBounded(t *testing.T) {
	for _, tl := range []adapter.Tool{InsertAfter(), AppendToFile()} {
		b, _ := json.Marshal(tl)
		t.Logf("%s schema: %d chars", tl.Name, len(b))
		if len(b) > 600 {
			t.Errorf("%s schema is %d chars, want <= 600", tl.Name, len(b))
		}
	}
	for _, n := range []string{"edit_file", "write_file", "insert_after", "append_to_file"} {
		if !IsFileEditTool(n) || !isEditTool(n) || !isWriteTool(n) {
			t.Errorf("%s is not treated as an edit", n)
		}
	}
}

func insertFixture(t *testing.T, name, content string) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewExecutor(root), root
}

func run(ex *Executor, name string, args map[string]any) adapter.ToolResult {
	b, _ := json.Marshal(args)
	return ex.Execute(context.Background(), adapter.ToolCall{ID: "c", Name: name, Arguments: string(b)})
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "const v%d = %d;\n", i, i)
	}
	return b.String()
}

// insert_after: the anchor by line or by text, the read-before-edit gate,
// the read-coverage rule, the syntax check, idempotence; append_to_file.
func TestInsertAfterRules(t *testing.T) {
	ex, root := insertFixture(t, "a.ts", numbered(600))
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(root, "a.ts"))
		return string(b)
	}

	// Not read: refused.
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 3, "text": "const x = 1;"}); !r.IsError || !strings.Contains(r.Content, "read_file must be called") {
		t.Fatalf("unread: %s", r.Content)
	}
	// Read lines 1-400: line 500 is outside.
	run(ex, "read_file", map[string]any{"path": "a.ts", "start_line": 1, "end_line": 400})
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 500, "text": "const x = 1;"}); !r.IsError || !strings.Contains(r.Content, "outside the lines you read (lines 1-400 of 600)") {
		t.Fatalf("unread anchor: %s", r.Content)
	}
	// By line.
	r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 3, "text": "const x = 1;\nconst y = 2;"})
	if r.IsError || !strings.Contains(r.Content, "Inserted 2 lines into a.ts after line 3: now at lines 4-5") {
		t.Fatalf("insert: %s", r.Content)
	}
	if !strings.HasPrefix(read(), "const v1 = 1;\nconst v2 = 2;\nconst v3 = 3;\nconst x = 1;\nconst y = 2;\nconst v4 = 4;\n") {
		t.Fatalf("file:\n%.200s", read())
	}
	// The same insertion again: already applied, not written twice.
	r = run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 3, "text": "const x = 1;\nconst y = 2;"})
	if r.IsError || !strings.Contains(r.Content, "already applied at line 4") || strings.Count(read(), "const x = 1;") != 1 {
		t.Fatalf("re-insert: %s", r.Content)
	}
	// By text: unique, ambiguous, absent; top of file.
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_text": "const v10 = 10;", "text": "// after ten"}); r.IsError || !strings.Contains(r.Content, "after line 12") {
		t.Fatalf("anchor_text: %s", r.Content)
	}
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_text": "const v1", "text": "// x"}); !r.IsError || !strings.Contains(r.Content, "pass anchor_line=<one of them>") {
		t.Fatalf("ambiguous anchor_text: %s", r.Content)
	}
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_text": "const v1", "anchor_line": 120, "text": "// near 120"}); r.IsError {
		t.Fatalf("anchor_text near anchor_line: %s", r.Content)
	}
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_text": "nowhere", "text": "// x"}); !r.IsError || !strings.Contains(r.Content, "is not in the file") {
		t.Fatalf("absent anchor_text: %s", r.Content)
	}
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 0, "text": "// header"}); r.IsError || !strings.HasPrefix(read(), "// header\nconst v1 = 1;") {
		t.Fatalf("top: %s", r.Content)
	}
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "text": "// x"}); !r.IsError || !strings.Contains(r.Content, "needs anchor_line") {
		t.Fatalf("no anchor: %s", r.Content)
	}
	// An insertion that leaves the file unparsable is refused (R10 W7).
	before := read()
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 5, "text": "function broken() {"}); !r.IsError || read() != before {
		t.Fatalf("unbalanced insert: %s", r.Content)
	}
	// append_to_file needs the end of the file read.
	if r := run(ex, "append_to_file", map[string]any{"path": "a.ts", "text": "export {};"}); !r.IsError || !strings.Contains(r.Content, "outside the lines you read") {
		t.Fatalf("append unread end: %s", r.Content)
	}
	run(ex, "read_file", map[string]any{"path": "a.ts", "start_line": 400, "end_line": 700})
	if r := run(ex, "append_to_file", map[string]any{"path": "a.ts", "text": "export {};"}); r.IsError || !strings.HasSuffix(read(), "const v600 = 600;\nexport {};\n") {
		t.Fatalf("append: %s", r.Content)
	}
	// A missing file is write_file's job.
	if r := run(ex, "insert_after", map[string]any{"path": "missing.ts", "anchor_line": 0, "text": "x"}); !r.IsError {
		t.Fatalf("missing file: %s", r.Content)
	}
	// The ledger line names the outcome like edit_file's.
	if got := ledgerLine("insert_after", `{"path":"a.ts","anchor_line":3,"text":"x"}`, "Inserted 2 lines into a.ts after line 3: now at lines 4-5", false); got != "- insert_after(a.ts) -> applied L4-5" {
		t.Fatalf("ledger line = %q", got)
	}
}

// A file without a trailing newline gets one before the inserted lines.
func TestAppendToFileWithoutTrailingNewline(t *testing.T) {
	ex, root := insertFixture(t, "notes.md", "# Notes\nlast line")
	run(ex, "read_file", map[string]any{"path": "notes.md"})
	if r := run(ex, "append_to_file", map[string]any{"path": "notes.md", "text": "- added"}); r.IsError {
		t.Fatal(r.Content)
	}
	b, _ := os.ReadFile(filepath.Join(root, "notes.md"))
	if string(b) != "# Notes\nlast line\n- added\n" {
		t.Fatalf("file = %q", b)
	}
}

// The steering names the insertion line and the changed lines.
func TestInsertSteering(t *testing.T) {
	content := "a\nb\nc\n});\n"
	// Re-typing the anchor around new lines: a pure insertion after line 3.
	got := insertSteering("x.ts", content, "});", "new1\nnew2\n});")
	if !strings.Contains(got, "after line 3 use insert_after(path=\"x.ts\", anchor_line=3") || !strings.Contains(got, "to change line 4,") {
		t.Fatalf("steering = %s", got)
	}
	// A real change of line 2 plus an addition: lines 2-2 change.
	got = insertSteering("x.ts", content, "b\nc", "B\nc\nnew")
	if !strings.Contains(got, "anchor_line=1") || !strings.Contains(got, "to change lines 2-3,") {
		t.Fatalf("steering = %s", got)
	}
	if insertSteering("x.ts", content, "absent", "x") != "" {
		t.Fatal("steering for an absent old_string")
	}
}
