package evidence

import (
	"fmt"
	"strings"
	"time"
)

// LegacyCompleteMessage mirrors the implementation.complete payload published
// by cmd/implementation-agent. Field names and JSON tags are copied verbatim
// so existing Kafka messages decode into it unchanged. The first block is the
// original shape; the second block is the additive evidence fields introduced
// by the finalization work (M0-03), all optional on the wire.
type LegacyCompleteMessage struct {
	TaskID       string   `json:"task_id"`
	WorkID       string   `json:"work_id"`
	FilesChanged []string `json:"files_changed"`
	TestResults  string   `json:"test_results"`
	Summary      string   `json:"summary,omitempty"`
	Success      bool     `json:"success"`
	Reason       string   `json:"reason,omitempty"`

	// Additive evidence fields. HeadSHA is the commit the final gate verified;
	// BaseSHA is the integration base; Verified is the producer's own claim
	// that tests ran and passed on HeadSHA. Outcome is derived from the
	// checks below, not from Verified, so a producer cannot assert
	// verification without a matching passed check.
	HeadSHA  string `json:"head_sha,omitempty"`
	BaseSHA  string `json:"base_sha,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Verified bool   `json:"verified"`
}

// legacyCommand labels the synthetic check that stands in for a legacy claim
// the adapter cannot attribute to a real command.
const legacyCommand = "legacy implementation.complete"

// legacyProvenanceReason is the skip_reason on the not_run check every
// adapted record carries. A legacy message's files_changed comes from the
// producer, not from a git diff the consumer can verify, and D1.4 requires
// git-derived changed files for verified; the unrun check caps the outcome
// at partially_verified so ReleaseDecision can never release it.
const legacyProvenanceReason = "legacy message: changed_files provenance not git-verified; evidence produced by a pre-evidence producer"

// FromLegacyCompleteMessage adapts an implementation.complete message into
// schema version 1 so consumers can migrate to the contract before every
// producer does. It is deliberately conservative:
//
//   - test_results is parsed into one check of kind "test":
//     "passed: <cmd>"  -> passed, Command = <cmd>
//     "failed: <cmd>"  -> failed, Command = <cmd>
//     "skipped: <why>" -> skipped, SkipReason = <why>
//     any other text   -> not_run, with the raw text quoted in skip_reason
//     (the pre-finalization producer emitted "executed via tool loop" even
//     when nothing ran, so it is preserved as an unverified claim only).
//     A "passed:" result without a head_sha cannot be tied to a commit and
//     is downgraded to not_run.
//   - success=false additionally records one failed check of kind "other"
//     (the run-level failure: push failure, gate abort, environment error),
//     so outcome is always "failed" for a failed run. The legacy reason is
//     carried into Reason.
//   - Every adapted record additionally carries one not_run check of kind
//     "other" whose skip_reason is legacyProvenanceReason: the legacy
//     payload cannot prove its files_changed came from git (D1.4), so no
//     legacy message can derive "verified" and ReleaseDecision never
//     returns release for one.
//   - Outcome is DeriveOutcome over those checks: success=true with a
//     "passed:" result and a head_sha yields "partially_verified" (the best
//     a legacy message can reach); a skipped or unparseable result yields
//     "unverified"; success=false yields "failed".
//   - head_sha, base_sha, and branch are copied through when present; older
//     messages without them leave the fields empty. repository_id is not on
//     the legacy payload and stays nil.
//   - files_changed is copied into ChangedFiles. Pre-finalization producers
//     populated it from the planner's expected list, not git; the outcome
//     (never better than partially_verified, and never release) is the
//     signal that it is untrusted.
//   - StartedAt and FinishedAt are both set to receivedAt: the legacy
//     payload carries no timing information.
//
// Summary is not carried; callers that need it for display keep the
// original message.
func FromLegacyCompleteMessage(msg LegacyCompleteMessage, receivedAt time.Time) *ImplementationEvidence {
	ev := &ImplementationEvidence{
		SchemaVersion: SchemaVersion,
		WorkID:        msg.WorkID,
		TaskID:        msg.TaskID,
		BaseSHA:       msg.BaseSHA,
		HeadSHA:       msg.HeadSHA,
		Branch:        msg.Branch,
		ChangedFiles:  append([]string{}, msg.FilesChanged...),
		Reason:        msg.Reason,
		StartedAt:     receivedAt,
		FinishedAt:    receivedAt,
		ArtifactRefs:  []string{},
	}

	if check, ok := parseLegacyTestResults(msg.TestResults, msg.HeadSHA); ok {
		ev.Checks = append(ev.Checks, check)
	} else if msg.Success {
		// No parseable result and the producer claims success: record the
		// claim as not_run so it is visible but never counts as passed.
		reason := "legacy message carried no verifiable execution evidence"
		if msg.TestResults != "" {
			reason = fmt.Sprintf("%s; reported test_results: %q", reason, msg.TestResults)
		}
		ev.Checks = append(ev.Checks, CheckResult{
			Kind:       CheckKindTest,
			Command:    legacyCommand,
			Status:     CheckNotRun,
			SkipReason: reason,
		})
	}

	// Provenance cap (D1.4): recorded before any run-level failure so the
	// failed check stays last, and unconditionally so that no combination of
	// legacy fields derives "verified".
	ev.Checks = append(ev.Checks, CheckResult{
		Kind:       CheckKindOther,
		Command:    legacyCommand,
		Status:     CheckNotRun,
		SkipReason: legacyProvenanceReason,
	})

	if !msg.Success {
		ev.Checks = append(ev.Checks, CheckResult{
			Kind:    CheckKindOther,
			Command: legacyCommand,
			Status:  CheckFailed,
		})
	}

	ev.Outcome = DeriveOutcome(ev.Checks)
	return ev
}

// parseLegacyTestResults maps the "<status>: <text>" forms emitted by the
// finalization gate to a CheckResult. ok is false for any other text.
func parseLegacyTestResults(testResults, headSHA string) (CheckResult, bool) {
	prefix, text, found := strings.Cut(testResults, ":")
	if !found {
		return CheckResult{}, false
	}
	text = strings.TrimSpace(text)
	check := CheckResult{Kind: CheckKindTest}
	switch strings.TrimSpace(prefix) {
	case "passed":
		check.Command = text
		check.Status = CheckPassed
		if headSHA == "" {
			check.Status = CheckNotRun
			check.SkipReason = "legacy message reported a passed test run but carried no head_sha to tie it to"
		}
	case "failed":
		check.Command = text
		check.Status = CheckFailed
	case "skipped":
		check.Command = legacyCommand
		check.Status = CheckSkipped
		check.SkipReason = text
		if check.SkipReason == "" {
			check.SkipReason = "legacy message reported skipped without a reason"
		}
	default:
		return CheckResult{}, false
	}
	return check, true
}
