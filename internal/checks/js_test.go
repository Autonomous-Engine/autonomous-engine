package checks

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

const npmFixture = "testdata/npmws"

func jsPolicy(cmd string) Policy { return PolicyFor(verifypolicy.Default(uuid.New()), cmd) }

// copyTree copies a fixture directory into a fresh temporary directory.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			if info.Name() == "node_modules" || info.Name() == "__pycache__" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
}

// assertPlan checks the targets and the plan invariants every language
// shares: deterministic, the gate's suite in Full, every executable test
// target recognised as test evidence by the controller.
func assertPlan(t *testing.T, tree string, changed []string, p Policy, w []want) Plan {
	t.Helper()
	plan := Select(context.Background(), tree, changed, p, nil)
	assertTargets(t, plan.Targets, w)
	if len(plan.Full) == 0 || plan.Full[0].Command != p.FullTestCommand {
		t.Fatalf("full suite = %+v, want the policy's %q", plan.Full, p.FullTestCommand)
	}
	again := Select(context.Background(), tree, append([]string{}, changed...), p, nil)
	if !reflect.DeepEqual(plan, again) {
		t.Fatalf("Select is not deterministic:\n%+v\n%+v", plan, again)
	}
	for _, b := range plan.Batches() {
		if b.Kind == evidence.CheckKindTest && !controller.IsTestCommand(b.Command, "") {
			t.Fatalf("%q is not recognised as a test command", b.Command)
		}
	}
	return plan
}

