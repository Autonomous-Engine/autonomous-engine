package checks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// The targeted commands are verified against the real runners: each batch
// a plan selects is executed in the fixture exactly as the implementation
// agent's fast loop runs it (`sh -c <command>` in the repository root).
// The runners are installed once into a per-user cache directory keyed by
// the fixture's manifests (npm for vitest/jest/typescript; pip --target
// for pytest when it is not installed). The tests are opt-in: they run
// only with CHECKS_TOOLCHAIN_TESTS=1 (never under -short), and skip, with
// the reason, when a toolchain is missing or the install fails (offline).
// Every selection assertion that needs no toolchain lives in js_test.go,
// py_test.go and checks_test.go and always runs.

// toolchainEnv opts in to the real-toolchain tests: they need network on
// the first run, write runner installs to the user cache and take 40+ s,
// so a plain `go test ./...` never runs them.
const toolchainEnv = "CHECKS_TOOLCHAIN_TESTS"

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs real toolchains; skipped under -short")
	}
	if os.Getenv(toolchainEnv) != "1" {
		t.Skipf("runs real toolchains (npm/pip installs into the user cache, vitest, jest, node --test, tsc, pytest, ruff); set %s=1 to run", toolchainEnv)
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH: %v", tool, err)
		}
	}
}

func runSh(t *testing.T, dir, cmd string, env ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	return string(out), err
}

// fixtureCache returns a cache directory for the fixture keyed by the
// content of the files named in keyFiles.
func fixtureCache(t *testing.T, name, src string, keyFiles []string) string {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory: %v", err)
	}
	h := sha256.New()
	for _, f := range keyFiles {
		b, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(f)))
		h.Write([]byte(f))
		h.Write(b)
	}
	return filepath.Join(base, "ae-checks-fixtures", name+"-"+hex.EncodeToString(h.Sum(nil))[:12])
}

// installedNpmFixture syncs the npm workspace fixture into its cache
// directory (sources overwritten every run, node_modules kept) and
// installs its runners once.
func installedNpmFixture(t *testing.T) string {
	t.Helper()
	keys := []string{"package.json", "packages/lib/package.json", "packages/app/package.json", "packages/clock/package.json"}
	dir := fixtureCache(t, "npmws", npmFixture, keys)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cache dir: %v", err)
	}
	if err := copyDir(npmFixture, dir); err != nil {
		t.Fatal(err)
	}
	ready := true
	for _, bin := range []string{"vitest", "jest", "tsc"} {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", bin)); err != nil {
			ready = false
		}
	}
	if !ready {
		out, err := runSh(t, dir, "npm install --prefer-offline --no-audit --no-fund --ignore-scripts")
		if err != nil {
			t.Skipf("cannot install the fixture's runners (offline?): %v\n%s", err, out)
		}
	}
	return dir
}

