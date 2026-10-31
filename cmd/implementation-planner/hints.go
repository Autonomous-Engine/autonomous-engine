package main

// Repair hints (R9 P1). A reprompt code goes back to the model with its
// message verbatim; a hint under it says how to fix it with what the plan
// already holds. R8 added the orphan hint (the paths a criterion names and
// the tasks that change them); R9 adds hints for a missing check
// (verifier_missing), a missing deliverable (no_deliverable) and a step
// whose every file is a hypothesis (hypothesis_only_task, a warning the
// prompt mentions when a round runs anyway).

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
)

// repairHints is, by plan.Violation.Key, the sentence the repair prompt
// writes under each violation that has one.
func repairHints(b *planBuild, vs []plan.Violation) map[string]string {
	hints := map[string]string{}
	if b == nil {
		return hints
	}
	orphans := orphanRepairHints(b, vs)
	for _, v := range vs {
		var h string
		switch v.Code {
		case plan.CodeOrphanCriterion:
			h = orphans[v.CriterionID]
		case plan.CodeVerifierMissing:
			h = verifierMissingHint(b, v)
		case plan.CodeNoDeliverable:
			h = noDeliverableHint(b, v)
		case plan.CodeHypothesisOnlyTask:
			h = hypothesisOnlyHint(b, v)
		}
		if h != "" {
			hints[v.Key()] = h
		}
	}
	return hints
}

// activeSteps are the steps that were not dropped.
func (b *planBuild) activeSteps() []plan.TaskSpecWithFiles {
	var out []plan.TaskSpecWithFiles
	for _, s := range b.specs {
		if s.Dropped == "" {
			out = append(out, s)
		}
	}
	return out
}

// stepByIndex is the step with that 1-based index.
func (b *planBuild) stepByIndex(i int) (plan.TaskSpecWithFiles, bool) {
	for _, s := range b.specs {
		if s.Index == i {
			return s, true
		}
	}
	return plan.TaskSpecWithFiles{}, false
}

// verifierMissingHint names the step to add the check to and, when the plan
// already runs a command of that kind, the step to add.
func verifierMissingHint(b *planBuild, v plan.Violation) string {
	k := plan.NormalizeCheckKind(v.CheckKind)
	if k == "" {
		return ""
	}
	active := b.activeSteps()
	var owners []plan.TaskSpecWithFiles
	if v.CriterionID != "" {
		for _, s := range active {
			for _, id := range s.Spec.CriterionIDs {
				if id == v.CriterionID {
					owners = append(owners, s)
					break
				}
			}
		}
	} else if s, ok := b.stepByIndex(v.TaskIndex); ok {
		for _, o := range active {
			if o.RepositoryID == s.RepositoryID {
				owners = append(owners, o)
			}
		}
	}
	if len(owners) == 0 {
		return ""
	}
	where := fmt.Sprintf("task %d", owners[0].Index)
	switch {
	case v.CriterionID != "" && len(owners) == 1:
		where += ", which cites " + v.CriterionID
	case v.CriterionID != "":
		where += fmt.Sprintf(" (or task %s; they cite %s)", stepNumbers(owners[1:]), v.CriterionID)
	default:
		where += fmt.Sprintf(" (or any task in repository %s)", owners[0].Repository)
	}
	subject := v.CriterionID
	if subject == "" {
		subject = "The repository"
	}
	// Only the owning repository's commands (a command another repository
	// runs does not check this one).
	cmds := repositoryCommands(b, owners[0].RepositoryID, k)
	if len(cmds) > 1 {
		quoted := make([]string, 0, len(cmds))
		for _, c := range cmds {
			quoted = append(quoted, fmt.Sprintf("%q", c))
		}
		which := "the one that checks " + v.CriterionID
		if v.CriterionID == "" {
			which = "the one the policy needs"
		}
		return fmt.Sprintf("%s needs a %s step: the plan runs %s as %s checks; add %s to the verification of %s",
			subject, k, joinWords(quoted), k, which, where)
	}
	if len(cmds) == 1 {
		cmd := cmds[0]
		var step strings.Builder
		enc := json.NewEncoder(&step)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(struct {
			CheckKind evidence.CheckKind `json:"check_kind"`
			Command   string             `json:"command"`
		}{k, cmd})
		return fmt.Sprintf("%s needs a %s step: add %s to the verification of %s (the plan already runs %q as a %s check)",
			subject, k, strings.TrimSpace(step.String()), where, cmd, k)
	}
	return fmt.Sprintf(`%s needs a %s step: add a verification step with "check_kind":"%s" and the command that runs the %s to %s`,
		subject, k, k, k, where)
}

// noDeliverableHint offers the criterion the step cites as the model of its
// deliverable.
func noDeliverableHint(b *planBuild, v plan.Violation) string {
	s, ok := b.stepByIndex(v.TaskIndex)
	if !ok {
		return ""
	}
	for _, id := range s.Spec.CriterionIDs {
		if c, ok := b.criterionByID(id); ok && strings.TrimSpace(c.Text) != "" {
			return fmt.Sprintf(`set "deliverable" of task %d to the observable result it produces, for example the criterion it cites: %q (%s)`,
				s.Index, strings.Join(strings.Fields(c.Text), " "), c.ID)
		}
	}
	return fmt.Sprintf(`set "deliverable" of task %d to one sentence saying what is observably different once %q is done`, s.Index, s.Title)
}

// hypothesisOnlyHint names, for a step whose every file is a hypothesis,
// the evidence the brief's File Evidence table has for its paths.
func hypothesisOnlyHint(b *planBuild, v plan.Violation) string {
	s, ok := b.stepByIndex(v.TaskIndex)
	if !ok {
		return ""
	}
	var table map[string]plan.EvidenceRef
	if b.criteria != nil {
		table = b.criteria.FileEvidence
	}
	var parts []string
	var unknown []string
	for _, f := range s.Spec.Files {
		if ev, ok := table[f.Path]; ok && ev.Kind.Supported() {
			parts = append(parts, fmt.Sprintf("%s is in the File Evidence table: use evidence_ref %q for it", f.Path, string(ev.Kind)+":"+ev.Ref))
			continue
		}
		unknown = append(unknown, f.Path)
	}
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf(`mark %s "new" if task %d creates it, or name the file that really holds the change`, joinWords(unknown), s.Index))
	}
	return strings.Join(parts, "; ")
}