// TestSelectJSWorkspace is the M3-05 proof on the npm workspace fixture:
// the root test script delegates to @ws/lib (jest), @ws/app (vitest, TS,
// depends on @ws/lib, lint script) and @ws/clock (node:test).
func TestSelectJSWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		want    []want
	}{
		{
			name:    "jest package file: its test, and the dependent vitest package's test through the package import",
			changed: []string{"packages/lib/src/math.js"},
			want: []want{
				{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/greet.test.ts", "test packages/app/src/greet.test.ts is affected by changed file packages/lib/src/math.js (via packages/app/src/greet.ts); policy requires test"},
				{evidence.CheckKindTest, "npm test --workspace=packages/lib -- src/math.test.js", "test packages/lib/src/math.test.js imports changed file packages/lib/src/math.js"},
				{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "typecheck packages/app: packages/app/src/greet.ts is affected by changed file packages/lib/src/math.js"},
			},
		},
		{
			name:    "vitest TypeScript file: its test (ESM .js specifier), typecheck, lint script",
			changed: []string{"packages/app/src/format.ts"},
			want: []want{
				{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/format.test.ts", "test packages/app/src/format.test.ts imports changed file packages/app/src/format.ts"},
				{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "changed packages/app/src/format.ts"},
				{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script of packages/app (owner of changed file packages/app/src/format.ts)"},
			},
		},
		{
			name:    "node:test package file: node --test with the test file",
			changed: []string{"packages/clock/src/clock.js"},
			want: []want{
				{evidence.CheckKindTest, "npm test --workspace=packages/clock -- test/clock.test.js", "imports changed file packages/clock/src/clock.js"},
			},
		},
		{
			name:    "file no test imports: the owning package's whole test script",
			changed: []string{"packages/clock/src/extra.js"},
			want: []want{
				{evidence.CheckKindTest, "npm test --workspace=packages/clock", "whole test script of packages/clock: no test imports changed file packages/clock/src/extra.js statically"},
			},
		},
		{
			name:    "changed jest test under __tests__",
			changed: []string{"packages/lib/__tests__/strings.test.js"},
			want: []want{
				{evidence.CheckKindTest, "npm test --workspace=packages/lib -- __tests__/strings.test.js", "changed test file packages/lib/__tests__/strings.test.js"},
			},
		},
		{
			name:    "changed package manifest: that package and its dependents run whole",
			changed: []string{"packages/lib/package.json"},
			want: []want{
				{evidence.CheckKindTest, "npm test --workspace=packages/app", "depends on @ws/lib, whose manifest packages/lib/package.json changed"},
				{evidence.CheckKindTest, "npm test --workspace=packages/lib", "changed manifest packages/lib/package.json"},
				{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "affected by changed file packages/lib/package.json"},
			},
		},
		{
			name:    "docs only: the policy requires test, so the full suite",
			changed: []string{"README.md", "packages/lib/README.md"},
			want: []want{
				{evidence.CheckKindTest, "npm test", "policy requires test and no package owns the changed files"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := assertPlan(t, npmFixture, tc.changed, jsPolicy("npm test"), tc.want)
			if plan.Fallback != "" || plan.Language != "javascript" {
				t.Fatalf("fallback %q, language %q", plan.Fallback, plan.Language)
			}
			for _, tg := range plan.Targets {
				if (tg.Kind == evidence.CheckKindTypecheck || tg.Kind == evidence.CheckKindLint) != tg.Advisory {
					t.Fatalf("%s target advisory=%t; typecheck and lint are advisory unless required, tests never", tg.Kind, tg.Advisory)
				}
			}
		})
	}
}

func TestSelectJSBatchesFilesPerPackage(t *testing.T) {
	plan := Select(context.Background(), npmFixture, []string{"packages/lib/src/math.js", "packages/lib/src/strings.js"}, jsPolicy("npm test"), nil)
	var cmds []string
	for _, b := range plan.Batches() {
		if b.Kind == evidence.CheckKindTest {
			cmds = append(cmds, b.Command)
		}
	}
	want := []string{
		"npm test --workspace=packages/app -- src/greet.test.ts",
		"npm test --workspace=packages/lib -- __tests__/strings.test.js src/math.test.js",
	}
	if !reflect.DeepEqual(cmds, want) {
		t.Fatalf("test batches = %q, want %q\n%s", cmds, want, dump(plan.Targets))
	}
}

// TestSelectJSNeverStricterThanTheGate: file filters are only added when
// the runner's invocation and config leave discovery at its defaults;
// otherwise the package's whole test script — exactly what the gate runs
// for it — runs. Packages the gate does not run are never selected.
func TestSelectJSNeverStricterThanTheGate(t *testing.T) {
	edit := func(t *testing.T, tree, rel, content string) {
		t.Helper()
		write(t, tree, rel, content)
	}
	t.Run("coverage flag in the package's test script", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "packages/lib/package.json", `{"name":"@ws/lib","main":"src/index.js","scripts":{"test":"jest --ci --coverage"}}`)
		assertPlan(t, tree, []string{"packages/lib/src/math.js"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/greet.test.ts", "affected by changed file packages/lib/src/math.js"},
			{evidence.CheckKindTest, "npm test --workspace=packages/lib", "cannot take file filters: flag --coverage may change which tests run"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "typecheck packages/app"},
		})
	})
	t.Run("vitest config customising include", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "packages/app/vitest.config.ts", "export default { test: { include: ['src/**/*.test.ts'] } };\n")
		assertPlan(t, tree, []string{"packages/app/src/format.ts"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app", `packages/app/vitest.config.ts sets "include"`},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "changed packages/app/src/format.ts"},
			{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script"},
		})
	})
	t.Run("a test script that already selects tests", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "packages/clock/package.json", `{"name":"@ws/clock","type":"module","scripts":{"test":"node --test test/"}}`)
		assertPlan(t, tree, []string{"packages/clock/src/clock.js"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/clock", "it already selects tests (test/)"},
		})
	})
	t.Run("a package the gate does not run is not selected", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "package.json", `{"name":"npmws","private":true,"workspaces":["packages/*"],"scripts":{"test":"npm test -w packages/app"},"devDependencies":{"typescript":"^5.3.0","vitest":"^1.6.1"}}`)
		assertPlan(t, tree, []string{"packages/lib/src/math.js"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/greet.test.ts", "affected by changed file packages/lib/src/math.js"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "typecheck packages/app"},
		})
	})
	t.Run("a package without a test script under --if-present", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "packages/clock/package.json", `{"name":"@ws/clock","type":"module","scripts":{}}`)
		assertPlan(t, tree, []string{"packages/clock/src/clock.js"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test", "policy requires test and no package owns the changed files"},
		})
	})
	t.Run("root runner: one invocation over the repository", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "package.json", `{"name":"npmws","private":true,"workspaces":["packages/*"],"scripts":{"test":"vitest run"},"devDependencies":{"typescript":"^5.3.0","vitest":"^1.6.1"}}`)
		plan := assertPlan(t, tree, []string{"packages/lib/src/math.js"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test -- packages/app/src/greet.test.ts", "affected by changed file packages/lib/src/math.js"},
			{evidence.CheckKindTest, "npm test -- packages/lib/src/math.test.js", "imports changed file packages/lib/src/math.js"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "typecheck packages/app"},
		})
		if b := plan.Batches(); b[0].Command != "npm test -- packages/app/src/greet.test.ts packages/lib/src/math.test.js" {
			t.Fatalf("batch = %q", b[0].Command)
		}
	})
	t.Run("direct runner command and yarn", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		edit(t, tree, "package.json", `{"name":"npmws","private":true,"workspaces":["packages/*"],"scripts":{"test":"vitest run --reporter=dot"},"devDependencies":{"vitest":"^1.6.1"}}`)
		assertPlan(t, tree, []string{"packages/app/src/format.ts"}, jsPolicy("npx vitest run --silent"), []want{
			{evidence.CheckKindTest, "npx vitest run --silent packages/app/src/format.test.ts", "imports changed file packages/app/src/format.ts"},
			{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script"},
		})
		assertPlan(t, tree, []string{"packages/app/src/format.ts"}, jsPolicy("yarn test"), []want{
			{evidence.CheckKindTest, "yarn test packages/app/src/format.test.ts", "imports changed file packages/app/src/format.ts"},
			{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "lint script"},
		})
	})
	t.Run("a deleted file still selects the tests that import it", func(t *testing.T) {
		tree := copyTree(t, npmFixture)
		if err := os.Remove(filepath.Join(tree, "packages", "app", "src", "format.ts")); err != nil {
			t.Fatal(err)
		}
		assertPlan(t, tree, []string{"packages/app/src/format.ts"}, jsPolicy("npm test"), []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/format.test.ts", "imports changed file packages/app/src/format.ts"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "changed packages/app/src/format.ts"},
		})
	})
	t.Run("required typecheck and lint are enforced", func(t *testing.T) {
		p := PolicyFor(verifypolicy.Policy{RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindTypecheck, evidence.CheckKindLint}}, "npm test")
		plan := assertPlan(t, npmFixture, []string{"packages/app/src/format.ts"}, p, []want{
			{evidence.CheckKindTest, "npm test --workspace=packages/app -- src/format.test.ts", "policy requires test"},
			{evidence.CheckKindTypecheck, "npx --no-install tsc --noEmit -p packages/app/tsconfig.json", "policy requires typecheck"},
			{evidence.CheckKindLint, "npm run lint --workspace=packages/app", "policy requires lint"},
		})
		for _, tg := range plan.Targets {
			if tg.Advisory {
				t.Fatalf("required %s is advisory", tg.Kind)
			}
		}
	})
}

