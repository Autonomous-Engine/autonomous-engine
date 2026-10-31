package failures

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The testdata outputs were captured from real runs (go 1.22 in
// golang:1.22 and go 1.27 on Windows; pytest 9 on python:3.12-slim; tsc 5,
// vitest 1 and jest 29 on node:20-slim) of small failing projects; see
// the spec for the commands.

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type want struct {
	cat   Category
	file  string
	line  int
	test  string
	own   Ownership
	retry bool
	msg   string // substring
}

func TestParseCapturedOutputs(t *testing.T) {
	cases := []struct {
		file    string
		lang    string
		command string
		root    string
		want    []want
	}{
		{"go/go_build_compile.txt", "go", "go build ./...", "", []want{
			{CategoryCompile, "calc/calc.go", 4, "", OwnedChangedFile, true, "undefined: c"},
			{CategoryCompile, "calc/calc.go", 8, "", OwnedChangedFile, true, "cannot use a - b"},
		}},
		{"go/go_build_missing_module.txt", "go", "go build ./...", "", []want{
			{CategoryCompile, "calc/calc.go", 3, "", OwnedDependency, false, "cannot find module providing package"},
		}},
		{"go/go_test_assert.txt", "go", "go test ./... -count=1", "", []want{
			{CategoryTestAssert, "calc_test.go", 7, "TestAdd", OwnedChangedFile, true, "Add(2, 3) = -1, want 5"},
			{CategoryTestAssert, "calc_test.go", 18, "TestTable/pos", OwnedChangedFile, true, "got 0 want 2"},
		}},
		{"go/go_test_build_failed.txt", "go", "go test ./...", "", []want{
			{CategoryCompile, "calc/calc_test.go", 6, "", OwnedChangedFile, true, "undefined: Mul"},
		}},
		{"go/go_test_panic.txt", "go", "go test ./...", "/tmp/gocap/panic", []want{
			{CategoryTestPanic, "calc/calc.go", 3, "TestAt", OwnedChangedFile, true, "index out of range [3] with length 3"},
		}},
		{"go/go_test_timeout.txt", "go", "go test ./... -timeout 1s", "/tmp/gocap/timeout", []want{
			{CategoryTimeout, "calc/calc_test.go", 8, "TestSlow", OwnedChangedFile, true, "test timed out after 1s"},
		}},
		{"go/go_vet.txt", "go", "go vet ./...", "", []want{
			{CategoryLint, "calc/calc.go", 5, "", OwnedChangedFile, true, "format %s has arg n of wrong type int"},
		}},
		{"ts/tsc.txt", "typescript", "npx tsc -p .", "", []want{
			{CategoryTypecheck, "src/sum.ts", 5, "", OwnedChangedFile, true, "TS2322: Type 'string' is not assignable"},
		}},
		{"ts/tsc_pretty.txt", "", "npx tsc -p . --pretty", "", []want{
			{CategoryTypecheck, "src/sum.ts", 5, "", OwnedChangedFile, true, "TS2322"},
		}},
		{"ts/vitest.txt", "typescript", "npx vitest run", "", []want{
			{CategoryTestAssert, "src/sum.test.ts", 6, "sum > adds numbers", OwnedChangedFile, true, "expected -1 to be 3"},
			{CategoryTestPanic, "src/sum.test.ts", 10, "sum > throws on bad input", OwnedChangedFile, true, "TypeError: Cannot read properties of undefined"},
		}},
		{"ts/jest.txt", "", "npx jest --ci", "", []want{
			{CategoryCompile, "sum.test.js", 2, "", OwnedChangedFile, true, "Cannot find module './nope'"},
			{CategoryTestAssert, "other.test.js", 5, "sum > adds numbers", OwnedChangedFile, true, "expect(received).toBe(expected)"},
			{CategoryTestPanic, "other.test.js", 9, "sum > reads a property", OwnedChangedFile, true, "TypeError"},
		}},
		{"python/pytest_failures.txt", "python", "python -m pytest", "", []want{
			{CategoryTestAssert, "tests/test_calc.py", 5, "tests/test_calc.py::test_add", OwnedChangedFile, true, "assert -1 == 5"},
			{CategoryTestPanic, "calc.py", 6, "tests/test_calc.py::test_div", OwnedChangedFile, true, "ZeroDivisionError: division by zero"},
		}},
		{"python/pytest_collection_error.txt", "", "python -m pytest", "", []want{
			{CategoryCompile, "tests/test_imports.py", 1, "", OwnedDependency, false, "No module named 'missing_lib_zz'"},
		}},
		{"python/python_traceback.txt", "python", "python script.py", "/tmp/py", []want{
			{CategoryRuntime, "calc.py", 6, "", OwnedChangedFile, true, "ZeroDivisionError: division by zero"},
		}},
		{"python/python_syntax.txt", "python", "python bad_syntax.py", "/tmp/py", []want{
			{CategoryCompile, "bad_syntax.py", 1, "", OwnedChangedFile, true, "SyntaxError: invalid syntax"},
		}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			exit := 1
			got := Parse(c.lang, Result{Tool: "pre_commit_checks", Command: c.command, ExitCode: &exit, Output: load(t, c.file), Root: c.root, RawRef: "ref:1"})
			if len(got) != len(c.want) {
				for _, g := range got {
					t.Logf("got %s %s:%d %q %q", g.Category, g.File, g.Line, g.Test, g.Message)
				}
				t.Fatalf("%d records, want %d", len(got), len(c.want))
			}
			for i, w := range c.want {
				g := got[i]
				if g.Category != w.cat || g.File != w.file || g.Line != w.line || g.Test != w.test || g.Ownership != w.own || g.Retryable != w.retry || !strings.Contains(g.Message, w.msg) {
					t.Errorf("record %d = {%s %s:%d test=%q own=%s retry=%v msg=%q}, want {%s %s:%d test=%q own=%s retry=%v msg~%q}",
						i, g.Category, g.File, g.Line, g.Test, g.Ownership, g.Retryable, g.Message, w.cat, w.file, w.line, w.test, w.own, w.retry, w.msg)
				}
				if len(g.Fingerprint) != 64 {
					t.Errorf("record %d fingerprint %q is not a sha256 hex", i, g.Fingerprint)
				}
				if g.Command != c.command || g.Tool != "pre_commit_checks" || g.ExitCode == nil || *g.ExitCode != 1 || g.RawRef != "ref:1" {
					t.Errorf("record %d does not carry the result's tool/command/exit/raw ref: %+v", i, g)
				}
				if g.Span == "" {
					t.Errorf("record %d has no span", i)
				}
			}
		})
	}
}

