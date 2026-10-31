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

const fixture = "testdata/gomod"

const fullGo = "go test ./... -count=1"

// want is a compact target expectation: kind, command ("" for not_run) and
// a substring the reason must contain.
type want struct {
	kind    evidence.CheckKind
	command string
	reason  string
}

func assertTargets(t *testing.T, got []Target, w []want) {
	t.Helper()
	if len(got) != len(w) {
		t.Fatalf("got %d targets, want %d:\n%s", len(got), len(w), dump(got))
	}
	for i := range w {
		g := got[i]
		if g.Kind != w[i].kind || g.Command != w[i].command || !strings.Contains(g.Reason, w[i].reason) {
			t.Fatalf("target %d = {%s %q %q}, want {%s %q reason containing %q}\nall:\n%s",
				i, g.Kind, g.Command, g.Reason, w[i].kind, w[i].command, w[i].reason, dump(got))
		}
		if g.Reason == "" {
			t.Fatalf("target %d has no reason", i)
		}
		if !g.Executable() && g.NotRunReason == "" {
			t.Fatalf("target %d has neither a command nor a not_run reason", i)
		}
	}
}

func dump(ts []Target) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString("  " + string(t.Kind) + " | " + t.Command + " | " + t.Reason + " | " + t.NotRunReason + "\n")
	}
	return b.String()
}

func defaultPolicy() Policy {
	return PolicyFor(verifypolicy.Default(uuid.New()), fullGo)
}

