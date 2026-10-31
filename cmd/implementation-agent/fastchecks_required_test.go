package main

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
)

// TestFastLoopRequiredLintVetFindingFailsTheRun is the counterpart of
// TestFastLoopPreExistingVetFindingIsAdvisory: when the repository policy
// requires lint, the go vet target is enforced, so the same vet finding is
// a run failure, reported to the repair loop and recorded as a failed lint
// check.
func TestFastLoopRequiredLintVetFindingFailsTheRun(t *testing.T) {
	f := newFastFixtureWith(t, map[string]string{
		"a/vet.go": "package a\n\n// U has unreachable code: a go vet finding outside go test's vet subset.\nfunc U() int {\n\treturn 1\n\tprintln(\"never\")\n\treturn 2\n}\n",
	}, fastFixtureFullSuite)
	src := &staticPolicySource{found: true, policy: verifypolicy.Policy{
		RequiredChecks: []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindLint},
		Source:         verifypolicy.SourceAdmin,
	}}
	ctx := context.Background()
	fast := startFastChecks(ctx, fastCheckDeps{
		worktree: f.worktree, baseSHA: f.baseSHA, taskID: uuid.New(), workID: uuid.New(),
		repositoryID: uuid.NewString(), policies: src,
		testCommand: f.testCommand, artifacts: f.artifacts, logger: logging.New("implementation-agent-test"),
	})
	if fast == nil {
		t.Fatal("startFastChecks returned nil")
	}
	if len(src.asked) != 1 {
		t.Fatalf("policy source asked %d times, want 1", len(src.asked))
	}
	writeFile(t, f.worktree, "a/a.go", "package a\n\nfunc A() int { return 10 }\n")
	out, err := fast.run(ctx)
	if err == nil {
		t.Fatalf("required lint with a vet finding did not fail the fast run:\n%s", out)
	}
	// Build stays advisory (not required); the vet batch must not be.
	if !strings.Contains(string(out), "unreachable code") || !strings.Contains(string(out), "# lint: vet") ||
		strings.Contains(string(out), "lint, advisory") || strings.Contains(string(out), "[advisory failed") {
		t.Fatalf("vet finding not reported as an enforced failure:\n%s", out)
	}
	var lint []evidence.CheckResult
	for _, c := range fast.failedChecks("ref") {
		if c.Kind == evidence.CheckKindLint {
			lint = append(lint, c)
		}
	}
	if len(lint) != 1 || lint[0].Status != evidence.CheckFailed || !strings.HasPrefix(lint[0].Command, "go vet") {
		t.Fatalf("failed checks = %+v, want one failed go vet lint check", fast.failedChecks("ref"))
	}
	var vetResult bool
	for _, r := range fast.failureResults() {
		if strings.HasPrefix(r.Command, "go vet") {
			vetResult = true
		}
	}
	if !vetResult {
		t.Fatalf("failure results %+v omit the go vet failure the repair loop must see", fast.failureResults())
	}
}
