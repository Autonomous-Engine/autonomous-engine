package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// R9 (owner ruling): verifier_missing is a normalise code. Before any
// re-prompt the planner attaches the owning repository's one whole-suite
// command of the required kind to the lowest-index owning step, marks it
// attached_by "planner", and records it; anything less clear-cut is
// re-prompted with the hint.

// buildSteps is a brief whose AC-2 requires a build check, and two steps:
// step 1 runs cmd1 as its build check, step 2 (owning AC-2) has only a test.
func buildSteps(cmd1 string) (*plan.BriefCriteria, []decomposedTask) {
	brief := orphanBrief("A", "B")
	brief.Criteria[1].RequiredChecks = []evidence.CheckKind{evidence.CheckKindBuild}
	steps := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	}
	steps[0].Verification = append(steps[0].Verification, plan.VerificationStep{CheckKind: evidence.CheckKindBuild, Command: cmd1})
	return brief, steps
}

func planCounting(t *testing.T, brief *plan.BriefCriteria, steps []decomposedTask) (planOutcome, int, string) {
	t.Helper()
	calls := 0
	var repair string
	out := planWithRepair(uuid.Nil, func(r string) ([]decomposedTask, error) {
		calls++
		if r != "" {
			repair = r
		}
		cp := copySteps(steps)
		for i := range cp {
			cp[i].Verification = append([]plan.VerificationStep(nil), steps[i].Verification...)
		}
		return cp, nil
	}, capturedBuilder(brief))
	if out.decomposeErr != nil {
		t.Fatal(out.decomposeErr)
	}
	return out, calls, repair
}

