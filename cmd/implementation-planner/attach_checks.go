package main

// verifier_missing's normaliser (R9, owner ruling). The most frequent
// violation of the early live runs was a criterion whose required check
// kind no owning step ran, although another step of the same repository
// ran the whole suite of that kind ("npm run test"). Before any re-prompt,
// the planner attaches that command to the owning step, when, and only
// when, it is unambiguous:
//
//   - the active steps of the owning repository run exactly one distinct,
//     non-empty command of the required kind (steps the planner attached
//     do not count);
//   - that command names no file path and no test filter: it runs the
//     whole suite, so it says nothing about any one criterion;
//   - a criterion-scoped violation attaches to the lowest-index step that
//     owns the criterion; a repository-policy violation to the group's
//     first step (whose steps, by definition, run no command of the kind,
//     so it never qualifies).
//
// The attached step is marked attached_by "planner", recorded on the plan
// (attached_checks in every subtask's message, a description section), and
// rendered to review as the planner's whole-suite run, never as evidence
// that a criterion is met. Anything else (two candidates, none, a command
// only another repository runs, a scoped command) is re-prompted with the
// hint.

import (
	"fmt"
	"log"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// attachedCheck records one check the planner attached
// (plan_tasks.message attached_checks).
type attachedCheck struct {
	// CriterionID is the criterion whose required kind it satisfies; ""
	// for a repository-policy kind.
	CriterionID string             `json:"criterion_id,omitempty"`
	TaskIndex   int                `json:"task_index"`
	TaskTitle   string             `json:"task_title,omitempty"`
	CheckKind   evidence.CheckKind `json:"check_kind"`
	Command     string             `json:"command"`
}

// testFilterFlags select part of a test suite (lower case; "--flag=value"
// is matched on the flag).
var testFilterFlags = map[string]bool{
	"-run": true, "-test.run": true, "-t": true, "-k": true, "-g": true, "-m": true,
	"--grep": true, "--filter": true, "--testnamepattern": true, "--test-name-pattern": true,
	"--testpathpattern": true, "--test-path-pattern": true, "--spec": true, "--tests": true,
	"--only": true, "--focus": true, "--project": true, "--selectprojects": true,
}

// wholeSuiteCommand reports whether cmd runs a whole suite: it is not empty,
// and no argument is a test filter, a node id ("a.py::test_x") or a path
// (a slash or backslash, or a repository file name) — except Go's "./...",
// which is the whole module.
func wholeSuiteCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields[1:] {
		if strings.Contains(f, "::") {
			return false
		}
		low := strings.ToLower(f)
		flag, val := low, ""
		if i := strings.Index(low, "="); i > 0 {
			flag, val = low[:i], low[i+1:]
		}
		if testFilterFlags[flag] {
			return false
		}
		for _, s := range []string{flag, val} {
			if namesPath(s) {
				return false
			}
		}
	}
	return true
}

func namesPath(s string) bool {
	s = strings.Trim(s, `"'`)
	if s == "" || s == "./..." || s == "..." {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	if strings.HasPrefix(s, "-") {
		return false
	}
	return isRepositoryFilePath(s)
}

// repositoryCommands are the distinct non-empty commands of kind k the
// active steps of repository repoID run, in step order; steps the planner
// attached do not count.
func repositoryCommands(b *planBuild, repoID string, k evidence.CheckKind) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range b.activeSteps() {
		if s.RepositoryID != repoID {
			continue
		}
		for _, v := range s.Spec.Verification {
			cmd := strings.TrimSpace(v.Command)
			if v.AttachedBy != "" || cmd == "" || plan.NormalizeCheckKind(v.CheckKind) != k || seen[cmd] {
				continue
			}
			seen[cmd] = true
			out = append(out, cmd)
		}
	}
	return out
}

// checkTarget is the step a verifier_missing violation is about: the
// lowest-index active step that owns the criterion, or, for a repository
// policy kind, the group's first step (v.TaskIndex).
func checkTarget(b *planBuild, v plan.Violation) (plan.TaskSpecWithFiles, bool) {
	if v.CriterionID == "" {
		return b.stepByIndex(v.TaskIndex)
	}
	for _, s := range b.activeSteps() { // in step order
		for _, id := range s.Spec.CriterionIDs {
			if id == v.CriterionID {
				return s, true
			}
		}
	}
	return plan.TaskSpecWithFiles{}, false
}

