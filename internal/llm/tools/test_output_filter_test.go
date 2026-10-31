package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func checkFiltered(t *testing.T, name, out string, want, banned []string, maxChars int) {
	t.Helper()
	t.Logf("%s: filtered to %d chars:\n%s", name, len(out), out)
	if maxChars > 0 && len(out) > maxChars {
		t.Errorf("%s: %d chars, want <= %d", name, len(out), maxChars)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s: lacks %q", name, w)
		}
	}
	for _, b := range banned {
		if strings.Contains(out, b) {
			t.Errorf("%s: still contains %q", name, b)
		}
	}
}

// T5: a synthetic `npm test` (vitest) run of the made-up sample-app, shaped
// like live-run attempt 6's output: 9,367 chars, mostly pino JSON log
// lines and passing tests around one failure.
func TestFilterVitestOutput(t *testing.T) {
	raw := fixture(t, "vitest_npm_test.log")
	out, ok := shapeRunOutput("npm test", raw, false)
	if !ok {
		t.Fatal("not filtered")
	}
	checkFiltered(t, "vitest", out, []string{
		"× Bookshelf API > GET /api/settings > returns reader settings with searchApiKey",
		"AssertionError: expected 404 to be 200",
		"tests/unit/server.test.ts:25:26",
		"Test Files  1 failed | 27 passed (28)",
		"Tests  1 failed | 223 passed (224)",
		"[exit code: exit status 1]",
		"lines; rerun with raw=true to see all)",
	}, []string{`"level":30`, "✓ tests/unit/isbn.test.ts", "\x1b["}, 2500)
	if len(out) > len(raw)/3 {
		t.Errorf("kept %d of %d chars", len(out), len(raw))
	}
}

func TestFilterGoTestOutput(t *testing.T) {
	raw := fixture(t, "go_test_v.log") + "\n[exit code: exit status 1]"
	out, ok := shapeRunOutput("go test -v ./...", raw, false)
	if !ok {
		t.Fatal("not filtered")
	}
	checkFiltered(t, "go test", out, []string{
		"calc_test.go:12: Add(2, 3) = -1, want 5", "--- FAIL: TestAddPositive",
		"calc_test.go:21: case 17: unexpected zero value", "--- FAIL: TestMany/case_17",
		"FAIL\texample.com/calc", "[exit code: exit status 1]", "rerun with raw=true",
	}, []string{"--- PASS", "=== RUN", "checking case 5"}, 1500)
}

func TestFilterJestOutput(t *testing.T) {
	raw := fixture(t, "jest_fail.log")
	out, ok := shapeRunOutput("npx jest", raw, false)
	if !ok {
		t.Fatal("not filtered")
	}
	checkFiltered(t, "jest", out, []string{
		"FAIL src/routes/cart.test.js", "● Cart API › POST /cart/items › rejects a negative quantity",
		"Expected: 400", "Received: 201", "src/routes/cart.test.js:43:24",
		"Tests:       1 failed, 311 passed, 312 total", "Test Suites: 1 failed, 27 passed, 28 total",
	}, []string{"PASS src/utils/money.test.js", `"level":"info"`}, 2500)
}

// run_command applies the filter to a real `go test -v` run, and raw=true
// returns the whole output.
func TestRunCommandFiltersARealTestRun(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	var src strings.Builder
	src.WriteString("package calc\n\nimport \"testing\"\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&src, "func TestPass%02d(t *testing.T) { t.Log(\"fine %d\") }\n", i, i)
	}
	src.WriteString("func TestBroken(t *testing.T) { t.Fatal(\"BROKEN-MARKER: want 5, got -1\") }\n")
	root := writeTree(t, map[string]string{"go.mod": "module example.com/calc\n\ngo 1.21\n", "calc_test.go": src.String()})
	ex := NewExecutor(root)
	r := ex.runCommand(context.Background(), "c1", map[string]interface{}{"command": "go test -v ./...", "timeout_seconds": float64(120)})
	if !strings.Contains(r.Content, "BROKEN-MARKER") || !strings.Contains(r.Content, "rerun with raw=true") || strings.Contains(r.Content, "--- PASS: TestPass07") {
		t.Fatalf("filtered run:\n%s", r.Content)
	}
	if r.ExitCode == nil || *r.ExitCode == 0 {
		t.Fatalf("exit code = %v", r.ExitCode)
	}
	full := ex.runCommand(context.Background(), "c2", map[string]interface{}{"command": "go test -v ./...", "raw": true, "timeout_seconds": float64(120)})
	if !strings.Contains(full.Content, "--- PASS: TestPass07") || strings.Contains(full.Content, "rerun with raw=true") {
		t.Fatalf("raw run:\n%s", full.Content)
	}
}

// R6 review: with 30 failures every failing test's name survives (only the
// block bodies are capped), and an assertion line identical across
// failures is kept under each failure whose body fits.
func TestFilterKeepsEveryFailureName(t *testing.T) {
	var b strings.Builder
	b.WriteString("\n> app@1.0.0 test\n> vitest run\n\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, `{"level":30,"msg":"noise %d"}`+"\n", i)
	}
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, " FAIL  tests/api.test.ts > api > case %02d returns 200\n", i)
		b.WriteString("AssertionError: expected 404 to be 200 // Object.is equality\n")
		for k := 0; k < 8; k++ {
			fmt.Fprintf(&b, "     %d|   expect(res.status).toBe(200); // frame %d.%d\n", 20+k, i, k)
		}
		b.WriteString("\n")
	}
	b.WriteString(" Test Files  1 failed (1)\n      Tests  30 failed | 4 passed (34)\n\n[exit code: exit status 1]")
	out, ok := shapeRunOutput("npm test", b.String(), false)
	if !ok {
		t.Fatal("not filtered")
	}
	for i := 1; i <= 30; i++ {
		if name := fmt.Sprintf("case %02d returns 200", i); !strings.Contains(out, name) {
			t.Errorf("failure %d's name was dropped", i)
		}
	}
	if n := strings.Count(out, "AssertionError: expected 404 to be 200"); n < 15 {
		t.Errorf("the shared assertion line appears %d times, want it under each failure whose body fits (>= 15)", n)
	}
	if !strings.Contains(out, "Tests  30 failed | 4 passed (34)") {
		t.Error("totals dropped")
	}
}

// raw=true, a non-test command and short output pass through unchanged.
func TestFilterLeavesOtherOutputAlone(t *testing.T) {
	raw := fixture(t, "vitest_npm_test.log")
	if out, ok := shapeRunOutput("npm test", raw, true); ok || out != raw {
		t.Error("raw=true was filtered")
	}
	if out, ok := shapeRunOutput("cat build.log", raw, false); ok || out != raw {
		t.Error("a non-test command was filtered")
	}
	short := "ok  \texample.com/calc\t0.1s\n"
	if out, ok := shapeRunOutput("go test ./...", short, false); ok || out != short {
		t.Error("short output was filtered")
	}
	// A piped test run is still a test run.
	if _, ok := shapeRunOutput("npm test 2>&1 | tail -200", raw, false); !ok {
		t.Error("piped npm test not filtered")
	}
}