// TestJSTargetedCommandsRunInFixture runs every batch the plan selects for
// seeded changes with the real vitest, jest, node --test, tsc and npm, and
// checks each one ran only the selected test files.
func TestJSTargetedCommandsRunInFixture(t *testing.T) {
	requireTools(t, "sh", "node", "npm")
	dir := installedNpmFixture(t)

	gate, err := runSh(t, dir, "npm test")
	if err != nil {
		t.Fatalf("the gate (npm test) fails in the fixture: %v\n%s", err, gate)
	}
	cases := []struct {
		changed  []string
		ran      []string // substrings each test batch output must show, by batch
		notRan   []string // test files that must not have run
		commands []string // executed commands, in order
	}{
		{
			changed: []string{"packages/lib/src/math.js"},
			ran:     []string{"greet.test.ts", "math.test.js"},
			notRan:  []string{"format.test.ts", "strings.test.js", "clock"},
			commands: []string{
				"npm test --workspace=packages/app -- src/greet.test.ts",
				"npm test --workspace=packages/lib -- src/math.test.js",
				"npx --no-install tsc --noEmit -p packages/app/tsconfig.json",
			},
		},
		{
			changed: []string{"packages/app/src/format.ts", "packages/lib/__tests__/strings.test.js"},
			ran:     []string{"format.test.ts", "strings.test.js"},
			notRan:  []string{"greet.test.ts", "math.test.js", "clock"},
			commands: []string{
				"npm test --workspace=packages/app -- src/format.test.ts",
				"npm test --workspace=packages/lib -- __tests__/strings.test.js",
				"npx --no-install tsc --noEmit -p packages/app/tsconfig.json",
				"npm run lint --workspace=packages/app",
			},
		},
		{
			changed:  []string{"packages/clock/src/clock.js"},
			ran:      []string{"hhmm"},
			notRan:   []string{"math.test.js", "greet.test.ts", "tick"},
			commands: []string{"npm test --workspace=packages/clock -- test/clock.test.js"},
		},
		{
			changed:  []string{"packages/clock/src/extra.js"},
			ran:      []string{"hhmm", "tick"},
			commands: []string{"npm test --workspace=packages/clock"},
		},
		// Literal filters: jest regex metacharacters escaped, every path
		// shell-quoted (the review's [id] / (group) / space cases).
		{
			changed: []string{"packages/lib/src/routes/[id].js", "packages/lib/src/(legacy)/old.js"},
			ran:     []string{"[id].test.js", "old.test.js", "Tests:       2 passed, 2 total", "greet.test.ts"},
			notRan:  []string{"math.test.js", "strings.test.js", "format.test.ts"},
			commands: []string{
				"npm test --workspace=packages/app -- src/greet.test.ts",
				`npm test --workspace=packages/lib -- 'src/\(legacy\)/old.test.js' 'src/routes/\[id\].test.js'`,
				"npx --no-install tsc --noEmit -p packages/app/tsconfig.json",
			},
		},
		{
			changed: []string{"packages/app/src/(group)/page.ts", "packages/clock/src/tick.js"},
			ran:     []string{"page.test.ts", "tick"},
			notRan:  []string{"greet.test.ts", "format.test.ts", "hhmm"},
			commands: []string{
				`npm test --workspace=packages/app -- 'src/(group)/page.test.ts'`,
				`npm test --workspace=packages/clock -- 'test/with space.test.js'`,
				"npx --no-install tsc --noEmit -p packages/app/tsconfig.json",
				"npm run lint --workspace=packages/app",
			},
		},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.changed, ","), func(t *testing.T) {
			plan := Select(context.Background(), dir, tc.changed, jsPolicy("npm test"), nil)
			if plan.Fallback != "" {
				t.Fatalf("fallback: %s", plan.Fallback)
			}
			var cmds []string
			var all strings.Builder
			for _, b := range plan.Batches() {
				cmds = append(cmds, b.Command)
				out, err := runSh(t, dir, b.Command)
				if err != nil {
					t.Fatalf("%s failed: %v\n%s", b.Command, err, out)
				}
				if b.Kind == evidence.CheckKindTest {
					all.WriteString(out)
				}
				t.Logf("$ %s\n%s", b.Command, lastLines(out, 8))
			}
			if strings.Join(cmds, "\n") != strings.Join(tc.commands, "\n") {
				t.Fatalf("commands = %q, want %q", cmds, tc.commands)
			}
			for _, s := range tc.ran {
				if !strings.Contains(all.String(), s) {
					t.Fatalf("the targeted runs did not run %s:\n%s", s, all.String())
				}
			}
			for _, s := range tc.notRan {
				if strings.Contains(all.String(), s) {
					t.Fatalf("the targeted runs ran %s, which the plan did not select:\n%s", s, all.String())
				}
			}
		})
	}

	// The filters really select: a failing test outside the selection does
	// not fail the targeted run, but fails the gate.
	t.Run("filters exclude unselected tests", func(t *testing.T) {
		broken := filepath.Join(dir, "packages", "lib", "__tests__", "strings.test.js")
		orig, _ := os.ReadFile(broken)
		defer os.WriteFile(broken, orig, 0o644)
		if err := os.WriteFile(broken, []byte("test('broken', () => { expect(1).toBe(2); });\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := runSh(t, dir, "npm test --workspace=packages/lib -- src/math.test.js"); err != nil {
			t.Fatalf("targeted jest run failed on an unselected test: %v\n%s", err, out)
		}
		if _, err := runSh(t, dir, "npm test --workspace=packages/lib"); err == nil {
			t.Fatal("the package's whole test script passed with a failing test")
		}
	})
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// pytestEnv returns extra environment making pytest importable: none when
// it is installed, else PYTHONPATH pointing at a cache directory it is
// pip-installed into once.
func pytestEnv(t *testing.T) []string {
	t.Helper()
	if _, err := runSh(t, ".", "python -m pytest --version"); err == nil {
		return nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("pytest is not installed and there is no cache directory: %v", err)
	}
	lib := filepath.Join(base, "ae-checks-fixtures", "pytest-lib")
	env := []string{"PYTHONPATH=" + lib}
	if _, err := runSh(t, ".", "python -m pytest --version", env...); err == nil {
		return env
	}
	out, err := runSh(t, ".", `python -m pip install --quiet --disable-pip-version-check --target "`+filepath.ToSlash(lib)+`" pytest`)
	if err != nil {
		t.Skipf("pytest is not installed and cannot be installed (offline?): %v\n%s", err, out)
	}
	return env
}

// collected matches a collected test (`-q`: file::test) or a file's count
// (`-qq`, as the fixture's addopts -q plus --collect-only -q give).
var collected = regexp.MustCompile(`(?m)^(tests/\S+?\.py)(?:::\S+|: \d+)`)

func collectedFiles(out string) []string {
	seen := map[string]bool{}
	var files []string
	for _, m := range collected.FindAllStringSubmatch(out, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			files = append(files, m[1])
		}
	}
	sort.Strings(files)
	return files
}

