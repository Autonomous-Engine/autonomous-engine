package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ReleaseAction is what a consumer must do with a work item after looking at
// its implementation evidence. The wire values are fixed for the M0-07 batch.
type ReleaseAction string

const (
	// ActionRelease: the automated verification gate passed. Only a valid
	// record whose outcome is verified ever produces this (D1.2).
	ActionRelease ReleaseAction = "release"
	// ActionHumanReview: the work may advance, but only to mandatory human
	// review. Produced solely for partially_verified evidence on a product
	// that has explicitly opted in (D2, D3).
	ActionHumanReview ReleaseAction = "human_review"
	// ActionBlock: hold the work back. Every other case, including nil,
	// malformed, unknown-version, and unrecognised-outcome evidence, fails
	// closed here (D1.3).
	ActionBlock ReleaseAction = "block"
)

// ReleasePolicy carries the per-product policy inputs to ReleaseDecision.
type ReleasePolicy struct {
	// PartialVerificationOptIn mirrors products.partial_verification_opt_in
	// (migration 045). When true, a partially_verified run may advance to
	// human review instead of being blocked. It never lifts a run to release.
	PartialVerificationOptIn bool
}

// Decision is the result of applying ReleasePolicy to evidence. Reason is
// always non-empty and written for an operator: it names the outcome and,
// for a block, says why.
type Decision struct {
	Action ReleaseAction
	Reason string
}

// ReleaseDecision is the single release gate every consumer of implementation
// evidence must call. It is a pure function of the record and the policy:
//
//	nil evidence                                  -> block
//	Validate() error (unknown schema version,
//	  unknown outcome, inconsistent checks, ...)   -> block
//	verified, flag security_review_required       -> human_review (D3)
//	verified, flag zone_review_required           -> human_review (zones)
//	verified, flag ungrounded                     -> human_review (codex Part 3)
//	verified                                      -> release
//	partially_verified, policy opted in           -> human_review
//	partially_verified, policy not opted in       -> block
//	failed, unverified, anything else             -> block
//
// The allow-list is positive: a new or unrecognised outcome can never release
// without this function being changed.
func ReleaseDecision(ev *ImplementationEvidence, policy ReleasePolicy) Decision {
	if ev == nil {
		return block("no evidence attached")
	}
	if err := ev.Validate(); err != nil {
		if errors.Is(err, ErrUnknownSchemaVersion) {
			return block(fmt.Sprintf("unknown evidence schema version %d (supported: %d)", ev.SchemaVersion, SchemaVersion))
		}
		return block("invalid evidence: " + err.Error())
	}
	switch ev.Outcome {
	case OutcomeVerified:
		checks := fmt.Sprintf("all %d recorded checks passed", len(ev.Checks))
		if len(ev.Checks) == 1 {
			checks = "the 1 recorded check passed"
		}
		security, zone := ev.HasFlag(FlagSecurityReviewRequired), ev.HasFlag(FlagZoneReviewRequired)
		switch {
		case security && zone:
			return Decision{
				Action: ActionHumanReview,
				Reason: fmt.Sprintf("outcome verified (%s against head %s) but the change touches security-review paths and %s: mandatory human review, not automatic release", checks, ev.HeadSHA, zoneList(ev)) + ungroundedSummary(ev),
			}
		case security:
			// D3: a security-sensitive change needs a human even when every
			// check passed. It advances, but only to human review.
			return Decision{
				Action: ActionHumanReview,
				Reason: fmt.Sprintf("outcome verified (%s against head %s) but the change touches security-review paths: mandatory security review, not automatic release", checks, ev.HeadSHA) + ungroundedSummary(ev),
			}
		case zone:
			// Codex zones (migration 063): a human_review zone needs a
			// human approving the PR, exactly like the security flag.
			return Decision{
				Action: ActionHumanReview,
				Reason: fmt.Sprintf("outcome verified (%s against head %s) but the change touches %s: mandatory human review, not automatic release", checks, ev.HeadSHA, zoneList(ev)) + ungroundedSummary(ev),
			}
		}
		if ev.HasFlag(FlagUngrounded) {
			// Codex coherence Part 3: a run that worked without the codex
			// is treated like missing grounding evidence — it advances
			// only to human review.
			return Decision{
				Action: ActionHumanReview,
				Reason: fmt.Sprintf("outcome verified (%s against head %s) but the run was ungrounded (too many empty codex lookups): mandatory human review, not automatic release", checks, ev.HeadSHA),
			}
		}
		return Decision{
			Action: ActionRelease,
			Reason: fmt.Sprintf("outcome verified: %s against head %s", checks, ev.HeadSHA),
		}
	case OutcomePartiallyVerified:
		if policy.PartialVerificationOptIn {
			return Decision{
				Action: ActionHumanReview,
				Reason: "outcome partially_verified and product has opted in: mandatory human review, not automatic release" + skippedSummary(ev) + securitySummary(ev) + ungroundedSummary(ev),
			}
		}
		return block("outcome partially_verified and product has not opted in" + skippedSummary(ev) + securitySummary(ev) + ungroundedSummary(ev))
	case OutcomeFailed:
		return block("outcome failed" + detail(ev.Reason))
	case OutcomeUnverified:
		return block("outcome unverified: no recorded check passed" + detail(ev.Reason))
	default:
		// Validate rejects unknown outcomes, so this is unreachable today;
		// it stays so that a future outcome added to validOutcome without a
		// gate decision still fails closed.
		return block(fmt.Sprintf("outcome %q has no release rule", ev.Outcome))
	}
}

