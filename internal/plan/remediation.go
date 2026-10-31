package plan

// The remediation policy (R9). Live-run attempts 5, 7 and 8 each
// blocked in planning on a defect the planner could have repaired
// mechanically; R3, R7 and R8 each fixed one violation code with the same
// principle: normalise when the fix is unambiguous, otherwise record a gap
// for human review, and block only for a structurally bad plan. This table
// makes the principle explicit for every code, so a new code cannot fall
// through to "block" by default: TestEveryViolationCodeHasARemediation fails
// for a code without an entry.
//
// The planner (cmd/implementation-planner, planWithPolicy) dispatches on it:
//
//   - normalise: the planner's own normaliser runs before any re-prompt
//     (orphan_criterion: R8's re-home by file overlap; verifier_missing: the
//     repository's one whole-suite command of that kind, attached and marked
//     as the planner's). What it cannot fix unambiguously takes the code's
//     Fallback: reprompt (the model, with a hint) or gap (recorded after the
//     last round; orphans within the R8 thresholds).
//   - reprompt: the violation's message, verbatim, plus a targeted hint goes
//     back to the model in the repair round. Still present after the last
//     round, it blocks the parent with an escalation prompt.
//   - gap: recorded on the plan (subtask description, non_code_criteria) and
//     never blocks; these codes are warnings.
//   - block: no repair is possible; the parent is blocked with an
//     escalation prompt at once. No violation code is a block today: the
//     planner's hard blocks (an empty brief, an unparseable decomposition,
//     no repository, nothing but non-code work, an invalid dependency
//     graph) are not violations of a plan and have their own table in the
//     planner.

import (
	"fmt"
	"strings"
)

// Remediation is how the planner answers a violation.
type Remediation string

const (
	RemediationNormalise Remediation = "normalise"
	RemediationReprompt  Remediation = "reprompt"
	RemediationGap       Remediation = "gap"
	RemediationBlock     Remediation = "block"
)

// Policy is one code's remediation and the reason for it.
type Policy struct {
	Remediation Remediation
	// Rationale is one line: why this remediation.
	Rationale string
	// Fallback is what happens to a normalise code the normaliser could
	// not fix unambiguously: reprompt or gap. Empty for other codes.
	Fallback Remediation
}

// RemediationPolicy is the remediation of every violation code.
var RemediationPolicy = map[ViolationCode]Policy{
	CodeOrphanCriterion: {Remediation: RemediationNormalise, Fallback: RemediationGap,
		Rationale: "re-homed to the step whose files contain every path the criterion names (R8); otherwise re-prompted with the candidate steps, recorded as an unowned gap after the last round, and blocked above the R8 thresholds"},
	CodeUnknownCriterion: {Remediation: RemediationReprompt,
		Rationale: "only the model knows which listed criterion it meant; the message lists the known ids"},
	CodeNoCriteria: {Remediation: RemediationReprompt,
		Rationale: "the model must say which criteria the task serves, or add a derived criterion with a reason"},
	CodeNoDeliverable: {Remediation: RemediationReprompt,
		Rationale: "the deliverable is the model's statement of the task's result; the hint offers the criterion the task cites as a model for it"},
	CodeEmptyVerification: {Remediation: RemediationReprompt,
		Rationale: "the planner cannot invent how a task is checked"},
	CodeVerifierMissing: {Remediation: RemediationNormalise, Fallback: RemediationReprompt,
		Rationale: "when the owning repository's steps run exactly one whole-suite command of that kind, it is attached to the lowest-index owning step, marked attached_by planner and never evidence for a criterion; otherwise re-prompted with the owners and the commands"},
	CodeUnevidencedPath: {Remediation: RemediationReprompt,
		Rationale: "the model must copy the path's evidence_ref from the File Evidence table or mark it a hypothesis"},
	CodeStaleEvidence: {Remediation: RemediationReprompt,
		Rationale: "the model must drop the path, or mark it new or a hypothesis"},
	CodeUnknownDependency: {Remediation: RemediationReprompt,
		Rationale: "only the model knows which task it meant to depend on; the message lists the titles"},
	CodeDependencyCycle: {Remediation: RemediationReprompt,
		Rationale: "the model must remove one depends_on edge of the cycle"},
	CodeOneSidedContract: {Remediation: RemediationReprompt,
		Rationale: "a task on the other side of the cross-repository contract must be planned, or the contract corrected (M2-06)"},
	CodeHypothesisOnlyTask: {Remediation: RemediationGap,
		Rationale: "planned and shown in the subtask description, so the implementer confirms the files first; the repair prompt mentions it when a round runs anyway"},
	CodeNonCodeCriterion: {Remediation: RemediationGap,
		Rationale: "recorded on the plan; review records it as not_verifiable and escalates to a human (R3, R8)"},
}

// RemediationFor is code's remediation; false for an unknown code.
func RemediationFor(code ViolationCode) (Policy, bool) {
	p, ok := RemediationPolicy[code]
	return p, ok
}

// remediationOf is code's remediation, an unknown code re-prompted (the
// model sees the message; the plan is never accepted with it).
func remediationOf(code ViolationCode) Remediation {
	if p, ok := RemediationPolicy[code]; ok {
		return p.Remediation
	}
	return RemediationReprompt
}

// RemediationOf is the remediation of one violation (see remediationOf).
func RemediationOf(v Violation) Remediation { return remediationOf(v.Code) }

// ModelFixes reports whether v, if it rejects the plan, is the model's to
// fix: a reprompt code, or a normalise code whose fallback is reprompt (the
// normaliser runs first, so one still present was not fixable by it).
func ModelFixes(v Violation) bool {
	if v.Severity == SeverityWarning {
		return false
	}
	p, ok := RemediationPolicy[v.Code]
	if !ok {
		return true
	}
	return p.Remediation == RemediationReprompt || (p.Remediation == RemediationNormalise && p.Fallback == RemediationReprompt)
}

// NeedsModel reports whether any rejecting violation can only be repaired by
// the model (ModelFixes). Gap codes, and normalise codes that fall back to
// a gap, never cost a re-prompt on their own.
func NeedsModel(vs []Violation) bool {
	for _, v := range Errors(vs) {
		if ModelFixes(v) {
			return true
		}
	}
	return false
}

// Blocks returns the rejecting violations whose remediation is block.
func Blocks(vs []Violation) []Violation {
	var out []Violation
	for _, v := range Errors(vs) {
		if remediationOf(v.Code) == RemediationBlock {
			out = append(out, v)
		}
	}
	return out
}

// Key identifies a violation independently of its wording: the code and
// what it is about (criterion, task, check kind, path, dependency). Two
// rounds' violations are compared by key.
func (v Violation) Key() string {
	return strings.Join([]string{string(v.Code), v.CriterionID, fmt.Sprint(v.TaskIndex), string(v.CheckKind), v.Path, v.Dependency}, "|")
}

// StrictlyFewer reports whether the rejecting violations of next are a
// strict subset of prev's, by key: at least one was fixed and none is new.
// It is the progress test for a further repair round.
func StrictlyFewer(prev, next []Violation) bool {
	before := map[string]bool{}
	for _, v := range Errors(prev) {
		before[v.Key()] = true
	}
	after := map[string]bool{}
	for _, v := range Errors(next) {
		k := v.Key()
		if !before[k] {
			return false
		}
		after[k] = true
	}
	return len(after) < len(before)
}
