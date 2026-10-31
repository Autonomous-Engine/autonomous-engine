package checks

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
)

const pyFixture = "testdata/pypkg"

func pyBuild(files string) want {
	return want{evidence.CheckKindBuild, compileCmd(files), "changed Python file(s) must still compile"}
}

func pyLint(files string) want {
	return want{evidence.CheckKindLint, "python -m ruff check " + files, "configured linter"}
}

// TestSelectPythonPackage is the M3-05 proof on the Python fixture: a src/
// layout package whose tests/ are mapped by the import graph, by name and
// by conftest scope.
func TestSelectPythonPackage(t *testing.T) {
	const pytest = "python -m pytest"
	cases := []struct {
		name    string
		changed []string
		want    []want
	}{
		{
			name:    "module: direct and transitive importers (relative import, sibling helper)",
			changed: []string{"src/calc/ops.py"},
			want: []want{
				{evidence.CheckKindTest, pytest + " tests/integration/test_flow.py", "test tests/integration/test_flow.py is affected by changed module src/calc/ops.py (via src/calc/fmt.py); policy requires test"},
				{evidence.CheckKindTest, pytest + " tests/test_fmt.py", "affected by changed module src/calc/ops.py (via src/calc/fmt.py)"},
				{evidence.CheckKindTest, pytest + " tests/test_ops.py", "test tests/test_ops.py imports changed module src/calc/ops.py"},
				{evidence.CheckKindTest, pytest + " tests/test_square.py", "affected by changed module src/calc/ops.py (via tests/helpers.py)"},
				pyBuild("src/calc/ops.py"),
				pyLint("src/calc/ops.py"),
			},
		},
		{
			name:    "from package import module",
			changed: []string{"src/calc/fmt.py"},
			want: []want{
				{evidence.CheckKindTest, pytest + " tests/integration/test_flow.py", "imports changed module src/calc/fmt.py"},
				{evidence.CheckKindTest, pytest + " tests/test_fmt.py", "imports changed module src/calc/fmt.py"},
				pyBuild("src/calc/fmt.py"),
				pyLint("src/calc/fmt.py"),
			},
		},
		{
			name:    "dynamically loaded module: the test named after it",
			changed: []string{"src/calc/util.py"},
			want: []want{
				{evidence.CheckKindTest, pytest + " tests/test_util.py", "test tests/test_util.py is named after changed module src/calc/util.py"},
				pyBuild("src/calc/util.py"),
				pyLint("src/calc/util.py"),
			},
		},
		{
			name:    "conftest: every test below its directory",
			changed: []string{"tests/integration/conftest.py"},
			want: []want{
				{evidence.CheckKindTest, pytest + " tests/integration", "changed tests/integration/conftest.py, which applies to every test below tests/integration"},
				pyBuild("tests/integration/conftest.py"),
				pyLint("tests/integration/conftest.py"),
			},
		},
		{
			name:    "changed test file",
			changed: []string{"tests/test_ops.py"},
			want: []want{
				{evidence.CheckKindTest, pytest + " tests/test_ops.py", "changed test file tests/test_ops.py"},
				pyBuild("tests/test_ops.py"),
				pyLint("tests/test_ops.py"),
			},
		},
		{
			name:    "package __init__: everything importing the package",
			changed: []string{"src/calc/__init__.py"},
			want: []want{
				{evidence.CheckKindTest, pytest + " tests/integration/test_flow.py", "imports changed module src/calc/__init__.py"},
				{evidence.CheckKindTest, pytest + " tests/test_fmt.py", "imports changed module src/calc/__init__.py"},
				{evidence.CheckKindTest, pytest + " tests/test_ops.py", "imports changed module src/calc/__init__.py"},
				{evidence.CheckKindTest, pytest + " tests/test_square.py", "via tests/helpers.py"},
				pyBuild("src/calc/__init__.py"),
				pyLint("src/calc/__init__.py"),
			},
		},
		{
			name:    "non-Python file: the policy requires test, so the full suite",
			changed: []string{"docs/usage.md"},
			want: []want{
				{evidence.CheckKindTest, pytest, "policy requires test and no package owns the changed files"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := assertPlan(t, pyFixture, tc.changed, jsPolicy(pytest), tc.want)
			if plan.Fallback != "" || plan.Language != "python" {
				t.Fatalf("fallback %q, language %q", plan.Fallback, plan.Language)
			}
			for _, tg := range plan.Targets {
				if (tg.Kind != evidence.CheckKindTest) != tg.Advisory {
					t.Fatalf("%s advisory=%t: compile and lint are advisory unless required, tests never", tg.Kind, tg.Advisory)
				}
			}
		})
	}
	// The test targets batch into one pytest invocation.
	plan := Select(context.Background(), pyFixture, []string{"src/calc/fmt.py"}, jsPolicy(pytest), nil)
	if b := plan.Batches(); b[0].Command != pytest+" tests/integration/test_flow.py tests/test_fmt.py" {
		t.Fatalf("batch = %q", b[0].Command)
	}
}

// TestSelectPythonNeverStricterThanTheGate: the gate's options are kept
// verbatim, its paths and --ignore bound the selection, and options that
// could make a narrowed run select no tests fall back to the full suite.
func TestSelectPythonNeverStricterThanTheGate(t *testing.T) {
	t.Run("flags kept, paths restrict", func(t *testing.T) {
		assertPlan(t, pyFixture, []string{"src/calc/ops.py"}, jsPolicy("pytest -x --tb=short -n 2 tests/integration"), []want{
			{evidence.CheckKindTest, "pytest -x --tb=short -n 2 tests/integration/test_flow.py", "via src/calc/fmt.py"},
			pyBuild("src/calc/ops.py"),
			pyLint("src/calc/ops.py"),
		})
	})
	t.Run("--ignore excludes", func(t *testing.T) {
		assertPlan(t, pyFixture, []string{"src/calc/fmt.py"}, jsPolicy("python -m pytest --ignore=tests/integration"), []want{
			{evidence.CheckKindTest, "python -m pytest --ignore=tests/integration tests/test_fmt.py", "imports changed module src/calc/fmt.py"},
			pyBuild("src/calc/fmt.py"),
			pyLint("src/calc/fmt.py"),
		})
	})
	t.Run("a test file without tests is never selected (pytest would exit 5)", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "tests/test_empty.py", "import calc.ops  # helpers only, no tests\n")
		plan := Select(context.Background(), tree, []string{"src/calc/ops.py"}, jsPolicy("python -m pytest"), nil)
		for _, tg := range plan.Targets {
			if strings.Contains(tg.Command, "test_empty") {
				t.Fatalf("selected a file without tests: %s", tg.Command)
			}
		}
	})
	t.Run("python_files from the config", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "pytest.ini", "[pytest]\ntestpaths = tests\npythonpath = src\npython_files = check_*.py\n")
		write(t, tree, "tests/check_ops.py", "from calc.ops import add\n\ndef test_add():\n    assert add(1, 1) == 2\n")
		assertPlan(t, tree, []string{"src/calc/ops.py"}, jsPolicy("python -m pytest"), []want{
			{evidence.CheckKindTest, "python -m pytest tests/check_ops.py", "imports changed module src/calc/ops.py"},
			pyBuild("src/calc/ops.py"),
			pyLint("src/calc/ops.py"),
		})
	})
	t.Run("required build and lint are enforced", func(t *testing.T) {
		p := PolicyFor(verifypolicy.Policy{RequiredChecks: []evidence.CheckKind{evidence.CheckKindBuild, evidence.CheckKindLint}}, "python -m pytest")
		plan := assertPlan(t, pyFixture, []string{"tests/test_ops.py"}, p, []want{
			{evidence.CheckKindTest, "python -m pytest tests/test_ops.py", "changed test file"},
			{evidence.CheckKindBuild, compileCmd("tests/test_ops.py"), "policy requires build"},
			{evidence.CheckKindLint, "python -m ruff check tests/test_ops.py", "policy requires lint"},
		})
		for _, tg := range plan.Targets {
			if tg.Advisory {
				t.Fatalf("required %s is advisory", tg.Kind)
			}
		}
	})
	t.Run("flake8 when ruff is not configured", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "pyproject.toml", "[tool.pytest.ini_options]\ntestpaths = [\"tests\"]\npythonpath = [\"src\"]\n")
		write(t, tree, ".flake8", "[flake8]\nmax-line-length = 100\n")
		assertPlan(t, tree, []string{"tests/test_ops.py"}, jsPolicy("python -m pytest"), []want{
			{evidence.CheckKindTest, "python -m pytest tests/test_ops.py", "changed test file"},
			pyBuild("tests/test_ops.py"),
			{evidence.CheckKindLint, "python -m flake8 tests/test_ops.py", "configured linter"},
		})
	})
}