// The same failure under two different temp directories (and two OSes)
// has the same fingerprint, with or without a root to relativise against.
func TestFingerprintStableAcrossTempDirs(t *testing.T) {
	linux := Parse("go", Result{Command: "go test ./...", Output: load(t, "go/go_test_panic.txt")})
	windows := Parse("go", Result{Command: "go test ./...", Output: load(t, "go/go_test_panic_windows.txt")})
	if len(linux) != 1 || len(windows) != 1 {
		t.Fatalf("records: %d / %d", len(linux), len(windows))
	}
	if linux[0].Fingerprint != windows[0].Fingerprint {
		t.Fatalf("fingerprints differ across temp dirs:\n %s %s\n %s %s", linux[0].Fingerprint, linux[0].File, windows[0].Fingerprint, windows[0].File)
	}

	// A different worktree, the same failure, relativised against each
	// root: identical records.
	out := load(t, "go/go_test_panic.txt")
	moved := strings.ReplaceAll(out, "/tmp/gocap/panic", "/var/lib/ae/worktrees/task-81f3")
	a := Parse("go", Result{Output: out, Root: "/tmp/gocap/panic"})
	b := Parse("go", Result{Output: moved, Root: "/var/lib/ae/worktrees/task-81f3"})
	if a[0].Fingerprint != b[0].Fingerprint || a[0].File != "calc/calc.go" || b[0].File != "calc/calc.go" {
		t.Fatalf("moved worktree: %s %s vs %s %s", a[0].File, a[0].Fingerprint, b[0].File, b[0].Fingerprint)
	}
	// Temp paths inside messages are stripped too.
	m1 := Record{Category: CategoryRuntime, Message: "open /tmp/TestX123/001/config.yaml: no such file or directory"}
	m2 := Record{Category: CategoryRuntime, Message: "open /tmp/TestX987/004/config.yaml: no such file or directory"}
	if Fingerprint(m1) != Fingerprint(m2) {
		t.Fatal("temp paths in messages change the fingerprint")
	}
}

