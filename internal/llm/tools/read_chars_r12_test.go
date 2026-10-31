package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// longLineHTML is an HTML-like file of n lines of about 120 chars each:
// 400 of them are about 48k chars, the shape of the long-line HTML page a
// live run read (attempt 12: a 400-line window was about 15k chars).
func longLineHTML(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "    <div class=\"row-%04d\" data-cover=\"https://covers.example.test/%04d/{size}/{id}-front.jpg\">line %d</div>\n", i, i, i)
	}
	return b.String()
}

var hintRangeRE = regexp.MustCompile(`\[lines (\d+)-(\d+) of (\d+)`)

// hintRange is the "[lines a-b of n" range of a read result.
func hintRange(t *testing.T, s string) (int, int, int) {
	t.Helper()
	m := hintRangeRE.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no range hint: %s", tail(s))
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	n, _ := strconv.Atoi(m[3])
	return a, b, n
}

// bodyOf is a read_file result without its trailing hint line.
func bodyOf(s string) string {
	if k := strings.LastIndex(s, "\n[lines "); k >= 0 {
		return s[:k+1]
	}
	return s
}

// R12 X1: a default read of a long-line file is bounded by chars as well
// as lines (READ_RESULT_MAX_CHARS, default 4,800), and the hint names the
// next line.
func TestDefaultReadIsBoundedByChars(t *testing.T) {
	root := writeTree(t, map[string]string{"public/index.html": longLineHTML(1000), "short.ts": numberedFile(1000)})
	ex := NewExecutor(root)
	read := func(name, args string) adapter.ToolResult {
		r := ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: name, Arguments: args})
		if r.IsError {
			t.Fatalf("%s %s: %s", name, args, r.Content)
		}
		return r
	}
	r := read("read_file", `{"path":"public/index.html"}`)
	a, b, n := hintRange(t, r.Content)
	body := bodyOf(r.Content)
	if a != 1 || n != 1000 || b >= 400 || b < 10 {
		t.Fatalf("window %d-%d of %d", a, b, n)
	}
	if len(body) > defaultReadResultMaxChars {
		t.Fatalf("body %d chars > %d", len(body), defaultReadResultMaxChars)
	}
	if !strings.Contains(r.Content, fmt.Sprintf("read_file with start_line=%d for more]", b+1)) {
		t.Fatalf("hint does not name the next line: %s", tail(r.Content))
	}
	if strings.Count(body, "\n") != b {
		t.Fatalf("body has %d lines, hint says %d", strings.Count(body, "\n"), b)
	}
	// Short lines: the 400-line window is under the cap and unchanged.
	if s := read("read_file", `{"path":"short.ts"}`); !strings.Contains(s.Content, "[lines 1-400 of 1000; read_file with start_line=401 for more]") {
		t.Fatalf("short-line file: %s", tail(s.Content))
	}
	// An explicit range is not capped.
	if e := read("read_file", `{"path":"public/index.html","start_line":1,"end_line":300}`); !strings.Contains(e.Content, "line 300</div>") {
		t.Fatalf("explicit range was capped: %s", tail(e.Content))
	}
	// The env var sets the cap.
	t.Setenv("READ_RESULT_MAX_CHARS", "2000")
	r = read("read_file", `{"path":"public/index.html"}`)
	if body := bodyOf(r.Content); len(body) > 2000 || len(body) < 1500 {
		t.Fatalf("READ_RESULT_MAX_CHARS=2000: body %d chars", len(body))
	}
}

// A seeded (Where-to-edit) read keeps the seed inside the char-bounded
// window, centred.
func TestSeededReadKeepsTheSeedCentred(t *testing.T) {
	root := writeTree(t, map[string]string{"public/index.html": longLineHTML(1000)})
	ex := NewExecutor(root)
	ex.SetCodexReadsFromWorktree(true)
	ex.SetReadSeeds(map[string]int{"public/index.html": 700})
	r := ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "read_file", Arguments: `{"path":"public/index.html"}`})
	a, b, _ := hintRange(t, r.Content)
	if a > 700 || b < 700 {
		t.Fatalf("seed 700 outside the window %d-%d", a, b)
	}
	if d := (700 - a) - (b - 700); d < -1 || d > 1 {
		t.Fatalf("window %d-%d is not centred on 700", a, b)
	}
	if body := bodyOf(r.Content); len(body) > defaultReadResultMaxChars || !strings.Contains(body, "line 700</div>") {
		t.Fatalf("seeded body %d chars", len(body))
	}
	if !strings.Contains(r.Content, "around the Where-to-edit line 700") || !strings.Contains(r.Content, fmt.Sprintf("start_line=%d for more]", b+1)) {
		t.Fatalf("seeded note: %s", tail(r.Content))
	}
	// A seed near the top grows the window downwards.
	ex.SetReadSeeds(map[string]int{"public/index.html": 3})
	r = ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "read_file", Arguments: `{"path":"public/index.html"}`})
	if a, b, _ := hintRange(t, r.Content); a != 1 || b < 30 {
		t.Fatalf("seed 3: window %d-%d", a, b)
	}

	// codex_read_file is bounded the same way.
	ex.SetReadSeeds(map[string]int{"public/index.html": 700})
	c := ex.Execute(context.Background(), adapter.ToolCall{ID: "c", Name: "codex_read_file", Arguments: `{"path":"public/index.html"}`})
	var p struct {
		Start   int    `json:"start_line"`
		End     int    `json:"end_line"`
		Content string `json:"content"`
		Note    string `json:"note"`
	}
	if err := json.Unmarshal([]byte(c.Content), &p); err != nil {
		t.Fatal(err)
	}
	if p.Start > 700 || p.End < 700 || len(p.Content) > defaultReadResultMaxChars || !strings.Contains(p.Note, "Where-to-edit line 700") {
		t.Fatalf("codex_read_file seeded %d-%d, %d chars, note %q", p.Start, p.End, len(p.Content), p.Note)
	}
	ex.SetReadSeeds(nil)
	c = ex.Execute(context.Background(), adapter.ToolCall{ID: "c", Name: "codex_read_file", Arguments: `{"path":"public/index.html"}`})
	if err := json.Unmarshal([]byte(c.Content), &p); err != nil {
		t.Fatal(err)
	}
	if p.Start != 1 || p.End >= 400 || len(p.Content) > defaultReadResultMaxChars || !strings.Contains(p.Note, fmt.Sprintf("start_line=%d for more", p.End+1)) {
		t.Fatalf("codex_read_file default %d-%d, %d chars, note %q", p.Start, p.End, len(p.Content), p.Note)
	}
}

// One line longer than the cap is still returned (the window is never
// empty).
func TestOverlongSingleLineIsReturned(t *testing.T) {
	long := strings.Repeat("x", 9000) + "\n"
	root := writeTree(t, map[string]string{"min.js": long + long + long})
	ex := NewExecutor(root)
	r := ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "read_file", Arguments: `{"path":"min.js"}`})
	if a, b, _ := hintRange(t, r.Content); a != 1 || b != 1 {
		t.Fatalf("window %d-%d", a, b)
	}
}