// attachChecks is verifier_missing's normaliser: it attaches the owning
// repository's one whole-suite command to each violation's target step
// where the rule above allows, rebuilds the plan and validates it again.
// Violations it cannot settle are left for the re-prompt.
func attachChecks(workID uuid.UUID, b *planBuild, vs []plan.Violation, build func([]decomposedTask) *planBuild) (*planBuild, []plan.Violation) {
	if b == nil {
		return b, vs
	}
	raw := cloneRaw(b.raw)
	byIndex := map[int]int{}
	for i, t := range b.raw {
		byIndex[t.index] = i
	}
	type stepKind struct {
		index int
		kind  evidence.CheckKind
	}
	added := map[stepKind]bool{}
	var attached []attachedCheck
	for _, v := range plan.Errors(vs) {
		if v.Code != plan.CodeVerifierMissing {
			continue
		}
		k := plan.NormalizeCheckKind(v.CheckKind)
		target, ok := checkTarget(b, v)
		if k == "" || !ok {
			continue
		}
		cmds := repositoryCommands(b, target.RepositoryID, k)
		if len(cmds) != 1 || !wholeSuiteCommand(cmds[0]) {
			continue
		}
		i, ok := byIndex[target.Index]
		if !ok {
			continue
		}
		if key := (stepKind{target.Index, k}); !added[key] {
			added[key] = true
			raw[i].Verification = append(raw[i].Verification, plan.VerificationStep{CheckKind: k, Command: cmds[0], AttachedBy: plan.AttachedByPlanner})
		}
		attached = append(attached, attachedCheck{CriterionID: v.CriterionID, TaskIndex: target.Index, TaskTitle: target.Title, CheckKind: k, Command: cmds[0]})
		what := v.CriterionID
		if what == "" {
			what = "the repository's verification policy"
		}
		log.Printf("implementation-planner: work_id=%s: %s check %q attached to task %d for %s (the repository's one whole-suite %s command)", workID, k, cmds[0], target.Index, what, k)
	}
	if len(attached) == 0 {
		return b, vs
	}
	nb := build(raw)
	if nb == nil || nb.noRepository || len(nb.planned) == 0 {
		return b, vs
	}
	nb.rehomed = append([]rehomedCriterion(nil), b.rehomed...)
	nb.unowned = append([]string(nil), b.unowned...)
	nb.attached = append(append([]attachedCheck(nil), b.attached...), attached...)
	nvs := nb.validate()
	countViolations(nvs)
	for _, v := range nvs {
		log.Printf("implementation-planner: work_id=%s: plan validation after attaching checks: %s [%s] %s", workID, v.Severity, v.Code, v.Message)
	}
	return nb, nvs
}

// cloneRaw copies the steps a normaliser changes (criterion ids,
// verification), so the model's plan stays as it was sent: a plan blocked
// after normalising records the model's plan, not the planner's edits.
func cloneRaw(raw []decomposedTask) []decomposedTask {
	out := make([]decomposedTask, len(raw))
	for i, t := range raw {
		t.CriterionIDs = append([]string(nil), t.CriterionIDs...)
		t.Verification = append([]plan.VerificationStep(nil), t.Verification...)
		out[i] = t
	}
	return out
}

// normalisePlan runs every normaliser of the policy's normalise codes, in
// order: orphans are re-homed first (a re-homed criterion may need its
// checks on its new step), then missing checks are attached.
func normalisePlan(workID uuid.UUID, b *planBuild, vs []plan.Violation, build func([]decomposedTask) *planBuild) (*planBuild, []plan.Violation) {
	b, vs = rehomeOrphans(workID, b, vs, build)
	return attachChecks(workID, b, vs, build)
}

// attachAttachedChecks records the attached checks on every subtask
// (attached_checks), and adds a note to the subtask whose step received
// one: the command proves the kind is run, not that the criterion is met.
func attachAttachedChecks(b *planBuild) {
	if len(b.attached) == 0 {
		return
	}
	for i := range b.planned {
		b.planned[i].msg.AttachedChecks = append([]attachedCheck(nil), b.attached...)
		var lines []string
		for _, a := range b.attached {
			for _, st := range b.planned[i].stepTasks {
				if st.index != a.TaskIndex {
					continue
				}
				who := "The repository's verification policy"
				tail := "it is a whole-suite run."
				if a.CriterionID != "" {
					who = a.CriterionID
					tail = fmt.Sprintf("it is a whole-suite run, not evidence that %s is met.", a.CriterionID)
				}
				lines = append(lines, fmt.Sprintf("- %s required a %s check that step %q did not plan; the planner attached `%s`, the one %s command the repository's steps run: %s",
					who, a.CheckKind, st.Title, a.Command, a.CheckKind, tail))
			}
		}
		if len(lines) == 0 {
			continue
		}
		section := "\n## Planner-attached verification\n\n" + strings.Join(lines, "\n") + "\n"
		b.planned[i].msg.Description += section
		if b.planned[i].item != nil {
			b.planned[i].item.Description += section
		}
	}
}