func TestFingerprintIgnoresLinesNumbersAndAddresses(t *testing.T) {
	base := Record{Category: CategoryTestAssert, File: "calc_test.go", Line: 7, Message: "Add(2, 3) = -1, want 5 (0xc000018060)", Test: "TestAdd"}
	shifted := base
	shifted.Line, shifted.Col = 31, 4
	shifted.Message = "Add(7, 9) = -2, want 16 (0xc0000a2000)"
	if Fingerprint(base) != Fingerprint(shifted) {
		t.Fatal("line shift / numbers / addresses changed the fingerprint")
	}
	other := base
	other.Test = "TestAddNegative"
	if Fingerprint(base) == Fingerprint(other) {
		t.Fatal("different test names must fingerprint differently")
	}
	cat := base
	cat.Category = CategoryTestPanic
	if Fingerprint(base) == Fingerprint(cat) {
		t.Fatal("different categories must fingerprint differently")
	}
	file := base
	file.File = "other_test.go"
	if Fingerprint(base) == Fingerprint(file) {
		t.Fatal("different files must fingerprint differently")
	}
}

// Subtests fingerprint by their own name: TestTable/pos and TestTable/neg
// failing the same way are two failures.
func TestFingerprintDistinguishesSubtests(t *testing.T) {
	out := load(t, "go/go_test_assert.txt")
	neg := strings.ReplaceAll(out, "TestTable/pos", "TestTable/neg")
	a := Parse("go", Result{Output: out})
	b := Parse("go", Result{Output: neg})
	if a[1].Fingerprint == b[1].Fingerprint {
		t.Fatal("different subtest names produced the same fingerprint")
	}
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Fatal("TestAdd's fingerprint changed although TestAdd did not")
	}
}

func TestUnknownOutputIsOneRecordFingerprintedByItsPrefix(t *testing.T) {
	head := strings.Repeat("the build exploded in an unfamiliar way ", 6) // > 200 chars
	a := Parse("", Result{Command: "make check", Output: head + "\nrun id 1234 at 10:41"})
	b := Parse("", Result{Command: "make check", Output: head + "\nrun id 9876 at 11:02 with other trailing text"})
	if len(a) != 1 || a[0].Category != CategoryUnknown || len(b) != 1 {
		t.Fatalf("unknown output: %+v", a)
	}
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Fatal("outputs that share their first 200 normalised characters must share the unknown fingerprint")
	}
	c := Parse("", Result{Output: "something else entirely"})
	if c[0].Fingerprint == a[0].Fingerprint {
		t.Fatal("different unknown outputs share a fingerprint")
	}
	if a[0].Ownership != OwnedChangedFile || !a[0].Retryable {
		t.Fatalf("unknown record ownership/retryable = %s/%v", a[0].Ownership, a[0].Retryable)
	}
}