func TestSelectPythonFallsBackToTheFullSuite(t *testing.T) {
	cov := copyTree(t, pyFixture)
	write(t, cov, ".coveragerc", "[report]\nfail_under = 90\n")
	addopts := copyTree(t, pyFixture)
	write(t, addopts, "pytest.ini", "[pytest]\naddopts = -m 'not slow'\n")
	cases := []struct {
		name, tree string
		changed    []string
		cmd        string
		fallback   string
		extra      []evidence.CheckKind
	}{
		{"pyproject changed", pyFixture, []string{"pyproject.toml", "src/calc/ops.py"}, "python -m pytest", "pyproject.toml changed", nil},
		{"requirements changed", pyFixture, []string{"requirements-dev.txt"}, "python -m pytest", "requirements-dev.txt changed", nil},
		{"-k selection", pyFixture, []string{"src/calc/ops.py"}, "python -m pytest -k add", "-k", nil},
		{"--lf state", pyFixture, []string{"src/calc/ops.py"}, "pytest --lf", "--lf", nil},
		{"node id", pyFixture, []string{"src/calc/ops.py"}, "pytest tests/test_ops.py::test_add", "selects test node", nil},
		{"unknown option", pyFixture, []string{"src/calc/ops.py"}, "pytest --frobnicate tests", "--frobnicate", nil},
		{"coverage threshold", cov, []string{"src/calc/ops.py"}, "pytest --cov=calc", "fail_under in .coveragerc", nil},
		{"addopts marker selection", addopts, []string{"src/calc/ops.py"}, "python -m pytest", "addopts in pytest.ini has -m", nil},
		{"custom gate command keeps compile and lint", pyFixture, []string{"src/calc/ops.py"}, "tox -e py312", "not a plain pytest invocation",
			[]evidence.CheckKind{evidence.CheckKindBuild, evidence.CheckKindLint}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Select(context.Background(), tc.tree, tc.changed, jsPolicy(tc.cmd), nil)
			if !strings.Contains(plan.Fallback, tc.fallback) {
				t.Fatalf("fallback = %q, want it to contain %q\n%s", plan.Fallback, tc.fallback, dump(plan.Targets))
			}
			ex := plan.Executable()
			if len(ex) == 0 || ex[0].Command != tc.cmd || !strings.Contains(ex[0].Reason, tc.fallback) {
				t.Fatalf("first target = %+v, want the full suite %q with the reason", ex, tc.cmd)
			}
			var kinds []evidence.CheckKind
			for _, tg := range ex[1:] {
				kinds = append(kinds, tg.Kind)
			}
			if !reflect.DeepEqual(kinds, tc.extra) {
				t.Fatalf("extra targets = %v, want %v", kinds, tc.extra)
			}
		})
	}
}

