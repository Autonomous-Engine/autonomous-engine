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

func numberedFile(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// T6: a read without a range returns the first 400 lines and says how to
// continue (attempt 6 read an 11.6k-char file whole); an explicit range
// reads any slice.
func TestReadFileCapsAWholeFileRead(t *testing.T) {
	root := writeTree(t, map[string]string{"big.ts": numberedFile(1000), "small.ts": numberedFile(12)})
	ex := NewExecutor(root)
	read := func(args string) string {
		r := ex.Execute(context.Background(), adapter.ToolCall{ID: "1", Name: "read_file", Arguments: args})
		if r.IsError {
			t.Fatalf("read_file %s: %s", args, r.Content)
		}
		return r.Content
	}
	got := read(`{"path":"big.ts"}`)
	if !strings.HasPrefix(got, "line 1\n") || !strings.Contains(got, "line 400\n") || strings.Contains(got, "line 401\n") {
		t.Fatalf("default read is not lines 1-400:\n%.200s … %s", got, got[len(got)-200:])
	}
	if !strings.Contains(got, "[lines 1-400 of 1000; read_file with start_line=401 for more]") {
		t.Fatalf("no continuation hint: %s", got[len(got)-150:])
	}
	got = read(`{"path":"big.ts","start_line":401,"end_line":1000}`)
	if !strings.HasPrefix(got, "line 401\n") || !strings.Contains(got, "line 1000") || strings.Contains(got, "for more]") {
		t.Fatalf("explicit range:\n%.100s … %s", got, got[len(got)-100:])
	}
	got = read(`{"path":"big.ts","start_line":990}`)
	if !strings.HasPrefix(got, "line 990\n") || !strings.Contains(got, "line 1000") {
		t.Fatalf("open-ended range: %q", got)
	}
	if got := read(`{"path":"small.ts"}`); got != numberedFile(12) {
		t.Fatalf("a short file is returned as is: %q", got)
	}
	// A ranged read still satisfies read-before-edit.
	if r := ex.Execute(context.Background(), adapter.ToolCall{ID: "2", Name: "edit_file", Arguments: `{"path":"big.ts","old_string":"line 999\n","new_string":"line 999 edited\n"}`}); r.IsError {
		t.Fatalf("edit after a ranged read: %s", r.Content)
	}
}

// R6 review: after a default 400-line read of a 900-line file, write_file
// would replace the file with what the model saw and drop lines 401-900.
// write_file on an existing file now needs a read of the whole file.
func TestWriteFileNeedsAFullRead(t *testing.T) {
	exec := func(ex *Executor, name, args string) (string, bool) {
		r := ex.Execute(context.Background(), adapter.ToolCall{ID: "1", Name: name, Arguments: args})
		return r.Content, r.IsError
	}
	write := `{"path":"big.ts","content":"line 1\n"}`

	root := writeTree(t, map[string]string{"big.ts": numberedFile(900), "small.ts": numberedFile(12)})
	ex := NewExecutor(root)
	exec(ex, "read_file", `{"path":"big.ts"}`)
	msg, isErr := exec(ex, "write_file", write)
	if !isErr || !strings.Contains(msg, "file was read partially (lines 1-400 of 900); use edit_file, or read the rest first") {
		t.Fatalf("write_file after a partial read: %v %q", isErr, msg)
	}
	// edit_file is unaffected (exact search/replace).
	if msg, isErr := exec(ex, "edit_file", `{"path":"big.ts","old_string":"line 850\n","new_string":"line 850 edited\n"}`); isErr {
		t.Fatalf("edit_file after a partial read: %s", msg)
	}
	// Reading the rest makes the coverage whole.
	exec(ex, "read_file", `{"path":"big.ts","start_line":401}`)
	if msg, isErr := exec(ex, "write_file", write); isErr {
		t.Fatalf("write_file after reading the rest: %s", msg)
	}

	ex = NewExecutor(root)
	if _, isErr := exec(ex, "read_file", `{"path":"small.ts"}`); isErr {
		t.Fatal("read small.ts")
	}
	if msg, isErr := exec(ex, "write_file", `{"path":"small.ts","content":"x\n"}`); isErr {
		t.Fatalf("write_file after a full default read: %s", msg)
	}
	if err := os.WriteFile(filepath.Join(root, "big2.ts"), []byte(numberedFile(900)), 0o644); err != nil {
		t.Fatal(err)
	}
	exec(ex, "read_file", `{"path":"big2.ts","start_line":1,"end_line":900}`)
	if msg, isErr := exec(ex, "write_file", `{"path":"big2.ts","content":"x\n"}`); isErr {
		t.Fatalf("write_file after an explicit full read: %s", msg)
	}
	exec(ex, "read_file", `{"path":"new.ts"}`)
	if msg, isErr := exec(ex, "write_file", `{"path":"new.ts","content":"x\n"}`); isErr {
		t.Fatalf("write_file of a new file: %s", msg)
	}
	// A partial codex_read_file counts the same.
	ex.SetCodexReadsFromWorktree(true)
	if err := os.WriteFile(filepath.Join(root, "big3.ts"), []byte(numberedFile(900)), 0o644); err != nil {
		t.Fatal(err)
	}
	exec(ex, "codex_read_file", `{"path":"big3.ts"}`)
	if msg, isErr := exec(ex, "write_file", `{"path":"big3.ts","content":"x\n"}`); !isErr || !strings.Contains(msg, "read partially") {
		t.Fatalf("write_file after a partial codex_read_file: %v %q", isErr, msg)
	}
}

// R6 review: currentLines (pinned reads) never reads outside the tree, a
// sibling directory sharing the name prefix included.
func TestCurrentLinesStaysInTheTree(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	sibling := filepath.Join(parent, "repo-x")
	for _, d := range []string{repo, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.ts"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret.ts"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(repo)
	if got, ok := ex.currentLines("a.ts", 1, 0); !ok || !strings.Contains(got, "a") {
		t.Fatalf("in-tree read: %q %v", got, ok)
	}
	for _, p := range []string{"../repo-x/secret.ts", "../repo/../repo-x/secret.ts"} {
		if got, ok := ex.currentLines(p, 1, 0); ok {
			t.Fatalf("currentLines(%q) read outside the tree: %q", p, got)
		}
	}
}

func TestCodexReadFileCapsAWholeFileRead(t *testing.T) {
	root := writeTree(t, map[string]string{"big.ts": numberedFile(1000)})
	ex := NewExecutor(root)
	ex.SetCodexReadsFromWorktree(true)
	r := ex.Execute(context.Background(), adapter.ToolCall{ID: "1", Name: "codex_read_file", Arguments: `{"path":"big.ts"}`})
	var got struct {
		Start, End, Total int
		Content, Note     string
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.Content), &raw); err != nil {
		t.Fatalf("%v: %s", err, r.Content)
	}
	_ = json.Unmarshal(raw["start_line"], &got.Start)
	_ = json.Unmarshal(raw["end_line"], &got.End)
	_ = json.Unmarshal(raw["total_lines"], &got.Total)
	_ = json.Unmarshal(raw["note"], &got.Note)
	if got.Start != 1 || got.End != 400 || got.Total != 1000 || !strings.Contains(got.Note, "start_line=401") {
		t.Fatalf("codex_read_file default = %+v", got)
	}
}
