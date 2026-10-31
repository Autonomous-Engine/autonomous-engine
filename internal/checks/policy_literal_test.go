package checks

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
)

// TestSelectLiteralInternalPolicyPicksOnlyTheInPolicyDependent: the policy
// is the literal `go test -short -tags unit ./internal/...`. A change in
// ./cmd/x (outside the policy's patterns) whose only reverse dependent is
// ./internal/y selects exactly ./internal/y, with -short and -tags unit
// preserved; ./cmd/x itself and the unrelated ./internal/z are not run.
func TestSelectLiteralInternalPolicyPicksOnlyTheInPolicyDependent(t *testing.T) {
	const policy = "go test -short -tags unit ./internal/..."
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/lit\n\ngo 1.22\n")
	write(t, root, "cmd/x/x.go", "package x\n\nfunc X() int { return 1 }\n")
	write(t, root, "cmd/x/x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")
	write(t, root, "internal/y/y.go", "package y\n\nimport \"example.com/lit/cmd/x\"\n\nfunc Y() int { return x.X() + 1 }\n")
	write(t, root, "internal/y/y_test.go", "//go:build unit\n\npackage y\n\nimport \"testing\"\n\nfunc TestY(t *testing.T) {}\n")
	write(t, root, "internal/z/z.go", "package z\n\nfunc Z() int { return 3 }\n")
	write(t, root, "internal/z/z_test.go", "package z\n\nimport \"testing\"\n\nfunc TestZ(t *testing.T) {}\n")

	p := PolicyFor(verifypolicy.Policy{TestCommand: strPtr(policy), RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest}}, "")
	plan := Select(context.Background(), root, []string{"cmd/x/x.go"}, p, nil)
	if plan.Fallback != "" {
		t.Fatalf("unexpected fallback: %s", plan.Fallback)
	}
	var tests []Target
	for _, tg := range plan.Executable() {
		if tg.Kind == evidence.CheckKindTest {
			tests = append(tests, tg)
		}
	}
	if len(tests) != 1 || tests[0].Command != "go test -short -tags unit ./internal/y" {
		t.Fatalf("test targets = %+v, want exactly `go test -short -tags unit ./internal/y`\nall:\n%s", tests, dump(plan.Targets))
	}
	for _, tg := range plan.Executable() {
		if tg.Kind == evidence.CheckKindTest && (strings.Contains(tg.Command, "./cmd/x") || strings.Contains(tg.Command, "./internal/z")) {
			t.Fatalf("out-of-policy or unimpacted package selected: %+v", tg)
		}
	}
	if len(plan.Full) == 0 || plan.Full[0].Command != policy {
		t.Fatalf("full suite = %+v, want the literal policy %q", plan.Full, policy)
	}
}
