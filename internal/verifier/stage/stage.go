// Package stage is the independent verifier's stage (R14): the scratch
// repository at the head under verification, the verifier run against it,
// the verdict artifact, the verification.complete message and the
// verification_results record. It is shared by the verification stage
// (cmd/review-agent, consuming implementation.complete, before any PR
// exists) and by review-agent's pr.ready path, which reuses the stored
// verdict for the PR's head and only falls back to running the verifier
// itself when none exists.
//
// Pipeline (R14): implementation.complete → verification stage →
// verification.complete{decision, verdict ref} → pr-coordinator (a PR or a
// pending change only on approve / approve_pending_second_pass) and
// status-orchestrator (the parent leaves `verification` for review,
// human_review, implementation or blocked) → pr.ready → review-agent (LLM
// second pass, reusing the verdict) → QA.
//
// ("Verification before the PR (R14)").
package stage

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/verifier"
)

// Decision is the verification stage's decision for one subtask's head, the
// "decision" of verification.complete and of verification_results.
type Decision string

const (
	// DecisionApprove: every criterion met, no blocking defect, and no
	// second-pass review will run (REVIEW_LLM_SECOND_PASS=false). The PR
	// opens as usual.
	DecisionApprove Decision = "approve"
	// DecisionApprovePendingSecondPass: the verifier approved and the LLM
	// review on pr.ready decides as the second pass. The PR opens, its title
	// labelled with SecondPassTitlePrefix.
	DecisionApprovePendingSecondPass Decision = "approve_pending_second_pass"
	// DecisionRequestChanges: a criterion is not met or a critical/major
	// defect was found. No PR; the parent goes back to implementation with
	// the defects as feedback.
	DecisionRequestChanges Decision = "request_changes"
	// DecisionEscalate: the verifier could not verify (a not_verifiable
	// criterion, an unparseable verdict, a failed tool loop). No PR; the
	// parent goes to human_review with the verdict as the reason.
	DecisionEscalate Decision = "escalate"
	// DecisionNotRun: the subtask was not verified because its
	// implementation is not releasable (failed, blocked, partially verified
	// without the product's opt-in, an unknown evidence schema) or because
	// the message carries no evidence (Legacy, EVIDENCE_COMPAT_WINDOW).
	// pr-coordinator applies its own evidence decision: a non-releasable
	// subtask gets no PR either way; only a legacy-window message without
	// evidence keeps its pre-evidence path (verified on pr.ready).
	DecisionNotRun Decision = "not_run"
	// DecisionError: the stage could not set up the verification (no clone,
	// the head is not in the clone, the scratch repository or the diff
	// failed, no verifier model). The parent is blocked with a prompt in the
	// same transaction; no PR.
	DecisionError Decision = "error"
)

// Valid reports whether d is a known decision.
func (d Decision) Valid() bool {
	switch d {
	case DecisionApprove, DecisionApprovePendingSecondPass, DecisionRequestChanges, DecisionEscalate, DecisionNotRun, DecisionError:
		return true
	}
	return false
}

// Approves reports whether d lets a PR (or a pending change) be created:
// approve or approve_pending_second_pass. D1: nothing is released without a
// verified outcome AND one of these.
func (d Decision) Approves() bool {
	return d == DecisionApprove || d == DecisionApprovePendingSecondPass
}

// FromVerdict reports whether d comes from a verifier verdict (and so has a
// verdict artifact).
func (d Decision) FromVerdict() bool {
	switch d {
	case DecisionApprove, DecisionApprovePendingSecondPass, DecisionRequestChanges, DecisionEscalate:
		return true
	}
	return false
}

// SecondPassTitlePrefix labels the PR of a subtask the verifier approved
// pending the second-pass review, like the "[PARTIALLY VERIFIED — human
// review required] " convention. Keep the words stable: reviewers and the
// e2e fixture key off them.
const SecondPassTitlePrefix = "[VERIFIER APPROVED — second-pass review pending] "

// SecondPassEnv names the switch for the LLM review after a verifier
// approval. Default on; "false", "0", "no" or "off" disable it.
const SecondPassEnv = "REVIEW_LLM_SECOND_PASS"

// SecondPassEnabled reads SecondPassEnv.
func SecondPassEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SecondPassEnv))) {
	case "false", "0", "no", "off":
		return false
	}
	return true
}

// DecisionFor maps the verifier gate onto the stage's decision. An approval
// is approve_pending_second_pass when the second pass runs: when it is
// enabled, or when the gate requires it (an item with no acceptance
// criteria).
func DecisionFor(g verifier.Decision, secondPass bool) Decision {
	switch g.Action {
	case verifier.ActionApprove:
		if secondPass || g.SecondPassRequired {
			return DecisionApprovePendingSecondPass
		}
		return DecisionApprove
	case verifier.ActionRequestChanges:
		return DecisionRequestChanges
	}
	return DecisionEscalate
}

// CriterionSummary is one criterion's result as verification.complete and
// the PR description carry it.
type CriterionSummary struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// maxNote caps a criterion note carried on the message.
const maxNote = 300

// Summaries are the per-criterion results of v, notes on one line and
// capped.
func Summaries(v verifier.Verdict) []CriterionSummary {
	out := make([]CriterionSummary, 0, len(v.PerCriterion))
	for _, r := range v.PerCriterion {
		out = append(out, CriterionSummary{ID: r.ID, Status: string(r.Status), Note: OneLine(r.Note, maxNote)})
	}
	return out
}

// OneLine collapses whitespace and cuts s to n runes ("…" appended).
func OneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if rs := []rune(s); n > 0 && len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}

// Message is the verification.complete payload. Implementation is the
// implementation.complete payload the decision is about, verbatim, so
// pr-coordinator keeps judging the evidence exactly as before and only adds
// the verification decision on top.
type Message struct {
	WorkID       string `json:"work_id"`
	TaskID       string `json:"task_id"`
	RepositoryID string `json:"repository_id,omitempty"`
	// HeadSHA is the verified head the decision is about (the evidence's,
	// never the message's own head_sha); empty for not_run on a message
	// without evidence.
	HeadSHA  string   `json:"head_sha,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	Decision Decision `json:"decision"`
	// Reason is one operator-readable line: the gate's reason, why the
	// verification did not run, or what failed.
	Reason string `json:"reason"`
	// Feedback is the structured defect feedback (request_changes) or the
	// rendered verdict (other verdicts).
	Feedback string `json:"feedback,omitempty"`
	// Legacy: not_run because the message carries no evidence.
	Legacy            bool                  `json:"legacy,omitempty"`
	ResultID          string                `json:"result_id"`
	VerdictArtifactID string                `json:"verdict_artifact_id,omitempty"`
	Criteria          []CriterionSummary    `json:"criteria,omitempty"`
	Gaps              []string              `json:"gaps,omitempty"`
	Defects           verifier.DefectCounts `json:"defects"`
	VerifierVersion   string                `json:"verifier_version,omitempty"`
	// Reused: the verdict is an earlier one for the same head (no new
	// verifier run).
	Reused         bool            `json:"reused,omitempty"`
	Implementation json.RawMessage `json:"implementation,omitempty"`
	Timestamp      string          `json:"timestamp"`
}