// TestPythonTargetedCommandsRunInFixture runs the selected pytest,
// compileall and ruff commands in a copy of the Python fixture, and checks
// with `pytest --collect-only -q` that the selected files collect exactly
// the expected tests, a subset of the gate's.
func TestPythonTargetedCommandsRunInFixture(t *testing.T) {
	requireTools(t, "sh", "python")
	if out, err := runSh(t, ".", "python -c 'import sys; assert sys.version_info >= (3, 8)'"); err != nil {
		t.Skipf("python is not Python 3.8+: %v\n%s", err, out)
	}
	env := pytestEnv(t)
	dir := copyTree(t, pyFixture)
	ruff := true
	if _, err := runSh(t, dir, "python -m ruff --version"); err != nil {
		ruff = false
		t.Log("ruff is not installed: the lint batch is not executed (its command is still asserted)")
	}

	gateOut, err := runSh(t, dir, "python -m pytest --collect-only -q", env...)
	if err != nil {
		t.Fatalf("gate collection failed: %v\n%s", err, gateOut)
	}
	gateIDs := collectedFiles(gateOut)
	if len(gateIDs) != 5 {
		t.Fatalf("gate collects tests from %d files, want 5:\n%s", len(gateIDs), gateOut)
	}
	cases := []struct {
		changed []string
		ids     []string
	}{
		{[]string{"src/calc/ops.py"}, []string{"tests/integration/test_flow.py", "tests/test_fmt.py", "tests/test_ops.py", "tests/test_square.py"}},
		{[]string{"src/calc/util.py"}, []string{"tests/test_util.py"}},
		{[]string{"tests/integration/conftest.py"}, []string{"tests/integration/test_flow.py"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.changed, ","), func(t *testing.T) {
			plan := Select(context.Background(), dir, tc.changed, jsPolicy("python -m pytest"), nil)
			if plan.Fallback != "" {
				t.Fatalf("fallback: %s", plan.Fallback)
			}
			for _, b := range plan.Batches() {
				if b.Kind == evidence.CheckKindLint && !ruff {
					continue
				}
				out, err := runSh(t, dir, b.Command, env...)
				if err != nil {
					t.Fatalf("%s failed: %v\n%s", b.Command, err, out)
				}
				t.Logf("$ %s\n%s", b.Command, lastLines(out, 4))
				if b.Kind != evidence.CheckKindTest {
					continue
				}
				out, err = runSh(t, dir, b.Command+" --collect-only -q", env...)
				if err != nil {
					t.Fatalf("collect-only: %v\n%s", err, out)
				}
				ids := collectedFiles(out)
				if strings.Join(ids, " ") != strings.Join(tc.ids, " ") {
					t.Fatalf("selected files collect %q, want %q", ids, tc.ids)
				}
				for _, id := range ids {
					if !containsString(gateIDs, id) {
						t.Fatalf("%s is not collected by the gate: the fast set must be a subset", id)
					}
				}
			}
		})
	}

	// compileall keeps its bytecode out of the tree (PYTHONPYCACHEPREFIX).
	clean := copyTree(t, pyFixture)
	plan := Select(context.Background(), clean, []string{"src/calc/ops.py"}, jsPolicy("python -m pytest"), nil)
	for _, b := range plan.Batches() {
		if b.Kind == evidence.CheckKindBuild {
			if out, err := runSh(t, clean, b.Command, env...); err != nil {
				t.Fatalf("%s: %v\n%s", b.Command, err, out)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(clean, "src", "calc", "__pycache__")); err == nil {
		t.Fatal("compileall wrote __pycache__ into the tree")
	}

	// compileall really compiles: a syntax error fails the build target.
	write(t, dir, "src/calc/ops.py", "def add(a, b)\n    return a + b\n")
	plan = Select(context.Background(), dir, []string{"src/calc/ops.py"}, jsPolicy("python -m pytest"), nil)
	for _, b := range plan.Batches() {
		if b.Kind == evidence.CheckKindBuild {
			if out, err := runSh(t, dir, b.Command, env...); err == nil {
				t.Fatalf("%s passed on a syntax error:\n%s", b.Command, out)
			}
			return
		}
	}
	t.Fatal("no build target selected")
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
