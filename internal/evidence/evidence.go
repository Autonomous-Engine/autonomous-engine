// Package evidence defines the versioned implementation-evidence contract:
// the shared, honest record of what an implementation run actually verified.
//
// It replaces the ad-hoc implementation.complete payload (task_id, work_id,
// files_changed, test_results, success, reason) in which "files changed"
// could mean "files the planner expected" and "test_results" could claim
// execution when tests were skipped. Every producer and consumer of
// implementation evidence is expected to migrate to these types.
//
// Rules:
//   - changed_files MUST come from git (diff against base_sha), never from
//     the planner's expected-file list.
//   - A skipped check is always status "skipped" with a skip_reason. It is
//     never reported as passed or "executed".
//   - outcome MUST equal DeriveOutcome(checks). Validate enforces this so a
//     producer cannot claim "verified" without the checks to back it.
package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the current contract version. Bump it (and add a
// migration path in Validate/Parse) when the wire format changes
// incompatibly.
const SchemaVersion = 1

// CheckKind classifies what a check verified.
type CheckKind string

const (
	CheckKindTest       CheckKind = "test"
	CheckKindDiagnostic CheckKind = "diagnostic"
	CheckKindBuild      CheckKind = "build"
	CheckKindLint       CheckKind = "lint"
	CheckKindOther      CheckKind = "other"
	// CheckKindTypecheck, CheckKindMigration and CheckKindAPICompat are the
	// impact-selected fast checks (M3-04, internal/checks): a type check
	// without a build (tsc --noEmit), a repository's migration test, and a
	// cross-repository contract-compatibility check. Additive within
	// schema version 1.
	CheckKindTypecheck CheckKind = "typecheck"
	CheckKindMigration CheckKind = "migration"
	CheckKindAPICompat CheckKind = "api_compat"
)

// CheckStatus is the observed result of one check.
type CheckStatus string

const (
	// CheckPassed: the command ran to completion and reported success.
	CheckPassed CheckStatus = "passed"
	// CheckFailed: the command ran and reported failure (non-zero exit,
	// timeout, or crash).
	CheckFailed CheckStatus = "failed"
	// CheckSkipped: the check was deliberately not executed. SkipReason is
	// required.
	CheckSkipped CheckStatus = "skipped"
	// CheckNotRun: the check never executed and no deliberate skip decision
	// was recorded (e.g. the run aborted first, or a legacy producer could
	// not say whether it ran).
	CheckNotRun CheckStatus = "not_run"
)

// Outcome summarises the verification level of a whole implementation run.
type Outcome string

const (
	// OutcomeVerified: every recorded check passed.
	OutcomeVerified Outcome = "verified"
	// OutcomePartiallyVerified: some checks passed, the rest were skipped or
	// not run, and none failed.
	OutcomePartiallyVerified Outcome = "partially_verified"
	// OutcomeFailed: at least one check failed.
	OutcomeFailed Outcome = "failed"
	// OutcomeUnverified: no checks were recorded, or none of them ran.
	OutcomeUnverified Outcome = "unverified"
)

// CheckResult records one command that was (or was not) run as part of
// verifying an implementation.
type CheckResult struct {
	Kind    CheckKind `json:"kind"`
	Command string    `json:"command"`
	// ExitCode is nil when the command did not run (skipped / not_run) or
	// when the runner could not observe an exit code.
	ExitCode *int        `json:"exit_code"`
	Status   CheckStatus `json:"status"`
	// SkipReason is mandatory when Status is skipped and optional otherwise
	// (a not_run check may explain why it never started).
	SkipReason string     `json:"skip_reason,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// OutputRef points at the stored raw command output (artifact id, object
	// key, or URL). The output itself is never embedded here.
	OutputRef string `json:"output_ref,omitempty"`
}

// ImplementationEvidence (schema version 1) is the record of a single
// implementation run against a single repository state.
type ImplementationEvidence struct {
	SchemaVersion int    `json:"schema_version"`
	WorkID        string `json:"work_id"`
	TaskID        string `json:"task_id"`
	// RepositoryID is nil when the producer did not resolve a repository row
	// (legacy messages, single-repo products before multi-repo support).
	RepositoryID *string `json:"repository_id"`
	BaseSHA      string  `json:"base_sha"`
	HeadSHA      string  `json:"head_sha"`
	Branch       string  `json:"branch"`
	// ChangedFiles is derived from git (diff base_sha...head_sha), never
	// from the planner's expected list. The one exception is
	// FromLegacyCompleteMessage, which copies the producer's files_changed
	// through without git provenance; such records carry a not_run
	// provenance check, so their outcome is never better than
	// partially_verified and ReleaseDecision never releases them.
	ChangedFiles []string `json:"changed_files"`
	// Checks holds every check that ran or was deliberately not run. Any
	// run-level failure that is not a command (push failure, gate abort,
	// environment error) MUST also be recorded here as a failed CheckResult
	// of kind "other", so that Outcome is "failed". There is no separate
	// run-status enum.
	//
	// Consumers never gate on "Outcome != failed". The release gate is the
	// positive allow-list implemented by ReleaseDecision: only a valid record
	// with Outcome "verified" releases; "partially_verified" advances to
	// mandatory human review only when the product has opted in
	// (ReleasePolicy.PartialVerificationOptIn, products.partial_verification_opt_in)
	// and is blocked otherwise; "failed", "unverified", unknown outcomes,
	// unknown schema versions, and missing evidence all block.
	Checks     []CheckResult `json:"checks"`
	Outcome    Outcome       `json:"outcome"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	// ArtifactRefs lists stored artifacts produced by the run (diffs, logs,
	// reports) by reference.
	ArtifactRefs []string `json:"artifact_refs"`
	// Reason is operator-facing free text explaining a failed or unverified
	// run: the push failure message, the list of files a test run dirtied,
	// "verification unstable", and so on. It explains the outcome; it never
	// replaces the failed check that produces it.
	Reason string `json:"reason,omitempty"`
	// Flags carries risk markers the release gate must honour on top of the
	// outcome. Optional and additive within schema version 1: a record
	// without flags is exactly a pre-M3-03 record. Every flag must be one
	// this package knows (Validate rejects unknown flags, so a consumer that
	// cannot honour a newer flag fails closed instead of ignoring it).
	// Defined flags: FlagSecurityReviewRequired, FlagUngrounded,
	// FlagZoneReviewRequired.
	Flags []string `json:"flags,omitempty"`
	// Zones are the codex zones (migration 063) the changed files fall in,
	// whatever their policy, each with its policy and matching files. The
	// PR body lists them. Optional and additive within schema version 1.
	Zones []ZoneTouch `json:"zones,omitempty"`
}

// ZoneTouch is one codex zone a run's changed files fall in.
type ZoneTouch struct {
	Name string `json:"name"`
	// Policy is the zone's policy: autonomous, human_review or escalate.
	Policy string   `json:"policy"`
	Files  []string `json:"files,omitempty"`
}

// Zone policies as recorded on ZoneTouch (internal/zones.Policy values).
const (
	ZonePolicyAutonomous  = "autonomous"
	ZonePolicyHumanReview = "human_review"
	ZonePolicyEscalate    = "escalate"
)

// FlagZoneReviewRequired marks evidence whose changed files fall in a
// codex zone with policy human_review (migration 063); Zones names them.
// The release gate treats it exactly like FlagSecurityReviewRequired:
// otherwise-releasable (verified) evidence goes to human review, whatever
// HUMAN_REVIEW_REQUIRED says.
const FlagZoneReviewRequired = "zone_review_required"

// ZoneReviewNames are the names of the human_review zones the record
// touched, in order.
func (e *ImplementationEvidence) ZoneReviewNames() []string {
	if e == nil {
		return nil
	}
	var out []string
	for _, z := range e.Zones {
		if z.Policy == ZonePolicyHumanReview {
			out = append(out, z.Name)
		}
	}
	return out
}

// FlagSecurityReviewRequired marks evidence whose changed files match the
// repository's security_review_paths (those changes always need human security review). The release
// gate routes otherwise-releasable (verified) evidence carrying it to human
// review instead of release.
const FlagSecurityReviewRequired = "security_review_required"

// FlagUngrounded marks evidence of a run that worked without the codex
// (codex coherence Part 3): its research or implementation run recorded
// more empty codex lookups than CODEX_GAP_THRESHOLD (status-orchestrator's
// codex.gap consumer, internal/codexgap). The verification stage adds it
// before judging the head. Like missing evidence it never releases or
// approves automatically: verified evidence carrying it goes to human
// review, and the verification stage turns an approving verdict into an
// escalation.
const FlagUngrounded = "ungrounded"

// HasFlag reports whether the record carries flag.
func (e *ImplementationEvidence) HasFlag(flag string) bool {
	if e == nil {
		return false
	}
	for _, f := range e.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

// ErrUnknownSchemaVersion is returned (wrapped) by Validate and Parse when the
// schema_version is not one this package understands.
var ErrUnknownSchemaVersion = errors.New("unknown evidence schema version")

// DeriveOutcome computes the Outcome implied by a set of checks:
//   - any failed check                       -> failed
//   - no checks, or none passed              -> unverified
//   - every check passed                     -> verified
//   - passed mixed with skipped/not_run      -> partially_verified
//
// A check with an unrecognised status is treated as not having passed, so it
// can never lift the outcome; Validate rejects it separately.
func DeriveOutcome(checks []CheckResult) Outcome {
	passed, failed := 0, 0
	for _, c := range checks {
		switch c.Status {
		case CheckPassed:
			passed++
		case CheckFailed:
			failed++
		}
	}
	switch {
	case failed > 0:
		return OutcomeFailed
	case passed == 0:
		return OutcomeUnverified
	case passed == len(checks):
		return OutcomeVerified
	default:
		return OutcomePartiallyVerified
	}
}

// Validate reports the first contract violation found, or nil. Beyond the
// enum and consistency rules, a verified or partially_verified outcome must
// name the head_sha it was verified against.
func (e *ImplementationEvidence) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: %d (supported: %d)", ErrUnknownSchemaVersion, e.SchemaVersion, SchemaVersion)
	}
	if !validOutcome(e.Outcome) {
		return fmt.Errorf("unknown outcome %q", e.Outcome)
	}
	for i, c := range e.Checks {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("checks[%d]: %w", i, err)
		}
	}
	if derived := DeriveOutcome(e.Checks); e.Outcome != derived {
		return fmt.Errorf("outcome %q is inconsistent with checks (derived %q)", e.Outcome, derived)
	}
	for i, f := range e.Flags {
		if !validFlag(f) {
			return fmt.Errorf("flags[%d]: unknown flag %q", i, f)
		}
	}
	for i, z := range e.Zones {
		if strings.TrimSpace(z.Name) == "" {
			return fmt.Errorf("zones[%d]: empty name", i)
		}
		switch z.Policy {
		case ZonePolicyAutonomous, ZonePolicyHumanReview, ZonePolicyEscalate:
		default:
			return fmt.Errorf("zones[%d]: unknown policy %q", i, z.Policy)
		}
	}
	if e.HasFlag(FlagZoneReviewRequired) && len(e.ZoneReviewNames()) == 0 {
		return fmt.Errorf("flag %s without a human_review zone in zones", FlagZoneReviewRequired)
	}
	// A passed check is only meaningful against a specific commit.
	if (e.Outcome == OutcomeVerified || e.Outcome == OutcomePartiallyVerified) && e.HeadSHA == "" {
		return fmt.Errorf("outcome %q requires a non-empty head_sha", e.Outcome)
	}
	return nil
}

