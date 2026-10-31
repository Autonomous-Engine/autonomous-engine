package checks

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
)

// TestFastSetNeverStricterThanTheGate: every fast target keeps the policy
// command's non-package arguments, stays within its package patterns, and
// go vet / go build are advisory unless the policy requires them.
func TestFastSetNeverStricterThanTheGate(t *testing.T) {
	sel := func(full string, required []evidence.CheckKind, changed ...string) Plan {
		t.Helper()
		p := PolicyFor(verifypolicy.Policy{TestCommand: strPtr(full), RequiredChecks: required}, "")
		return Select(context.Background(), fixture, changed, p, nil)
	}
	testOnly := []evidence.CheckKind{evidence.CheckKindTest}

	t.Run("policy flags are preserved in every test target", func(t *testing.T) {
		plan := sel("go test -short -tags integration -race -timeout 5m ./...", testOnly, "core/core.go")
		if plan.Fallback != "" {
			t.Fatal(plan.Fallback)
		}
		assertTargets(t, plan.Targets, []want{
			{evidence.CheckKindTest, "go test -short -tags integration -race -timeout 5m ./core", "owner of changed file core/core.go"},
			{evidence.CheckKindTest, "go test -short -tags integration -race -timeout 5m ./api", "reverse dependent"},
			{evidence.CheckKindTest, "go test -short -tags integration -race -timeout 5m ./svc", "reverse dependent"},
			{evidence.CheckKindTest, "go test -short -tags integration -race -timeout 5m ./tools", "tests import ./core"},
			{evidence.CheckKindBuild, "go build -tags integration -race ./...", "module must still compile"},
			{evidence.CheckKindLint, "go vet -tags integration ./core", "vet the changed package"},
		})
		b := plan.Batches()
		if len(b) != 3 || b[0].Command != "go test -short -tags integration -race -timeout 5m ./core ./api ./svc ./tools" || len(b[0].Targets) != 4 {
			t.Fatalf("batches = %+v, want the four test targets in one invocation", b)
		}
		if b[0].Advisory || !b[1].Advisory || !b[2].Advisory {
			t.Fatalf("advisory = %v %v %v, want test enforced, build and vet advisory", b[0].Advisory, b[1].Advisory, b[2].Advisory)
		}
		if !strings.Contains(plan.Note(), "advisory: reported, never fails the run") {
			t.Fatalf("note does not mark advisory checks: %s", plan.Note())
		}
	})

	t.Run("package patterns restrict the selection", func(t *testing.T) {
		plan := sel("go test -count=1 ./core/... ./svc", testOnly, "core/core.go")
		assertTargets(t, plan.Targets, []want{
			{evidence.CheckKindTest, "go test -count=1 ./core", "owner of changed file core/core.go"},
			{evidence.CheckKindTest, "go test -count=1 ./svc", "reverse dependent of ./core"},
			{evidence.CheckKindBuild, "go build ./core/... ./svc", "module must still compile"},
			{evidence.CheckKindLint, "go vet ./core", "vet the changed package"},
		})
		// A change only in an excluded package selects nothing outside the
		// policy: the policy's own suite runs.
		plan = sel("go test -count=1 ./core/... ./svc", testOnly, "leaf/leaf.go")
		for _, tg := range plan.Executable() {
			if strings.Contains(tg.Command, "./leaf") {
				t.Fatalf("excluded package selected: %+v", tg)
			}
		}
		if ex := plan.Executable(); ex[len(ex)-1].Kind != evidence.CheckKindTest || ex[len(ex)-1].Command != "go test -count=1 ./core/... ./svc" {
			t.Fatalf("want the policy suite, got\n%s", dump(plan.Targets))
		}
		// Import-path patterns of the module work too.
		plan = sel("go test example.com/fixture/svc/...", testOnly, "core/core.go")
		if plan.Fallback != "" || plan.Targets[0].Command != "go test ./svc" {
			t.Fatalf("import-path pattern: fallback %q targets\n%s", plan.Fallback, dump(plan.Targets))
		}
	})

	t.Run("required lint and build are enforced", func(t *testing.T) {
		plan := sel("go test ./...", []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindLint, evidence.CheckKindBuild}, "leaf/leaf_test.go")
		var kinds []string
		for _, tg := range plan.Targets {
			if tg.Advisory {
				t.Fatalf("required kind is advisory: %+v", tg)
			}
			kinds = append(kinds, string(tg.Kind)+":"+tg.Command)
		}
		if want := []string{"test:go test ./leaf", "build:go build ./...", "lint:go vet ./leaf"}; !reflect.DeepEqual(kinds, want) {
			t.Fatalf("targets = %v, want %v", kinds, want)
		}
	})

	t.Run("migration target keeps flags and asks for -v", func(t *testing.T) {
		plan := sel("go test -short ./...", nil, "migrations/001_init.up.sql")
		if plan.Targets[0].Command != "go test -short -v -run Migrat ./db" {
			t.Fatalf("migration target = %+v", plan.Targets[0])
		}
	})

	for _, tc := range []struct{ cmd, why string }{
		{"go test -mystery ./...", "cannot place"},
		{"go test std", "cannot be matched"},
		{"go test github.com/other/mod/...", "cannot be matched"},
		{"go test ./... | tee out", "not a plain go test"},
	} {
		t.Run("fallback: "+tc.cmd, func(t *testing.T) {
			plan := sel(tc.cmd, testOnly, "core/core.go")
			if !strings.Contains(plan.Fallback, tc.why) || plan.Executable()[0].Command != tc.cmd {
				t.Fatalf("fallback %q, targets\n%s", plan.Fallback, dump(plan.Targets))
			}
		})
	}
}

func TestParseGoTest(t *testing.T) {
	c, err := parseGoTest("go test -count=1 -run TestX -v ./a/... ./b -args -foo bar")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.flags, []string{"-count=1", "-run", "TestX", "-v"}) ||
		!reflect.DeepEqual(c.patterns, []string{"./a/...", "./b"}) ||
		!reflect.DeepEqual(c.trailing, []string{"-args", "-foo", "bar"}) {
		t.Fatalf("parsed %+v", c)
	}
	if got := goCommand("test", c.flags, []string{"./a"}, c.trailing); got != "go test -count=1 -run TestX -v ./a -args -foo bar" {
		t.Fatalf("command %q", got)
	}
	c, _ = parseGoTest("go test")
	if !reflect.DeepEqual(c.patterns, []string{"."}) {
		t.Fatalf("no patterns -> %v, want [.]", c.patterns)
	}
	m, err := newPatternMatcher([]string{"./..."}, "example.com/m")
	if err != nil || !m.matches(".") || !m.matches("./x") {
		t.Fatalf("./... must match everything: %v", err)
	}
	m, _ = newPatternMatcher([]string{"./internal/..."}, "example.com/m")
	for dir, want := range map[string]bool{"./internal": true, "./internal/a/b": true, "./internalx": false, ".": false, "./cmd": false} {
		if m.matches(dir) != want {
			t.Fatalf("./internal/... matches(%s) = %v", dir, !want)
		}
	}
}
