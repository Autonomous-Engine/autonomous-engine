package checks

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// Fix round on M3-05: filters are literal (shell-quoted; regex-escaped for
// jest; unfilterable globs run the whole script), pytest's collection
// bounds (--ignore-glob, -o overrides, conftest collect_ignore,
// python_functions / python_classes) are honoured, and import-alias
// configurations make a unit package-level.

func TestFilterArg(t *testing.T) {
	cases := []struct {
		kind, rel, want string
		ok              bool
	}{
		{runnerJest, "src/math.test.js", "src/math.test.js", true},
		{runnerJest, "src/routes/[id].test.js", `'src/routes/\[id\].test.js'`, true},
		{runnerJest, "src/(legacy)/old.test.js", `'src/\(legacy\)/old.test.js'`, true},
		{runnerJest, "src/a+b.test.js", `'src/a\+b.test.js'`, true},
		{runnerJest, "src/my file.test.js", `'src/my file.test.js'`, true},
		{runnerJest, "src/it's.test.js", `'src/it'\''s.test.js'`, true},
		{runnerVitest, "src/(group)/page.test.ts", `'src/(group)/page.test.ts'`, true},
		{runnerVitest, "src/[id].test.ts", `'src/[id].test.ts'`, true},
		{runnerNode, "test/with space.test.js", `'test/with space.test.js'`, true},
		{runnerNode, "test/[id].test.js", "", false},
		{runnerMocha, "test/(a).js", "", false},
		{runnerMocha, "test/a.js", "test/a.js", true},
		// a dash-leading path must not be read as an option
		{runnerJest, "-x.test.js", `'\-x.test.js'`, true},
		{runnerNode, "-x.test.js", "./-x.test.js", true},
		{runnerMocha, "-a.js", "./-a.js", true},
		{runnerVitest, "-x.test.ts", "", false},
	}
	for _, tc := range cases {
		got, ok := filterArg(tc.kind, tc.rel)
		if got != tc.want || ok != tc.ok {
			t.Errorf("filterArg(%s, %q) = %q, %t; want %q, %t", tc.kind, tc.rel, got, ok, tc.want, tc.ok)
		}
	}
}

func TestJestDefaultTestMatchExtensions(t *testing.T) {
	j := jsRunner{kind: runnerJest}
	for rel, want := range map[string]bool{
		"src/a.test.js": true, "src/a.test.jsx": true, "src/a.test.ts": true, "src/a.spec.tsx": true,
		"src/a.test.mjs": false, "src/a.test.cjs": false, "src/a.test.mts": false, "src/a.test.cts": false,
		"__tests__/a.js": true, "__tests__/a.mjs": false,
	} {
		if got := isRunnerTestFile(j, rel); got != want {
			t.Errorf("jest test file %q = %t, want %t", rel, got, want)
		}
	}
}

func TestSelectJSLiteralFilters(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		want    []want
	}{
		// A non-test file of @ws/lib changes the package, so @ws/app's
		// importer of @ws/lib is selected too.
		{"jest regex metacharacters are escaped and quoted", []string{"packages/lib/src/routes/[id].js"}, []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/greet.test.ts", "affected by changed file packages/lib/src/routes/[id].js"},
			{evidence.CheckKindTest, `npm test --workspace=packages/lib -- 'src/routes/\[id\].test.js'`, "imports changed file packages/lib/src/routes/[id].js"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "typecheck packages/app"},
		}},
		{"jest parentheses", []string{"packages/lib/src/(legacy)/old.js"}, []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/greet.test.ts", "affected by changed file packages/lib/src/(legacy)/old.js"},
			{evidence.CheckKindTest, `npm test --workspace=packages/lib -- 'src/\(legacy\)/old.test.js'`, "imports changed file packages/lib/src/(legacy)/old.js"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "typecheck packages/app"},
		}},
		{"vitest parentheses are quoted", []string{"packages/app/src/(group)/page.ts"}, []want{
			{evidence.CheckKindTest, `npm test --workspace=packages/app -- 'src/(group)/page.test.ts'`, "imports changed file packages/app/src/(group)/page.ts"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "changed packages/app/src/(group)/page.ts"},
			{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script"},
		}},
		{"node --test path with a space", []string{"packages/clock/src/tick.js"}, []want{
			{evidence.CheckKindTest, `npm test --workspace=packages/clock -- 'test/with space.test.js'`, "imports changed file packages/clock/src/tick.js"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertPlan(t, npmFixture, tc.changed, jsPolicy("npm test"), tc.want)
		})
	}
	t.Run("node --test glob characters run the whole script", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		write(t, tree, "packages/clock/test/[slot].test.js", "import { test } from 'node:test';\nimport { tick } from '../src/tick.js';\ntest('slot', () => tick(1));\n")
		assertPlan(t, tree, []string{"packages/clock/src/tick.js"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/clock", "cannot be written as a literal node:test filter"},
		})
	})
	t.Run("jest does not run .mjs tests by default", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		write(t, tree, "packages/lib/src/math.test.mjs", "import { add } from './math.js';\n")
		plan := Select(context.Background(), tree, []string{"packages/lib/src/math.js"}, jsPolicy("npm test"), nil)
		for _, tg := range plan.Targets {
			if strings.Contains(tg.Command, ".mjs") {
				t.Fatalf("selected a test jest does not run: %s", tg.Command)
			}
		}
	})
	t.Run("batched quoted filters", func(t *testing.T) {
		plan := Select(context.Background(), npmFixture, []string{"packages/lib/src/routes/[id].js", "packages/lib/src/(legacy)/old.js"}, jsPolicy("npm test"), nil)
		b := plan.Batches()
		if len(b) != 3 || b[1].Command != `npm test --workspace=packages/lib -- 'src/\(legacy\)/old.test.js' 'src/routes/\[id\].test.js'` {
			t.Fatalf("batches = %+v", b)
		}
	})
}