func TestParsePytest(t *testing.T) {
	cases := []struct {
		cmd    string
		flags  []string
		paths  []string
		python string
	}{
		{"python -m pytest", nil, nil, "python"},
		{"python3 -m pytest -q -x tests", []string{"-q", "-x"}, []string{"tests"}, "python3"},
		{"pytest -n 4 --tb short -ra tests/unit ./tests/integration/", []string{"-n", "4", "--tb", "short", "-ra"}, []string{"tests/unit", "tests/integration"}, "python"},
		{"pytest -p no:cacheprovider --maxfail=2 -vv", []string{"-p", "no:cacheprovider", "--maxfail=2", "-vv"}, nil, "python"},
		{"pytest --cov=src --cov-report term tests", []string{"--cov=src", "--cov-report", "term"}, []string{"tests"}, "python"},
	}
	for _, tc := range cases {
		c, err := parsePytest(tc.cmd)
		if err != nil {
			t.Fatalf("parsePytest(%q): %v", tc.cmd, err)
		}
		if !reflect.DeepEqual(c.flags, tc.flags) || !reflect.DeepEqual(c.paths, tc.paths) || c.python != tc.python {
			t.Errorf("parsePytest(%q) = flags %q paths %q python %q", tc.cmd, c.flags, c.paths, c.python)
		}
	}
	for _, bad := range []string{"pytest -k x", "pytest -mslow", "pytest --deselect a", "pytest --sw", "pytest -c other.ini", "pytest --cov src tests", "pytest -Z"} {
		if _, err := parsePytest(bad); err == nil {
			t.Errorf("parsePytest(%q) accepted a command it cannot narrow safely", bad)
		}
	}
	for _, other := range []string{"tox", "make test", "python -m unittest", "pytest | tee out"} {
		_, err := parsePytest(other)
		if _, ok := err.(*runnerMismatchError); !ok {
			t.Errorf("parsePytest(%q) = %v, want a runner mismatch", other, err)
		}
	}
}

