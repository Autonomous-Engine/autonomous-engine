package plan

import (
	"fmt"
	"strings"
)

// Non-code criteria (R3). The planner drops steps that are not code work —
// process, deployment, operations: steps that name no repository file. A
// criterion only such steps cited cannot be owned by any code task, and
// blocking the plan on it cannot be repaired (the planner drops the step
// again). It is instead marked non-code, exempt from the ownership rule,
// and carried to review as a recorded gap a human verifies. A criterion
// also cited by a step dropped for another reason (test-only without a
// test runner, no repository) is code work and stays an orphan.

// NonCodeCriteria returns, by criterion id, why each criterion is non-code:
// it is owned by no kept task, it is cited by at least one dropped step, and
// every step that cites it was dropped as non-code work. A criterion's own
// NonCode flag is never trusted here: only the plan's steps decide, so a
// brief (model output) cannot exempt a criterion from ownership.
//
// R13 D3: a criterion whose text targets the PR text (TargetsPRText: the PR
// description, a commit message, release notes) that no kept step owns is
// non-code with PRTextReason, whether its citing steps were dropped (for any
// reason) or nothing cites it: pr-coordinator writes the PR text, no
// implementation task can. When a kept step owns it ("implement X and say so
// in the PR description") it stays an ordinary criterion the verifier
// judges; pr-coordinator lists its text in the PR description either way.
func NonCodeCriteria(brief BriefCriteria, tasks []TaskSpecWithFiles) map[string]string {
	out := map[string]string{}
	for _, c := range brief.Criteria {
		var citing []TaskSpecWithFiles
		owned := false
		for _, t := range tasks {
			if !containsString(t.Spec.CriterionIDs, c.ID) {
				continue
			}
			if t.Dropped == "" {
				owned = true
				break
			}
			citing = append(citing, t)
		}
		if owned {
			continue
		}
		if IsPRTextCriterion(c) {
			out[c.ID] = PRTextReason
			continue
		}
		if len(citing) == 0 {
			continue
		}
		allNonCode := true
		var by []string
		for _, t := range citing {
			if !t.NonCode {
				allNonCode = false
				break
			}
			by = append(by, fmt.Sprintf("%s, which the planner dropped (%s)", TaskLabel(t.Index, t.Title), t.Dropped))
		}
		if allNonCode {
			out[c.ID] = "cited only by " + strings.Join(by, " and by ")
		}
	}
	return out
}

// StripNonCode clears NonCode and NonCodeReason on every criterion of bc.
// Only the planner (MarkNonCode, after validation) may set them; every
// reader of model output or of a stored brief strips them on load.
func StripNonCode(bc *BriefCriteria) {
	if bc == nil {
		return
	}
	for i := range bc.Criteria {
		bc.Criteria[i].NonCode, bc.Criteria[i].NonCodeReason, bc.Criteria[i].Unowned = false, "", false
	}
}

// MarkNonCode returns a copy of criteria with every id in marks set
// non-code with its reason. criteria is not modified.
func MarkNonCode(criteria []Criterion, marks map[string]string) []Criterion {
	out := append([]Criterion(nil), criteria...)
	for i := range out {
		if reason, ok := marks[out[i].ID]; ok {
			out[i].NonCode, out[i].NonCodeReason = true, reason
		}
	}
	return out
}

// NonCodeGap is the line review sees for a non-code criterion. An unowned
// one (R8: owned by no task after planning, and no task's files cover it)
// says so and quotes the criterion.
func NonCodeGap(c Criterion) string {
	if c.Unowned {
		return UnownedGap(c)
	}
	reason := strings.TrimSpace(c.NonCodeReason)
	if reason == "" {
		reason = "no code task can satisfy it"
	}
	return fmt.Sprintf("criterion %s is non-code: %s; verify manually", c.ID, reason)
}

// UnownedGap is the line review sees for a criterion the planner could not
// give to any task (R8).
func UnownedGap(c Criterion) string {
	return fmt.Sprintf("criterion %s is unowned after planning: %s; verify manually", c.ID, strings.TrimSpace(c.Text))
}