// ReleaseDecisionFromJSON applies ReleaseDecision to evidence still on the
// wire or in the database. Empty or undecodable payloads block.
//
// It judges the record on its own: nothing checks that the record belongs
// to the caller's work item. A consumer of a message that names a work_id
// and task_id (implementation.complete and anything derived from it) must
// use ReleaseDecisionForMessage instead, so a producer bug or a replayed
// message cannot release one subtask on another subtask's evidence. This
// function is for callers with no message context: displaying a stored
// record, tooling, tests.
func ReleaseDecisionFromJSON(data []byte, policy ReleasePolicy) Decision {
	ev, d, ok := decodeForDecision(data)
	if !ok {
		return d
	}
	return ReleaseDecision(ev, policy)
}

// MessageIdentity is what a consumer knows about the message it is judging
// evidence for: the message's own work_id and task_id, and the subtask's
// repository when the consumer has resolved it. RepositoryID nil means
// "unknown here" and skips the repository check; it never means "must be
// unset on the record".
type MessageIdentity struct {
	WorkID       string
	TaskID       string
	RepositoryID *string
}

// ErrIdentityMismatch is returned (wrapped) by CheckIdentity when a record
// does not belong to the message it arrived on.
var ErrIdentityMismatch = errors.New("evidence does not belong to this message")

// CheckIdentity reports whether ev is the record for expect. work_id and
// task_id must be equal (exact string equality after trimming surrounding
// whitespace; no case folding, no UUID normalisation). repository_id is
// compared the same way, but only when both sides carry one. A nil record
// is a mismatch. The error names the field and both values so the operator
// can see which message and which record were confused.
func CheckIdentity(ev *ImplementationEvidence, expect MessageIdentity) error {
	if ev == nil {
		return fmt.Errorf("%w: no record", ErrIdentityMismatch)
	}
	if got, want := strings.TrimSpace(ev.WorkID), strings.TrimSpace(expect.WorkID); got != want {
		return fmt.Errorf("%w: evidence work_id %q != message work_id %q", ErrIdentityMismatch, got, want)
	}
	if got, want := strings.TrimSpace(ev.TaskID), strings.TrimSpace(expect.TaskID); got != want {
		return fmt.Errorf("%w: evidence task_id %q != message task_id %q", ErrIdentityMismatch, got, want)
	}
	if ev.RepositoryID != nil && expect.RepositoryID != nil {
		if got, want := strings.TrimSpace(*ev.RepositoryID), strings.TrimSpace(*expect.RepositoryID); got != want {
			return fmt.Errorf("%w: evidence repository_id %q != subtask repository_id %q", ErrIdentityMismatch, got, want)
		}
	}
	return nil
}