// TestSelectMultiLanguageUnion: a repository with Go, JavaScript and
// Python gets the union of the languages' plans. Only the language whose
// runner is the gate's command contributes test targets; the others add
// their advisory checks.
func TestSelectMultiLanguageUnion(t *testing.T) {
	tree := copyTree(t, npmFixture)
	if err := copyDir(pyFixture, tree); err != nil {
		t.Fatal(err)
	}
	write(t, tree, "go.mod", "module example.com/multi\n\ngo 1.22\n")
	write(t, tree, "gopkg/x.go", "package gopkg\n\nfunc X() int { return 1 }\n")
	write(t, tree, "gopkg/x_test.go", "package gopkg\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { _ = X() }\n")
	changed := []string{"gopkg/x.go", "packages/app/src/format.ts", "src/calc/util.py"}

	plan := assertPlan(t, tree, changed, jsPolicy("npm test"), []want{
		{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/format.test.ts", "imports changed file packages/app/src/format.ts; policy requires test"},
		{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "changed packages/app/src/format.ts"},
		{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script"},
		pyBuild("src/calc/util.py"),
		pyLint("src/calc/util.py"),
	})
	if plan.Language != "go+javascript+python" || plan.Fallback != "" {
		t.Fatalf("language %q fallback %q", plan.Language, plan.Fallback)
	}

	plan = assertPlan(t, tree, changed, PolicyFor(verifypolicy.Default([16]byte{}), "go test ./... -count=1"), []want{
		{evidence.CheckKindTest, "go test -count=1 ./gopkg", "owner of changed file gopkg/x.go; policy requires test"},
		{evidence.CheckKindBuild, "go build ./...", "module must still compile"},
		{evidence.CheckKindLint, "go vet ./gopkg", "vet the changed package"},
		{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "changed packages/app/src/format.ts"},
		{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script"},
		pyBuild("src/calc/util.py"),
		pyLint("src/calc/util.py"),
	})
	if plan.Fallback != "" {
		t.Fatalf("fallback %q", plan.Fallback)
	}

	// No language runs the gate's command: the full suite plus every
	// language's advisory checks (Go contributes none without its gate).
	plan = Select(context.Background(), tree, changed, jsPolicy("make test"), nil)
	if !strings.Contains(plan.Fallback, "go: the policy test command") || !strings.Contains(plan.Fallback, "python: the policy test command") {
		t.Fatalf("fallback = %q, want every language's mismatch", plan.Fallback)
	}
	if ex := plan.Executable(); len(ex) != 5 || ex[0].Command != "make test" {
		t.Fatalf("targets:\n%s", dump(plan.Targets))
	}
}
