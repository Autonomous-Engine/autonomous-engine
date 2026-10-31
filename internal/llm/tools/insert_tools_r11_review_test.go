package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R11 review: append_to_file is idempotent (a 5-line block appended twice
// lands once), and so is a one-line insertion at the top of the file; a
// short line (`}`) may still repeat.
func TestInsertionsAreIdempotent(t *testing.T) {
	ex, root := insertFixture(t, "s.test.ts", "import { a } from './a';\n\ndescribe('x', () => {});\n")
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(root, "s.test.ts"))
		return string(b)
	}
	run(ex, "read_file", map[string]any{"path": "s.test.ts"})
	block := "describe('y', () => {\n  it('works', () => {\n    expect(1).toBe(1);\n  });\n});"
	for i := 0; i < 2; i++ {
		r := run(ex, "append_to_file", map[string]any{"path": "s.test.ts", "text": block})
		if r.IsError {
			t.Fatalf("append %d: %s", i+1, r.Content)
		}
		if i == 1 && (!strings.Contains(r.Content, "already applied at line 4") || strings.Contains(r.Content, "new_string")) {
			t.Fatalf("second append: %s", r.Content)
		}
	}
	if strings.Count(read(), "describe('y'") != 1 {
		t.Fatalf("appended twice:\n%s", read())
	}
	imp := "import { config } from './config';"
	for i := 0; i < 2; i++ {
		if r := run(ex, "insert_after", map[string]any{"path": "s.test.ts", "anchor_line": 0, "text": imp}); r.IsError {
			t.Fatalf("insert %d: %s", i+1, r.Content)
		} else if i == 0 && !strings.Contains(r.Content, "Inserted 1 line into") {
			t.Fatalf("one-line result: %s", r.Content)
		}
	}
	if strings.Count(read(), imp) != 1 {
		t.Fatalf("import inserted twice:\n%s", read())
	}
	// A short line may repeat.
	run(ex, "insert_after", map[string]any{"path": "s.test.ts", "anchor_line": 1, "text": ""})
	before := strings.Count(read(), "\n")
	run(ex, "insert_after", map[string]any{"path": "s.test.ts", "anchor_line": 1, "text": "// x"})
	run(ex, "insert_after", map[string]any{"path": "s.test.ts", "anchor_line": 1, "text": "// x"})
	if got := strings.Count(read(), "\n"); got != before+2 {
		t.Fatalf("short line: %d newlines, want %d", got, before+2)
	}
}

// R11 review: insertions and edits in a CRLF file keep CRLF endings.
func TestInsertKeepsCRLF(t *testing.T) {
	ex, root := insertFixture(t, "c.ts", "const a = 1;\r\nconst b = 2;\r\nconst c = 3;\r\n")
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(root, "c.ts"))
		return string(b)
	}
	run(ex, "read_file", map[string]any{"path": "c.ts"})
	if r := run(ex, "insert_after", map[string]any{"path": "c.ts", "anchor_line": 1, "text": "const x = 9;\nconst y = 8;"}); r.IsError {
		t.Fatal(r.Content)
	}
	if r := run(ex, "append_to_file", map[string]any{"path": "c.ts", "text": "export { a };"}); r.IsError {
		t.Fatal(r.Content)
	}
	if r := run(ex, "edit_file", map[string]any{"path": "c.ts", "old_string": "const b = 2;", "new_string": "const b = 2;\nconst b2 = 22;"}); r.IsError {
		t.Fatal(r.Content)
	}
	want := "const a = 1;\r\nconst x = 9;\r\nconst y = 8;\r\nconst b = 2;\r\nconst b2 = 22;\r\nconst c = 3;\r\nexport { a };\r\n"
	if got := read(); got != want {
		t.Fatalf("file = %q\nwant  %q", got, want)
	}
}

// R11 review: after an insertion the read ranges below it move down and the
// inserted lines count as read.
func TestInsertShiftsReadRanges(t *testing.T) {
	ex, _ := insertFixture(t, "a.ts", numbered(600))
	run(ex, "read_file", map[string]any{"path": "a.ts", "start_line": 100, "end_line": 120})
	run(ex, "read_file", map[string]any{"path": "a.ts", "start_line": 300, "end_line": 310})
	if r := run(ex, "insert_after", map[string]any{"path": "a.ts", "anchor_line": 110, "text": "const n1 = 1;\nconst n2 = 2;\nconst n3 = 3;"}); r.IsError {
		t.Fatal(r.Content)
	}
	c := ex.partialRead("a.ts")
	if c == nil || c.String() != "lines 100-123, 303-313 of 603" {
		t.Fatalf("coverage = %v", c)
	}
	if ok, _ := ex.readCovers("a.ts", 313); !ok {
		t.Fatal("the shifted range is not covered")
	}
}