func TestOneWholeSuiteCommandIsAttachedWithoutARePrompt(t *testing.T) {
	brief, steps := buildSteps("npm run build")
	out, calls, _ := planCounting(t, brief, steps)
	if out.rejected() || calls != 1 {
		t.Fatalf("rejected=%t calls=%d violations=%+v", out.rejected(), calls, out.violations)
	}
	b := out.build
	var attached *plan.VerificationStep
	for i, v := range b.raw[1].Verification {
		if v.CheckKind == evidence.CheckKindBuild {
			attached = &b.raw[1].Verification[i]
		}
	}
	if attached == nil || attached.Command != "npm run build" || attached.AttachedBy != plan.AttachedByPlanner {
		t.Fatalf("task 2 verification = %+v, want npm run build attached by the planner", b.raw[1].Verification)
	}
	for _, v := range b.raw[0].Verification {
		if v.AttachedBy != "" {
			t.Errorf("task 1's own step is marked: %+v", v)
		}
	}
	want := attachedCheck{CriterionID: "AC-2", TaskIndex: 2, TaskTitle: "B", CheckKind: evidence.CheckKindBuild, Command: "npm run build"}
	if len(b.attached) != 1 || b.attached[0] != want {
		t.Fatalf("attached = %+v, want %+v", b.attached, want)
	}

	// It reaches plan_tasks: the stored message's task_spec carries the
	// marked step, attached_checks records it, and the description says it
	// is not evidence for the criterion.
	attachNonCodeCriteria(b)
	msg := b.planned[0].msg
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		TaskSpec       *plan.TaskSpec  `json:"task_spec"`
		AttachedChecks []attachedCheck `json:"attached_checks"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	var marked bool
	for _, v := range stored.TaskSpec.Verification {
		marked = marked || (v.Command == "npm run build" && v.AttachedBy == plan.AttachedByPlanner)
	}
	if !marked || len(stored.AttachedChecks) != 1 || stored.AttachedChecks[0] != want {
		t.Fatalf("stored message: task_spec %+v attached_checks %+v", stored.TaskSpec.Verification, stored.AttachedChecks)
	}
	for _, w := range []string{"## Planner-attached verification", "AC-2 required a build check", "`npm run build`", "not evidence that AC-2 is met"} {
		if !strings.Contains(msg.Description, w) {
			t.Errorf("description lacks %q:\n%s", w, msg.Description)
		}
	}
}

// Two distinct commands of the kind: which one covers the criterion is the
// model's call.
func TestTwoDistinctCommandsStillRePrompt(t *testing.T) {
	brief, steps := buildSteps("npm run build")
	steps = append(steps, orphanStep("C", []string{"AC-1"}, "src/c.ts"))
	steps[2].Verification = append(steps[2].Verification, plan.VerificationStep{CheckKind: evidence.CheckKindBuild, Command: "tsc -b"})
	out, calls, repair := planCounting(t, brief, steps)
	if !out.rejected() || calls != 2 || len(out.build.attached) != 0 {
		t.Fatalf("rejected=%t calls=%d attached=%+v", out.rejected(), calls, out.build.attached)
	}
	if !strings.Contains(repair, `AC-2 needs a build step: the plan runs "npm run build" and "tsc -b" as build checks; add the one that checks AC-2 to the verification of task 2, which cites AC-2`) {
		t.Fatalf("repair prompt:\n%s", repair)
	}
}

// A command only another repository runs is not used, in the attachment or
// the hint.
func TestACommandInAnotherRepositoryIsNotUsed(t *testing.T) {
	brief, steps := buildSteps("npm run build")
	steps[0].RepositoryID = "web"
	steps[1].RepositoryID = "api"
	out, calls, repair := planCounting(t, brief, steps)
	if !out.rejected() || calls != 2 || len(out.build.attached) != 0 {
		t.Fatalf("rejected=%t calls=%d attached=%+v", out.rejected(), calls, out.build.attached)
	}
	if strings.Contains(repair, "npm run build") && strings.Contains(repair, "the plan already runs") {
		t.Fatalf("the hint offers another repository's command:\n%s", repair)
	}
	if !strings.Contains(repair, `AC-2 needs a build step: add a verification step with "check_kind":"build" and the command that runs the build to task 2, which cites AC-2`) {
		t.Fatalf("repair prompt:\n%s", repair)
	}
}

// A path-scoped or filtered command checks part of the suite: not used.
func TestAPathScopedCommandIsNotUsed(t *testing.T) {
	brief, steps := buildSteps("npm run build -- src/a.ts")
	out, calls, _ := planCounting(t, brief, steps)
	if !out.rejected() || calls != 2 || len(out.build.attached) != 0 {
		t.Fatalf("rejected=%t calls=%d attached=%+v", out.rejected(), calls, out.build.attached)
	}
}

func TestWholeSuiteCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"npm test":                   true,
		"npm run test":               true,
		"npm run build":              true,
		"go test ./...":              true,
		"go build ./...":             true,
		"make test":                  true,
		"pytest":                     true,
		"cargo test":                 true,
		"":                           false,
		"go test ./pkg/...":          false,
		"go test -run TestTax ./...": false,
		"npx vitest run tests/unit/server.test.ts": false,
		"npm test -- -t notice":                    false,
		"pytest tests/test_api.py":                 false,
		"pytest -k notice":                         false,
		"pytest tests/a.py::test_x":                false,
		"jest --testNamePattern=x":                 false,
		"npx vitest --run --grep foo":              false,
		"go test calc_test.go":                     false,
		`go test .\pkg`:                            false,
	} {
		if got := wholeSuiteCommand(cmd); got != want {
			t.Errorf("wholeSuiteCommand(%q) = %t, want %t", cmd, got, want)
		}
	}
}

// The mark is the planner's: a model cannot claim it.
func TestModelAttachedByIsCleared(t *testing.T) {
	tasks, err := parseTaskArray(`[{"title":"A","criterion_ids":["AC-1"],"deliverable":"d","verification":[{"check_kind":"test","command":"npm test","attached_by":"planner"}],"files":[{"path":"src/a.ts","evidence_ref":"hypothesis"}]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if v := tasks[0].Verification[0]; v.AttachedBy != "" {
		t.Fatalf("attached_by from the model was kept: %+v", v)
	}
}

// A repository-policy violation names no criterion; its candidates are the
// policy group's steps, which by definition run no command of the kind, so
// it is never attached and always re-prompted.
func TestPolicyViolationIsNeverAttached(t *testing.T) {
	brief := orphanBrief("A", "B")
	steps := []decomposedTask{
		orphanStep("A", []string{"AC-1"}, "src/a.ts"),
		orphanStep("B", []string{"AC-2"}, "src/b.ts"),
	}
	for i := range steps {
		steps[i].Verification = []plan.VerificationStep{{CheckKind: evidence.CheckKindBuild, Command: "npm run build"}}
	}
	out, calls, repair := planCounting(t, brief, steps)
	if !out.rejected() || calls != 2 || len(out.build.attached) != 0 {
		t.Fatalf("rejected=%t calls=%d attached=%+v", out.rejected(), calls, out.build.attached)
	}
	if !strings.Contains(repair, "verification policy requires a test check") {
		t.Fatalf("repair prompt:\n%s", repair)
	}
}