// Validate reports the first contract violation in a single check, or nil.
func (c *CheckResult) Validate() error {
	if !validCheckKind(c.Kind) {
		return fmt.Errorf("unknown check kind %q", c.Kind)
	}
	if !validCheckStatus(c.Status) {
		return fmt.Errorf("unknown check status %q", c.Status)
	}
	if c.Status == CheckSkipped && c.SkipReason == "" {
		return errors.New("skipped check must carry a skip_reason")
	}
	return nil
}

// Parse decodes JSON into ImplementationEvidence and validates it. It is the
// entry point consumers should use for evidence read off the wire or out of
// the database.
func Parse(data []byte) (*ImplementationEvidence, error) {
	var ev ImplementationEvidence
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("decode evidence: %w", err)
	}
	if err := ev.Validate(); err != nil {
		return nil, err
	}
	return &ev, nil
}

func validOutcome(o Outcome) bool {
	switch o {
	case OutcomeVerified, OutcomePartiallyVerified, OutcomeFailed, OutcomeUnverified:
		return true
	}
	return false
}

func validCheckKind(k CheckKind) bool {
	switch k {
	case CheckKindTest, CheckKindDiagnostic, CheckKindBuild, CheckKindLint, CheckKindOther,
		CheckKindTypecheck, CheckKindMigration, CheckKindAPICompat:
		return true
	}
	return false
}

func validFlag(f string) bool {
	return f == FlagSecurityReviewRequired || f == FlagUngrounded || f == FlagZoneReviewRequired
}

// WithFlag returns the evidence record raw (still on the wire) with flag
// added to its flags, every other field kept as it is. A record that
// already carries the flag is returned unchanged. raw must be a JSON object.
func WithFlag(raw []byte, flag string) ([]byte, error) {
	if !validFlag(flag) {
		return nil, fmt.Errorf("unknown flag %q", flag)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode evidence: %w", err)
	}
	if fields == nil {
		return nil, errors.New("no evidence record")
	}
	var flags []string
	if f, ok := fields["flags"]; ok {
		if err := json.Unmarshal(f, &flags); err != nil {
			return nil, fmt.Errorf("decode evidence flags: %w", err)
		}
	}
	for _, f := range flags {
		if f == flag {
			return raw, nil
		}
	}
	encoded, err := json.Marshal(append(flags, flag))
	if err != nil {
		return nil, err
	}
	fields["flags"] = encoded
	return json.Marshal(fields)
}

func validCheckStatus(s CheckStatus) bool {
	switch s {
	case CheckPassed, CheckFailed, CheckSkipped, CheckNotRun:
		return true
	}
	return false
}
