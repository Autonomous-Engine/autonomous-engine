package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

func editCall(t *testing.T, args map[string]any) adapter.ToolCall {
	t.Helper()
	b, _ := json.Marshal(args)
	return adapter.ToolCall{ID: "e", Name: "edit_file", Arguments: string(b)}
}

func readTree(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// R10 W2: attempt 9's call 36 re-issued call 31's compose edit (new_string
// = old_string + one line). It used to apply again and duplicate the line;
// now it reports "already applied" and writes nothing.
func TestEditFileReapplyIsAlreadyApplied(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	first := runEdit(t, ex, attempt9ToolCall(t, 31, 0))
	if first.IsError {
		t.Fatalf("call-31 edit: %s", first.Content)
	}
	after := readTree(t, root, "docker-compose.yml")
	again := runEdit(t, ex, attempt9ToolCall(t, 36, 0))
	if again.IsError || !strings.Contains(again.Content, "already applied at line") || !strings.Contains(again.Content, "no change") {
		t.Fatalf("re-applied edit: %v %s", again.IsError, again.Content)
	}
	if readTree(t, root, "docker-compose.yml") != after {
		t.Fatal("the re-applied edit changed the file")
	}
	if n := strings.Count(after, "SEARCH_API_KEY="); n != 1 {
		t.Fatalf("SEARCH_API_KEY lines = %d, want 1", n)
	}
}

// old_string gone and new_string present once: already applied.
func TestEditFileReplacedTextPresentIsAlreadyApplied(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.ts"), []byte("a();\nconst x = computeOld();\nb();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(root)
	call := editCall(t, map[string]any{"path": "a.ts", "old_string": "const x = computeOld();", "new_string": "const x = computeNew();\nconst y = computeMore();"})
	if r := runEdit(t, ex, call); r.IsError {
		t.Fatal(r.Content)
	}
	r := runEdit(t, ex, call)
	if r.IsError || !strings.Contains(r.Content, "already applied at line 2") {
		t.Fatalf("got %v %s", r.IsError, r.Content)
	}
}

// R10 review: a single-line new_string found elsewhere, or an old_string
// whose text is still in the file (tabs vs spaces), is "not found" with
// hints, never a false "already applied" that writes nothing.
func TestEditFileNoFalseAlreadyApplied(t *testing.T) {
	root := t.TempDir()
	orig := "function f() {\n\tfoo();\n\tbar();\n}\n"
	if err := os.WriteFile(filepath.Join(root, "a.js"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(root)
	for _, c := range []map[string]any{
		{"path": "a.js", "old_string": "    foo();", "new_string": "\tbar();"},
		{"path": "a.js", "old_string": "\tfoo( )", "new_string": "}"},
		{"path": "a.js", "old_string": "    foo();\n    baz();", "new_string": "function f() {\n\tfoo();"},
	} {
		r := runEdit(t, ex, editCall(t, c))
		if !r.IsError || !strings.Contains(r.Content, "not found") {
			t.Errorf("%q -> %v %s", c["old_string"], r.IsError, r.Content)
		}
	}
	if readTree(t, root, "a.js") != orig {
		t.Fatal("file changed")
	}
}

// Not found: the nearest lines, with their line numbers.
func TestEditFileNotFoundNamesNearestLines(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	r := runEdit(t, ex, attempt9ToolCall(t, 10, 4)) // docs: "## Environment Variables"
	if !r.IsError || !strings.Contains(r.Content, "not found") {
		t.Fatalf("got %v %s", r.IsError, r.Content)
	}
	if !strings.Contains(r.Content, "3: ## Global Environment Variables") {
		t.Fatalf("no nearest line in:\n%s", r.Content)
	}
	// A guessed old_string with nothing similar says so.
	r = runEdit(t, ex, attempt9ToolCall(t, 10, 2)) // .env.example: "# Add your environment variables here"
	if !r.IsError || !strings.Contains(r.Content, "not found") {
		t.Fatalf("got %v %s", r.IsError, r.Content)
	}
}

// Success says which lines were written.
func TestEditFileSuccessNamesTheLines(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	r := runEdit(t, ex, attempt9ToolCall(t, 2, 1)) // src/server/index.ts: return { app };
	if r.IsError {
		t.Fatal(r.Content)
	}
	if !regexp.MustCompile(`Edit applied to src.server.index\.ts: replaced 1 occurrence at lines \d+-\d+`).MatchString(r.Content) {
		t.Fatalf("no line range: %s", r.Content)
	}
}

// near_line picks one of several matches.
func TestEditFileNearLinePicksAMatch(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	r := runEdit(t, ex, editCall(t, map[string]any{"path": "docker-compose.yml", "old_string": "environment:", "new_string": "environment:\n      - SEARCH_API_KEY=", "near_line": 17}))
	if r.IsError {
		t.Fatalf("near_line edit: %s", r.Content)
	}
	got := readTree(t, root, "docker-compose.yml")
	if strings.Count(got, "SEARCH_API_KEY=") != 1 {
		t.Fatal("not applied exactly once")
	}
	lines := strings.Split(got, "\n")
	at := -1
	for i, l := range lines {
		if strings.Contains(l, "SEARCH_API_KEY=") {
			at = i + 1
		}
	}
	if at < 10 || at > 20 {
		t.Fatalf("applied at line %d, want the match nearest line 17", at)
	}
	// Without near_line the ambiguity is reported, with the hint.
	r = runEdit(t, ex, editCall(t, map[string]any{"path": "docker-compose.yml", "old_string": "environment:", "new_string": "environment:\n      - X="}))
	if !r.IsError || !strings.Contains(r.Content, "near_line") {
		t.Fatalf("ambiguous edit: %v %s", r.IsError, r.Content)
	}
}

// Golden: an edit's ledger line keeps the path and the outcome class.
func TestLedgerLineForEdits(t *testing.T) {
	args := `{"new_string":"## Configuration\n\n- **SEARCH_API_KEY**: The search service's API key. Create one and set it.","old_string":"## Configuration","path":"README.md"}`
	cases := []struct {
		content string
		isErr   bool
		want    string
	}{
		{"Edit applied to README.md: replaced 1 occurrence at lines 12-14", false, `- edit_file(README.md, old:"## Configuration") -> applied L12-14`},
		{"Edit to README.md already applied at line 12 — no change (new_string is already there).", false, `- edit_file(README.md, old:"## Configuration") -> already-applied L12`},
		{"old_string not found in README.md. Nearest lines:\n  3: ## Config", true, `- edit_file(README.md, old:"## Configuration") -> not-found`},
		{"old_string found 15 times — must be unique.", true, `- edit_file(README.md, old:"## Configuration") -> ambiguous(15)`},
		{"REJECTED: edit would duplicate a block. new_string lines 2-9 …", true, `- edit_file(README.md, old:"## Configuration") -> rejected: dup-block`},
		{"REJECTED: edit introduces suspicious duplicates that did not exist in the original file.", true, `- edit_file(README.md, old:"## Configuration") -> rejected: duplicates`},
		{"edit rejected: it would leave README.md unparsable: …", true, `- edit_file(README.md, old:"## Configuration") -> rejected: syntax`},
	}
	for _, c := range cases {
		if got := ledgerLine("edit_file", args, c.content, c.isErr); got != c.want {
			t.Errorf("ledgerLine(%q)\n got %s\nwant %s", c.content, got, c.want)
		}
	}
}