func TestSelectJSFallsBackToTheFullSuite(t *testing.T) {
	broken := copyTree(t, npmFixture)
	write(t, broken, "packages/lib/package.json", `{"name": "@ws/lib",`)
	compound := copyTree(t, npmFixture)
	write(t, compound, "package.json", `{"name":"npmws","workspaces":["packages/*"],"scripts":{"test":"tsc && vitest run"}}`)
	cases := []struct {
		name, tree string
		changed    []string
		cmd        string
		fallback   string
		extra      []evidence.CheckKind // non-test targets kept with the full suite
	}{
		{"lockfile changed", npmFixture, []string{"package-lock.json"}, "npm test", "package-lock.json changed", nil},
		{"root manifest changed", npmFixture, []string{"package.json", "packages/app/src/format.ts"}, "npm test", "package.json changed", nil},
		{"root tsconfig changed", npmFixture, []string{"tsconfig.base.json"}, "npm test", "tsconfig.base.json changed", nil},
		{"unparsable manifest", broken, []string{"packages/lib/src/math.js"}, "npm test", "parse error in packages/lib/package.json", nil},
		{"extra arguments to the script", npmFixture, []string{"packages/lib/src/math.js"}, "npm test -- --coverage", "passes extra arguments", nil},
		{"compound root script", compound, []string{"packages/lib/src/math.js"}, "npm test", "neither a single test-runner invocation", nil},
		{"custom gate command keeps the advisory checks", npmFixture, []string{"packages/app/src/format.ts"}, "make test", "not an npm/yarn/pnpm test script",
			[]evidence.CheckKind{evidence.CheckKindTypecheck, evidence.CheckKindLint}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := jsPolicy(tc.cmd)
			plan := Select(context.Background(), tc.tree, tc.changed, p, nil)
			if !strings.Contains(plan.Fallback, tc.fallback) {
				t.Fatalf("fallback = %q, want it to contain %q\n%s", plan.Fallback, tc.fallback, dump(plan.Targets))
			}
			ex := plan.Executable()
			if len(ex) == 0 || ex[0].Kind != evidence.CheckKindTest || ex[0].Command != tc.cmd || !strings.Contains(ex[0].Reason, tc.fallback) {
				t.Fatalf("first target = %+v, want the full suite %q with the reason", ex, tc.cmd)
			}
			var kinds []evidence.CheckKind
			for _, tg := range ex[1:] {
				kinds = append(kinds, tg.Kind)
				if !tg.Advisory {
					t.Fatalf("extra %s target is not advisory", tg.Kind)
				}
			}
			if !reflect.DeepEqual(kinds, tc.extra) {
				t.Fatalf("extra targets = %v, want %v", kinds, tc.extra)
			}
		})
	}
}