func TestToolingAndTimeout(t *testing.T) {
	recs := Parse("", Result{Command: "npx vitest run", Output: "sh: 1: vitest: not found\n"})
	if len(recs) != 1 || recs[0].Category != CategoryTooling || recs[0].Ownership != OwnedEnvironment || recs[0].Retryable {
		t.Fatalf("tooling: %+v", recs)
	}
	recs = Parse("go", Result{Command: "go test ./...", Output: "=== RUN TestSlow\n", TimedOut: true})
	if len(recs) != 1 || recs[0].Category != CategoryTimeout {
		t.Fatalf("timeout: %+v", recs)
	}
}

func TestTrackerRepeatedOnlyAfterARepair(t *testing.T) {
	r1 := Record{Fingerprint: "aaa", Category: CategoryTestAssert}
	r2 := Record{Fingerprint: "bbb", Category: CategoryCompile}
	tr := NewTracker()
	if o := tr.Observe([]Record{r1}, "c1", false); len(o.New) != 1 || len(o.Repeated) != 0 {
		t.Fatalf("run 1: %+v", o)
	}
	// Same failure again without a repair in between is not a repeat.
	if o := tr.Observe([]Record{r1}, "c2", false); len(o.Repeated) != 0 || len(o.Seen) != 1 {
		t.Fatalf("run 2: %+v", o)
	}
	if o := tr.Observe([]Record{r1, r2}, "c3", true); len(o.Repeated) != 1 || o.Repeated[0] != "aaa" || len(o.New) != 1 {
		t.Fatalf("run 3: %+v", o)
	}
	e, _ := tr.Entry("aaa")
	if e.Count != 3 || e.Consecutive != 3 || e.LastCallID != "c3" || e.FirstRun != 1 {
		t.Fatalf("entry: %+v", e)
	}
	// bbb disappears, then comes back after a repair: not consecutive.
	tr.Observe([]Record{r1}, "c4", true)
	if o := tr.Observe([]Record{r2}, "c5", true); len(o.Repeated) != 0 {
		t.Fatalf("non-consecutive repeat flagged: %+v", o)
	}
	if top := tr.Top(1); top[0].Record.Fingerprint != "aaa" || top[0].Count != 4 {
		t.Fatalf("top: %+v", top)
	}
}

func TestContradiction(t *testing.T) {
	planned := []string{"internal/calc/calc.go", "web/src/api.ts"}
	cases := []struct {
		r    Record
		want string
	}{
		{Record{Ownership: OwnedDependency, Message: "no required module provides package x"}, "dependency"},
		{Record{Ownership: OwnedEnvironment, Message: "sh: vitest: not found"}, "environment"},
		{Record{Ownership: OwnedChangedFile, File: "calc/calc.go", Message: "undefined: Multiply"}, "symbol"},
		{Record{Ownership: OwnedChangedFile, File: "web/src/page.ts", Message: "Cannot find module './api' from 'page.ts'"}, "not found"},
		{Record{Ownership: OwnedChangedFile, File: "calc/calc_test.go", Message: "got 1 want 2"}, ""},
		{Record{Ownership: OwnedChangedFile, File: "other/x.go", Message: "undefined: Foo"}, ""},
	}
	for i, c := range cases {
		got := Contradiction(c.r, planned)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("case %d: Contradiction = %q, want ~%q", i, got, c.want)
		}
	}
}

func TestFormatGroupedGroupsByFile(t *testing.T) {
	recs := Parse("go", Result{Output: load(t, "go/go_build_compile.txt")})
	recs = append(recs, Parse("go", Result{Output: load(t, "go/go_test_assert.txt")})...)
	s := FormatGrouped(recs, map[string]int{recs[0].Fingerprint: 2})
	if strings.Count(s, "### calc/calc.go") != 1 || strings.Count(s, "### calc_test.go") != 1 {
		t.Fatalf("not grouped by file:\n%s", s)
	}
	if !strings.Contains(s, Short(recs[0].Fingerprint)) || !strings.Contains(s, "failed in 2 runs") {
		t.Fatalf("missing fingerprint/count:\n%s", s)
	}
}