// TestSelectJSAliasConfigsRunWhole: tsconfig paths/baseUrl, package.json
// "imports", jest moduleNameMapper and vite resolve.alias add import paths
// the scan cannot resolve, so the unit runs whole.
func TestSelectJSAliasConfigsRunWhole(t *testing.T) {
	cases := []struct {
		name, file, content string
		changed             string
		cmd, reason         string
	}{
		{"tsconfig paths (@/ alias)", "packages/app/tsconfig.json",
			`{"compilerOptions":{"strict":true,"paths":{"@/*":["./src/*"]}},"include":["src/**/*.ts"]}`,
			"packages/app/src/format.ts", "npm test --workspace=packages/app", "packages/app/tsconfig.json sets compilerOptions.paths"},
		{"root tsconfig baseUrl", "tsconfig.base.json", `{"compilerOptions":{"baseUrl":"."}}`,
			"packages/app/src/format.ts", "npm test --workspace=packages/app", "tsconfig.base.json sets compilerOptions.baseUrl"},
		{"package.json imports", "packages/clock/package.json",
			`{"name":"@ws/clock","type":"module","imports":{"#src/*":"./src/*"},"scripts":{"test":"node --test"}}`,
			"packages/clock/src/clock.js", "npm test --workspace=packages/clock", `declares "imports"`},
		{"jest moduleNameMapper", "packages/lib/package.json",
			`{"name":"@ws/lib","main":"src/index.js","scripts":{"test":"jest --ci"},"jest":{"moduleNameMapper":{"^@/(.*)$":"<rootDir>/src/$1"}}}`,
			"packages/lib/src/math.js", "npm test --workspace=packages/lib", `sets jest "moduleNameMapper"`},
		{"vite resolve.alias", "packages/app/vite.config.ts",
			"export default { resolve: { alias: { '@': '/src' } } };\n",
			"packages/app/src/format.ts", "npm test --workspace=packages/app", `sets "alias"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := copyTree(t, npmFixture)
			write(t, tree, tc.file, tc.content)
			plan := Select(context.Background(), tree, []string{tc.changed}, jsPolicy("npm test"), nil)
			var found bool
			for _, tg := range plan.Targets {
				if tg.Kind == evidence.CheckKindTest && tg.Command == tc.cmd && strings.Contains(tg.Reason, tc.reason) {
					found = true
				}
				if tg.Kind == evidence.CheckKindTest && strings.Contains(tg.Command, " -- ") && strings.Contains(tg.Command, strings.TrimPrefix(tc.cmd, "npm test ")) {
					t.Fatalf("unit still filtered: %s", tg.Command)
				}
			}
			if !found {
				t.Fatalf("want %q with reason containing %q:\n%s", tc.cmd, tc.reason, dump(plan.Targets))
			}
		})
	}
}

func TestSelectPythonCollectionBounds(t *testing.T) {
	const ops = "src/calc/ops.py"
	has := func(plan Plan, file string) bool {
		for _, tg := range plan.Targets {
			if tg.Kind == evidence.CheckKindTest && strings.Contains(tg.Command, file) {
				return true
			}
		}
		return false
	}
	t.Run("--ignore-glob on the command line", func(t *testing.T) {
		plan := Select(context.Background(), pyFixture, []string{ops}, jsPolicy("python -m pytest --ignore-glob=*integration*"), nil)
		if plan.Fallback != "" || has(plan, "test_flow") || !has(plan, "tests/test_ops.py") {
			t.Fatalf("fallback %q\n%s", plan.Fallback, dump(plan.Targets))
		}
		if !strings.HasPrefix(plan.Targets[0].Command, "python -m pytest --ignore-glob=*integration* ") {
			t.Fatalf("option not kept: %s", plan.Targets[0].Command)
		}
	})
	t.Run("--ignore-glob in addopts", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "pytest.ini", "[pytest]\ntestpaths = tests\npythonpath = src\naddopts = --ignore-glob=tests/integration/*\n")
		plan := Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if plan.Fallback != "" || has(plan, "test_flow") || !has(plan, "tests/test_fmt.py") {
			t.Fatalf("fallback %q\n%s", plan.Fallback, dump(plan.Targets))
		}
	})
	t.Run("conftest collect_ignore and collect_ignore_glob literals", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "tests/conftest.py", "collect_ignore = [\"test_square.py\"]\ncollect_ignore_glob = ['integration/*']\n")
		plan := Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if plan.Fallback != "" || has(plan, "test_square") || has(plan, "test_flow") || !has(plan, "tests/test_ops.py") {
			t.Fatalf("fallback %q\n%s", plan.Fallback, dump(plan.Targets))
		}
	})
	t.Run("python_functions is honoured", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "pytest.ini", "[pytest]\ntestpaths = tests\npythonpath = src\npython_functions = check_*\n")
		write(t, tree, "tests/test_check.py", "from calc.ops import add\n\ndef check_add():\n    assert add(1, 1) == 2\n")
		plan := Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if !has(plan, "tests/test_check.py") || has(plan, "tests/test_ops.py") {
			t.Fatalf("python_functions not honoured:\n%s", dump(plan.Targets))
		}
	})
	t.Run("python_classes is honoured", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "tests/test_suite.py", "from calc.ops import add\n\nclass Suite:\n    def test_add(self):\n        assert add(1, 1) == 2\n")
		plan := Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if has(plan, "test_suite") {
			t.Fatalf("a method of a non-matching class was selected:\n%s", dump(plan.Targets))
		}
		write(t, tree, "pytest.ini", "[pytest]\ntestpaths = tests\npythonpath = src\npython_classes = Suite\n")
		plan = Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if !has(plan, "tests/test_suite.py") {
			t.Fatalf("python_classes = Suite not honoured:\n%s", dump(plan.Targets))
		}
	})
	t.Run("a quoted path", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "tests/test_my ops.py", "from calc.ops import add\n\ndef test_add():\n    assert add(1, 2) == 3\n")
		plan := Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if !strings.Contains(plan.Batches()[0].Command, `'tests/test_my ops.py'`) {
			t.Fatalf("path not quoted:\n%s", plan.Batches()[0].Command)
		}
	})
	for _, tc := range []struct {
		name, cmd, ini, conftest, fallback string
	}{
		{"-o python_files override", "python -m pytest -o python_files=check_*.py", "", "", "overrides python_files"},
		{"--override-ini testpaths", "python -m pytest --override-ini testpaths=other", "", "", "overrides testpaths"},
		{"-o in addopts", "python -m pytest", "[pytest]\naddopts = -o norecursedirs=x\n", "", "overrides norecursedirs"},
		{"computed collect_ignore", "python -m pytest", "", "collect_ignore = []\ncollect_ignore.append(\"test_ops.py\")\n", "tests/conftest.py computes collect_ignore"},
		{"literal plus a call", "python -m pytest", "", "collect_ignore = [\"test_ops.py\"] + extra()\n", "tests/conftest.py computes collect_ignore"},
		{"conditional literal", "python -m pytest", "", "collect_ignore = [\"x\"] if X else []\n", "tests/conftest.py computes collect_ignore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := copyTree(t, pyFixture)
			if tc.ini != "" {
				write(t, tree, "pytest.ini", tc.ini)
			}
			if tc.conftest != "" {
				write(t, tree, "tests/conftest.py", tc.conftest)
			}
			plan := Select(context.Background(), tree, []string{ops}, jsPolicy(tc.cmd), nil)
			if !strings.Contains(plan.Fallback, tc.fallback) || plan.Executable()[0].Command != tc.cmd {
				t.Fatalf("fallback %q, want %q\n%s", plan.Fallback, tc.fallback, dump(plan.Targets))
			}
		})
	}
	t.Run("a literal with a trailing comment is still a literal", func(t *testing.T) {
		tree := copyTree(t, pyFixture)
		write(t, tree, "tests/conftest.py", "collect_ignore = [\"test_square.py\"]  # flaky\n")
		plan := Select(context.Background(), tree, []string{ops}, jsPolicy("python -m pytest"), nil)
		if plan.Fallback != "" || has(plan, "test_square") || !has(plan, "tests/test_ops.py") {
			t.Fatalf("fallback %q\n%s", plan.Fallback, dump(plan.Targets))
		}
	})
	t.Run("-o of a non-collection key still narrows", func(t *testing.T) {
		plan := Select(context.Background(), pyFixture, []string{ops}, jsPolicy("python -m pytest -o cache_dir=.cache"), nil)
		if plan.Fallback != "" || !has(plan, "tests/test_ops.py") {
			t.Fatalf("fallback %q\n%s", plan.Fallback, dump(plan.Targets))
		}
	})
}

func TestCompileallKeepsBytecodeOutOfTheTree(t *testing.T) {
	plan := Select(context.Background(), pyFixture, []string{"src/calc/ops.py"}, jsPolicy("python -m pytest"), nil)
	for _, tg := range plan.Targets {
		if tg.Kind == evidence.CheckKindBuild {
			if !strings.HasPrefix(tg.Command, "PYTHONPYCACHEPREFIX=") || strings.Contains(pycachePrefix, "testdata") {
				t.Fatalf("compileall command %q", tg.Command)
			}
			return
		}
	}
	t.Fatal("no build target")
}

func compileCmd(files string) string {
	return "PYTHONPYCACHEPREFIX=" + shQuote(pycachePrefix) + " python -m compileall -q " + files
}