func TestParseJSRunner(t *testing.T) {
	cases := []struct {
		cmd        string
		kind       string
		narrowable bool
	}{
		{"vitest run", runnerVitest, true},
		{"vitest", runnerVitest, true},
		{"vitest --reporter verbose run", runnerVitest, true},
		{"./node_modules/.bin/vitest run --pool=forks", runnerVitest, true},
		{"vitest run src/", runnerVitest, false},
		{"vitest run --coverage", runnerVitest, false},
		{"vitest run --project unit", runnerVitest, false},
		{"vitest related src/a.ts", runnerVitest, false},
		{"jest", runnerJest, true},
		{"jest --ci --runInBand", runnerJest, true},
		{"NODE_ENV=test jest --ci", runnerJest, true},
		{"cross-env CI=1 TZ=UTC jest -w 2", runnerJest, true},
		{"npx jest --silent", runnerJest, true},
		{"jest --coverage", runnerJest, false},
		{"jest -c jest.config.js", runnerJest, false},
		{"jest unit", runnerJest, false},
		{"jest -- --foo", runnerJest, false},
		{"node --test", runnerNode, true},
		{"node --import tsx --test", runnerNode, true},
		{"node --test --experimental-test-coverage", runnerNode, false},
		{"node --test test/", runnerNode, false},
		{"mocha", runnerMocha, true},
		{"mocha --recursive --exit", runnerMocha, true},
		{"mocha 'spec/**/*.js'", runnerMocha, false},
		{"tsc && vitest run", "", false},
		{"eslint .", "", false},
		{"node scripts/run.js", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		r, ok := parseJSRunner(tc.cmd)
		if ok != (tc.kind != "") || r.kind != tc.kind || r.narrowable != tc.narrowable {
			t.Errorf("parseJSRunner(%q) = %+v, %t; want kind %q narrowable %t", tc.cmd, r, ok, tc.kind, tc.narrowable)
		}
		if ok && !r.narrowable && r.why == "" {
			t.Errorf("parseJSRunner(%q): not narrowable without a reason", tc.cmd)
		}
	}
}

func TestIsRunnerTestFile(t *testing.T) {
	cases := []struct {
		r    jsRunner
		rel  string
		want bool
	}{
		{jsRunner{kind: runnerVitest}, "src/a.test.ts", true},
		{jsRunner{kind: runnerVitest}, "src/a.spec.mjs", true},
		{jsRunner{kind: runnerVitest}, "src/a.ts", false},
		{jsRunner{kind: runnerVitest}, "dist/a.test.js", false},
		{jsRunner{kind: runnerVitest}, "__tests__/a.ts", false},
		{jsRunner{kind: runnerJest}, "__tests__/a.js", true},
		{jsRunner{kind: runnerJest}, "src/a.spec.tsx", true},
		{jsRunner{kind: runnerJest}, "node_modules/x/a.test.js", false},
		{jsRunner{kind: runnerMocha}, "test/a.js", true},
		{jsRunner{kind: runnerMocha}, "test/sub/a.js", false},
		{jsRunner{kind: runnerMocha, recursive: true}, "test/sub/a.js", true},
		{jsRunner{kind: runnerMocha}, "src/a.test.js", false},
		{jsRunner{kind: runnerNode}, "test/a.js", true},
		{jsRunner{kind: runnerNode}, "src/a.test.mjs", true},
		{jsRunner{kind: runnerNode}, "src/a_test.cjs", true},
		{jsRunner{kind: runnerNode}, "src/test-a.js", true},
		{jsRunner{kind: runnerNode}, "src/a.test.ts", false},
		{jsRunner{kind: runnerNode}, "src/a.js", false},
	}
	for _, tc := range cases {
		if got := isRunnerTestFile(tc.r, tc.rel); got != tc.want {
			t.Errorf("isRunnerTestFile(%s, %q) = %t, want %t", tc.r.kind, tc.rel, got, tc.want)
		}
	}
}

func TestParseNpmDelegation(t *testing.T) {
	cases := []struct {
		script    string
		ok        bool
		filter    []string
		ifPresent bool
	}{
		{"npm test --workspaces --if-present", true, nil, true},
		{"npm run test -ws", true, nil, false},
		{"npm test --workspace=packages/a -w b", true, []string{"packages/a", "b"}, false},
		{"npm test", false, nil, false},
		{"npm test --workspaces -- --coverage", false, nil, false},
		{"npm test --workspaces --include-workspace-root", false, nil, false},
		{"npm run build --workspaces", false, nil, false},
		{"pnpm -r test", false, nil, false},
	}
	for _, tc := range cases {
		ok, filter, ifPresent := parseNpmDelegation(tc.script)
		if ok != tc.ok || !reflect.DeepEqual(filter, tc.filter) || ifPresent != tc.ifPresent {
			t.Errorf("parseNpmDelegation(%q) = %t %q %t, want %t %q %t", tc.script, ok, filter, ifPresent, tc.ok, tc.filter, tc.ifPresent)
		}
	}
}