// TestSelectGoFixture is the roadmap 3.2 proof on the fixture module:
// seeded changes select the owning package's tests and every impacted
// dependent and contract check, each with its reason.
func TestSelectGoFixture(t *testing.T) {
	edges := []ContractEdge{
		{ContractType: "rest_openapi", ContractName: "GET /widgets", ProducerFile: "contract/handlers.go", ConsumerRepo: "web-frontend", ConsumerFile: "src/api/widgets.ts"},
		{ContractType: "rest_openapi", ContractName: "GET /gadgets", ProducerFile: "elsewhere/gadgets.go", ConsumerRepo: "web-frontend", ConsumerFile: "src/api/gadgets.ts"},
	}
	cases := []struct {
		name    string
		changed []string
		policy  Policy
		want    []want
	}{
		{
			name:    "leaf package: owner only",
			changed: []string{"leaf/leaf.go"},
			want: []want{
				{evidence.CheckKindTest, "go test -count=1 ./leaf", "owner of changed file leaf/leaf.go; policy requires test"},
				{evidence.CheckKindBuild, "go build ./...", "module must still compile"},
				{evidence.CheckKindLint, "go vet ./leaf", "vet the changed package"},
			},
		},
		{
			name:    "package with reverse dependents: owner, direct and transitive dependents, test importers",
			changed: []string{"core/core.go"},
			want: []want{
				{evidence.CheckKindTest, "go test -count=1 ./core", "owner of changed file core/core.go"},
				{evidence.CheckKindTest, "go test -count=1 ./api", "reverse dependent of ./core (via ./svc)"},
				{evidence.CheckKindTest, "go test -count=1 ./svc", "reverse dependent of ./core"},
				{evidence.CheckKindTest, "go test -count=1 ./tools", "tests import ./core, affected by ./core"},
				{evidence.CheckKindBuild, "go build ./...", "module must still compile"},
				{evidence.CheckKindLint, "go vet ./core", "vet the changed package"},
			},
		},
		{
			name:    "test-only change: the owner's tests, no dependents, no build",
			changed: []string{"core/core_test.go"},
			want: []want{
				{evidence.CheckKindTest, "go test -count=1 ./core", "owner of changed file core/core_test.go"},
				{evidence.CheckKindLint, "go vet ./core", "vet the changed package"},
			},
		},
		{
			name:    "testdata file: the owning package's tests only",
			changed: []string{"core/testdata/golden.txt"},
			want: []want{
				{evidence.CheckKindTest, "go test -count=1 ./core", "owner of changed file core/testdata/golden.txt"},
				{evidence.CheckKindLint, "go vet ./core", "vet the changed package"},
			},
		},
		{
			name:    "migration file: the detected migration test; policy test falls to the full suite",
			changed: []string{"migrations/001_init.up.sql"},
			want: []want{
				{evidence.CheckKindMigration, "go test -count=1 -v -run Migrat ./db", "migration tests detected (TestMigrationsApply)"},
				{evidence.CheckKindTest, fullGo, "policy requires test and no package owns the changed files"},
			},
		},
		{
			name:    "file on a contract edge: owner plus a not_run api_compat naming the consumer",
			changed: []string{"contract/handlers.go"},
			want: []want{
				{evidence.CheckKindTest, "go test -count=1 ./contract", "owner of changed file contract/handlers.go"},
				{evidence.CheckKindBuild, "go build ./...", "module must still compile"},
				{evidence.CheckKindLint, "go vet ./contract", "vet the changed package"},
				{evidence.CheckKindAPICompat, "", "consumer web-frontend src/api/widgets.ts uses rest_openapi GET /widgets (producer contract/handlers.go)"},
			},
		},
		{
			name:    "nested module and docs are outside the module graph",
			changed: []string{"nested/n.go", "docs/README.md"},
			want: []want{
				{evidence.CheckKindTest, fullGo, "policy requires test and no package owns the changed files"},
			},
		},
		{
			name:    "policy and criteria kinds are marked or added",
			changed: []string{"leaf/leaf.go"},
			policy:  PolicyFor(verifypolicy.Policy{RequiredChecks: []evidence.CheckKind{evidence.CheckKindLint}}, fullGo, evidence.CheckKindTypecheck),
			want: []want{
				{evidence.CheckKindTest, "go test -count=1 ./leaf", "owner of changed file leaf/leaf.go"},
				{evidence.CheckKindBuild, "go build ./...", "module must still compile"},
				{evidence.CheckKindLint, "go vet ./leaf", "vet the changed package(s); policy requires lint"},
				{evidence.CheckKindTypecheck, "", "policy requires typecheck"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.policy
			if p.FullTestCommand == "" {
				p = defaultPolicy()
			}
			plan := Select(context.Background(), fixture, tc.changed, p, edges)
			if plan.Fallback != "" {
				t.Fatalf("unexpected fallback: %s", plan.Fallback)
			}
			if plan.Language != "go" {
				t.Fatalf("language = %q, want go", plan.Language)
			}
			assertTargets(t, plan.Targets, tc.want)
			if len(plan.Full) == 0 || plan.Full[0].Command != fullGo {
				t.Fatalf("full suite = %+v, want the policy's %q", plan.Full, fullGo)
			}
			// Deterministic.
			again := Select(context.Background(), fixture, append([]string{}, tc.changed...), p, edges)
			if !reflect.DeepEqual(plan, again) {
				t.Fatalf("Select is not deterministic:\n%+v\n%+v", plan, again)
			}
			// Every executable test or migration target is test evidence
			// for the controller (M3-02) without any configured command.
			for _, tg := range plan.Executable() {
				if tg.Kind == evidence.CheckKindTest || tg.Kind == evidence.CheckKindMigration {
					if !controller.IsTestCommand(tg.Command, "") {
						t.Fatalf("%q is not recognised as a test command", tg.Command)
					}
				}
			}
		})
	}
}

func TestSelectFallsBackToTheFullSuite(t *testing.T) {
	broken := copyFixture(t)
	if err := os.WriteFile(filepath.Join(broken, "leaf", "leaf.go"), []byte("package leaf\n\nimport (\n\t\"fmt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		tree     string
		changed  []string
		policy   Policy
		fallback string
	}{
		{"go.mod changed", fixture, []string{"go.mod", "leaf/leaf.go"}, defaultPolicy(), "go.mod changed"},
		{"go.sum changed", fixture, []string{"go.sum"}, defaultPolicy(), "go.sum changed"},
		{"parse error", broken, []string{"leaf/leaf.go"}, defaultPolicy(), "parse error in leaf/leaf.go"},
		{"no module file", t.TempDir(), []string{"main.go"}, defaultPolicy(), "no impact implementation applies"},
		{"custom policy command", fixture, []string{"leaf/leaf.go"},
			PolicyFor(verifypolicy.Policy{TestCommand: strPtr("make test")}, fullGo), "not a plain go test invocation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Select(context.Background(), tc.tree, tc.changed, tc.policy, nil)
			if !strings.Contains(plan.Fallback, tc.fallback) {
				t.Fatalf("fallback = %q, want it to contain %q", plan.Fallback, tc.fallback)
			}
			ex := plan.Executable()
			if len(ex) == 0 {
				t.Fatalf("fallback plan runs nothing: %+v", plan.Targets)
			}
			if ex[0].Kind != evidence.CheckKindTest || ex[0].Command != tc.policy.FullTestCommand {
				t.Fatalf("fallback target = %+v, want the full suite %q", ex[0], tc.policy.FullTestCommand)
			}
			if !strings.Contains(ex[0].Reason, tc.fallback) {
				t.Fatalf("fallback reason %q does not say why (%q)", ex[0].Reason, tc.fallback)
			}
		})
	}
}

func TestSelectWithoutTestRunnerRunsNothingButSaysWhy(t *testing.T) {
	plan := Select(context.Background(), t.TempDir(), []string{"x.txt"}, PolicyFor(verifypolicy.Default(uuid.New()), ""), nil)
	if len(plan.Executable()) != 0 {
		t.Fatalf("plan without a test command executes %+v", plan.Executable())
	}
	if len(plan.Targets) != 1 || plan.Targets[0].NotRunReason == "" {
		t.Fatalf("targets = %+v, want one not_run test target with a reason", plan.Targets)
	}
	if plan.Full[0].Executable() || plan.Full[0].NotRunReason == "" {
		t.Fatalf("full = %+v, want a not_run test with a reason", plan.Full)
	}
}

func TestSelectDeletedPackageSelectsItsDependents(t *testing.T) {
	tree := copyFixture(t)
	if err := os.RemoveAll(filepath.Join(tree, "core")); err != nil {
		t.Fatal(err)
	}
	plan := Select(context.Background(), tree, []string{"core/core.go", "core/core_test.go"}, defaultPolicy(), nil)
	assertTargets(t, plan.Targets, []want{
		{evidence.CheckKindTest, "go test -count=1 ./api", "reverse dependent of ./core (via ./svc)"},
		{evidence.CheckKindTest, "go test -count=1 ./svc", "reverse dependent of ./core"},
		{evidence.CheckKindTest, "go test -count=1 ./tools", "tests import ./core"},
		{evidence.CheckKindBuild, "go build ./...", "module must still compile"},
	})
}

func TestSelectCapsPerPackageTargets(t *testing.T) {
	tree := t.TempDir()
	write(t, tree, "go.mod", "module example.com/wide\n\ngo 1.22\n")
	write(t, tree, "base/base.go", "package base\n\nfunc B() int { return 1 }\n")
	for i := 0; i < MaxGoTestTargets+2; i++ {
		dir := "p" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		write(t, tree, dir+"/x.go", "package "+dir+"\n\nimport \"example.com/wide/base\"\n\nvar X = base.B()\n")
	}
	plan := Select(context.Background(), tree, []string{"base/base.go"}, defaultPolicy(), nil)
	tests := 0
	for _, tg := range plan.Targets {
		if tg.Kind == evidence.CheckKindTest {
			tests++
			if !strings.HasPrefix(tg.Command, "go test -count=1 ./base ./paa ") || len(tg.Scope) != MaxGoTestTargets+3 {
				t.Fatalf("capped target = %+v", tg)
			}
		}
	}
	if tests != 1 {
		t.Fatalf("want one combined test target above the cap, got %d:\n%s", tests, dump(plan.Targets))
	}
}

func TestPlanNoteNamesFastChecksAndTheGate(t *testing.T) {
	plan := Select(context.Background(), fixture, []string{"contract/handlers.go"}, defaultPolicy(),
		[]ContractEdge{{ContractType: "rest_openapi", ContractName: "GET /widgets", ProducerFile: "contract/handlers.go", ConsumerRepo: "web", ConsumerFile: "a.ts"}})
	note := plan.Note()
	for _, s := range []string{"Fast checks for your change", "`go test -count=1 ./contract`", "owner of changed file contract/handlers.go", "`" + fullGo + "`) runs at the final gate", "Not run here: api_compat"} {
		if !strings.Contains(note, s) {
			t.Fatalf("note %q lacks %q", note, s)
		}
	}
}

func TestNormalizeFiles(t *testing.T) {
	got := NormalizeFiles([]string{"./b.go", `a\x.go`, "", "b.go", "../out.go", "/c.go"})
	if !reflect.DeepEqual(got, []string{"a/x.go", "b.go", "c.go"}) {
		t.Fatalf("NormalizeFiles = %v", got)
	}
}

func strPtr(s string) *string { return &s }

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(fixture, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(fixture, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestSelectOnThisRepository runs selection on the monorepo itself: a
// migration selects internal/db's migration tests, and a change to
// internal/evidence selects the implementation agent as a dependent.
func TestSelectOnThisRepository(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("repository root not found")
	}
	plan := Select(context.Background(), root, []string{"migrations/052_implementation_runs.up.sql"}, defaultPolicy(), nil)
	if plan.Fallback != "" {
		t.Fatalf("fallback: %s", plan.Fallback)
	}
	var mig *Target
	for i := range plan.Targets {
		if plan.Targets[i].Kind == evidence.CheckKindMigration {
			mig = &plan.Targets[i]
		}
	}
	if mig == nil || !strings.Contains(mig.Command, "./internal/db") {
		t.Fatalf("migration target = %+v, want internal/db's migration tests\n%s", mig, dump(plan.Targets))
	}
	plan = Select(context.Background(), root, []string{"internal/evidence/evidence.go"}, defaultPolicy(), nil)
	found := false
	for _, tg := range plan.Targets {
		for _, s := range tg.Scope {
			if s == "./cmd/implementation-agent" && tg.Kind == evidence.CheckKindTest {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("internal/evidence change does not select ./cmd/implementation-agent:\n%s", dump(plan.Targets))
	}
	t.Logf("migration: %s", mig.Command)
}
