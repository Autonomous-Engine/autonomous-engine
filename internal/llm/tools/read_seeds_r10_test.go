package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// R10 W5: with a Where-to-edit seed at public/index.html:840, a read
// without a range opens at lines 640-1039 (the code to change is in it), not
// 1-400; an explicit range, a file without a seed and a short file are
// read as before. codex_read_file does the same.
func TestDefaultReadCentresOnTheSeed(t *testing.T) {
	// The 400-line window without R12 X1's char cap
	// (TestDefaultReadIsBoundedByChars covers the cap).
	t.Setenv("READ_RESULT_MAX_CHARS", "10000000")
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetReadSeeds(map[string]int{"public/index.html": 840, "src/config/env.ts": 5})
	read := func(name, args string) adapter.ToolResult {
		return ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: name, Arguments: args})
	}
	r := read("read_file", `{"path":"public/index.html"}`)
	if r.IsError || !strings.Contains(r.Content, "searchClient.addIndex('https://search.example.test/v1/books_index/") ||
		!strings.Contains(r.Content, "[lines 640-1039 of 1697") || !strings.Contains(r.Content, "around the Where-to-edit line 840") {
		t.Fatalf("seeded read: %s", tail(r.Content))
	}
	if strings.HasPrefix(r.Content, "<!DOCTYPE html>") {
		t.Fatal("seeded read started at line 1")
	}
	if strings.Count(r.Content, "[lines ") != 1 {
		t.Fatalf("hint twice: %s", tail(r.Content))
	}
	// The ledger pins the window that was read, not the whole file.
	l := newEvidenceLedger()
	l.enablePins([]string{"public/index.html"}, nil)
	l.recordRead(adapter.ToolCall{Name: "read_file", Arguments: `{"path":"public/index.html"}`}, r.Content)
	if p := l.lastRead["public/index.html"]; p == nil || p.start != 640 || p.end != 1039 {
		t.Fatalf("pinned %+v, want lines 640-1039", p)
	}
	// A seeded window reaching the end of the file still carries the hint.
	ex.SetReadSeeds(map[string]int{"landing/index.html": 1100})
	end := read("read_file", `{"path":"landing/index.html"}`)
	l.recordRead(adapter.ToolCall{Name: "read_file", Arguments: `{"path":"landing/index.html"}`}, end.Content)
	if p := l.lastRead["landing/index.html"]; p == nil || p.start != 727 || p.end != 1126 {
		t.Fatalf("pinned %+v, want lines 727-1126 (%s)", p, tail(end.Content))
	}
	ex.SetReadSeeds(map[string]int{"public/index.html": 840, "src/config/env.ts": 5})
	if r := read("read_file", `{"path":"public/index.html","start_line":1,"end_line":10}`); !strings.HasPrefix(r.Content, "<!DOCTYPE html>") {
		t.Fatalf("explicit range: %s", tail(r.Content))
	}
	if r := read("read_file", `{"path":"landing/index.html"}`); !strings.HasPrefix(r.Content, "<!DOCTYPE html>") || !strings.Contains(r.Content, "[lines 1-400 of 1126") {
		t.Fatalf("unseeded read: %s", tail(r.Content))
	}
	if r := read("read_file", `{"path":"src/config/env.ts"}`); strings.Contains(r.Content, "Where-to-edit") {
		t.Fatalf("short file: %s", r.Content)
	}
	c := read("codex_read_file", `{"path":"public/index.html"}`)
	var payload struct {
		Start   int    `json:"start_line"`
		End     int    `json:"end_line"`
		Note    string `json:"note"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(c.Content), &payload); err != nil {
		t.Fatalf("codex_read_file: %v %s", err, tail(c.Content))
	}
	if payload.Start != 640 || payload.End != 1039 || !strings.Contains(payload.Content, "books_index") || !strings.Contains(payload.Note, "Where-to-edit line 840") {
		t.Fatalf("codex_read_file window %d-%d note %q", payload.Start, payload.End, payload.Note)
	}
}

func tail(s string) string {
	if len(s) > 300 {
		return "…" + s[len(s)-300:]
	}
	return s
}