// ReleaseDecisionForMessage is the release gate for evidence that arrived
// on, or is being applied to, a message with a known identity. It decodes
// exactly as ReleaseDecisionFromJSON does (empty, null, {}, malformed and
// unknown-version payloads block with the same reasons), then rejects a
// record whose work_id or task_id differ from the message's, or whose
// repository_id differs from the subtask's when both are known
// (CheckIdentity), before judging its content with ReleaseDecision. A
// record that does not belong to the message therefore never yields
// release or human_review, whatever its outcome (D1.3: anything that cannot
// be trusted fails closed). For a matching identity the result is exactly
// ReleaseDecisionFromJSON's.
//
// The identity check runs before the content is judged so that the reason
// for a foreign record names the real cause rather than, say, its schema
// version.
func ReleaseDecisionForMessage(data []byte, policy ReleasePolicy, expect MessageIdentity) Decision {
	ev, d, ok := decodeForDecision(data)
	if !ok {
		return d
	}
	if err := CheckIdentity(ev, expect); err != nil {
		return block(err.Error())
	}
	return ReleaseDecision(ev, policy)
}

// decodeForDecision is the shared decode step of ReleaseDecisionFromJSON and
// ReleaseDecisionForMessage. ok is false when data cannot be judged as a
// record at all, in which case d is the block to return. It decodes only;
// ReleaseDecision runs Validate so every entry point gives the same reason
// for the same record.
func decodeForDecision(data []byte) (ev *ImplementationEvidence, d Decision, ok bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, block("no evidence attached"), false
	}
	// A JSON null or an empty object is "nothing attached", not a record
	// with a bad version; probe the shape first so the reason says so.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, block("invalid evidence: decode evidence: " + err.Error()), false
	}
	if len(probe) == 0 {
		return nil, block("no evidence attached"), false
	}
	var decoded ImplementationEvidence
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, block("invalid evidence: decode evidence: " + err.Error()), false
	}
	return &decoded, Decision{}, true
}

func block(reason string) Decision {
	return Decision{Action: ActionBlock, Reason: reason}
}

func detail(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// securitySummary notes the D3 security-review flag on a decision reason.
func securitySummary(ev *ImplementationEvidence) string {
	s := ""
	if ev.HasFlag(FlagSecurityReviewRequired) {
		s = "; security review required (change touches security-review paths)"
	}
	if ev.HasFlag(FlagZoneReviewRequired) {
		s += "; human review required (change touches " + zoneList(ev) + ")"
	}
	return s
}

// zoneList names the human_review zones of a zone-flagged record:
// "human_review zone a" or "human_review zones a, b".
func zoneList(ev *ImplementationEvidence) string {
	names := ev.ZoneReviewNames()
	if len(names) == 1 {
		return "human_review zone " + names[0]
	}
	return "human_review zones " + strings.Join(names, ", ")
}

// ungroundedSummary notes the codex coherence Part 3 ungrounded flag on a
// decision reason.
func ungroundedSummary(ev *ImplementationEvidence) string {
	if ev.HasFlag(FlagUngrounded) {
		return "; ungrounded run (too many empty codex lookups)"
	}
	return ""
}

// skippedSummary lists the checks that did not run so the operator reviewing
// a partially_verified item sees what was not verified (D2 requires every
// skipped check and its reason to be named).
func skippedSummary(ev *ImplementationEvidence) string {
	var s string
	for _, c := range ev.Checks {
		if c.Status == CheckPassed {
			continue
		}
		if s == "" {
			s = "; not verified:"
		} else {
			s += ","
		}
		s += " " + string(c.Kind) + " " + string(c.Status)
		if c.SkipReason != "" {
			s += " (" + c.SkipReason + ")"
		}
	}
	return s
}
