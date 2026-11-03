package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// Live: three criteria required a lint check, no step ran any lint
// command, the repair round did not add one, and the plan was rejected
// ("Retry from technical_brief state to re-decompose"). When the plan has
// no command of the required kind, the repository's own default (go vet
// for a Go module) is attached, marked as the planner's.
func TestDefaultLintCommandIsAttachedWhenThePlanHasNone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	brief := orphanBrief("A", "B")
	brief.Criteria[1].RequiredChecks = []evidence.CheckKind{evidence.CheckKindLint}
	steps := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "a.go"),
		orphanStep("B", []string{"AC-2"}, "b.go"),
	}
	base := capturedBuilder(brief)
	build := func(ts []decomposedTask) *planBuild {
		b := base(ts)
		if b != nil {
			b.statRoots = map[string]string{}
			for _, s := range b.activeSteps() {
				b.statRoots[s.RepositoryID] = root
			}
		}
		return b
	}
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		cp := copySteps(steps)
		for i := range cp {
			cp[i].Verification = append([]plan.VerificationStep(nil), steps[i].Verification...)
		}
		return cp, nil
	}, build)
	if out.decomposeErr != nil {
		t.Fatal(out.decomposeErr)
	}
	if out.rejected() || calls != 1 {
		t.Fatalf("rejected=%t calls=%d violations=%+v", out.rejected(), calls, out.violations)
	}
	var got *plan.VerificationStep
	for i, v := range out.build.raw[1].Verification {
		if v.CheckKind == evidence.CheckKindLint {
			got = &out.build.raw[1].Verification[i]
		}
	}
	if got == nil || got.Command != "go vet ./..." || got.AttachedBy != plan.AttachedByPlanner {
		t.Fatalf("task 2 verification = %+v, want go vet ./... attached by the planner", out.build.raw[1].Verification)
	}
}

// Live (ciblame #8, second run): the plan's steps ran scoped test commands
// (go test ./internal/report/...), so the normaliser had no single
// whole-suite command to attach to the step that owned AC-2, and the plan
// was rejected after the repair round. When the plan's own commands do not
// qualify, the repository's default test command is attached.
func TestDefaultTestCommandIsAttachedWhenThePlansCommandsAreScoped(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "x_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	brief := orphanBrief("A", "B")
	brief.Criteria[1].RequiredChecks = []evidence.CheckKind{evidence.CheckKindTest}
	steps := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "a.go"),
		orphanStep("B", []string{"AC-2"}, "b.go"),
	}
	// Step 1 runs the fixture's whole-suite command and a scoped one (two
	// candidates); step 2, owning AC-2, runs no test at all.
	steps[0].Verification = append(steps[0].Verification, plan.VerificationStep{CheckKind: evidence.CheckKindTest, Command: "go test ./internal/report/..."})
	steps[1].Verification = []plan.VerificationStep{{CheckKind: evidence.CheckKindOther, Command: "go run ./cmd/x --demo"}}
	base := capturedBuilder(brief)
	build := func(ts []decomposedTask) *planBuild {
		b := base(ts)
		if b != nil {
			b.statRoots = map[string]string{}
			for _, s := range b.activeSteps() {
				b.statRoots[s.RepositoryID] = root
			}
		}
		return b
	}
	calls := 0
	out := planWithRepair(uuid.Nil, func(string) ([]decomposedTask, error) {
		calls++
		cp := copySteps(steps)
		for i := range cp {
			cp[i].Verification = append([]plan.VerificationStep(nil), steps[i].Verification...)
		}
		return cp, nil
	}, build)
	if out.decomposeErr != nil {
		t.Fatal(out.decomposeErr)
	}
	if out.rejected() || calls != 1 {
		t.Fatalf("rejected=%t calls=%d violations=%+v", out.rejected(), calls, out.violations)
	}
	var got *plan.VerificationStep
	for i, v := range out.build.raw[1].Verification {
		if v.CheckKind == evidence.CheckKindTest {
			got = &out.build.raw[1].Verification[i]
		}
	}
	if got == nil || !strings.HasPrefix(got.Command, "go test ./...") || got.AttachedBy != plan.AttachedByPlanner {
		t.Fatalf("task 2 verification = %+v, want the repository's go test command attached by the planner", out.build.raw[1].Verification)
	}
}
