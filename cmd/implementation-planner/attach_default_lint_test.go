package main

import (
	"os"
	"path/filepath"
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
