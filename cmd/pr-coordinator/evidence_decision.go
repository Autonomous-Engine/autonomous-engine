package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

// The compatibility window for implementation.complete messages without
// evidence (EVIDENCE_COMPAT_WINDOW) is evidence.CompatWindow, read through
// evidence.CompatWindowFromEnv so every consumer of the topic applies the
// same parsing and the same strict fallback for unrecognised values. The
// product policy comes from products.LoadReleasePolicyForItem and the
// legacy-fallback counter is metrics.EvidenceCompatLegacyFallback, shared
// for the same reason.

// Outcome labels shown per subtask in the PR body. They are operator-facing
// text, not wire values; the wire value is subtaskOutcome.Action.
const (
	outcomeLabelVerified          = "verified"
	outcomeLabelPartiallyVerified = "partially verified"
	outcomeLabelFailed            = "failed"
	// outcomeLabelBlocked covers every non-release, non-failed block:
	// partial without opt-in, unknown schema, malformed or missing evidence
	// in strict mode, contradictory success flag.
	outcomeLabelBlocked = "blocked"
	// outcomeLabelLegacy is a pre-evidence producer's success in the legacy
	// window. It is deliberately not "verified": nothing was git-verified.
	outcomeLabelLegacy = "completed (legacy message, no evidence)"
	// outcomeLabelSecurityReview is verified evidence carrying the
	// security_review_required flag (M3-03, D3).
	outcomeLabelSecurityReview = "security review required"
	// outcomeLabelZoneReview is verified evidence carrying the
	// zone_review_required flag (codex zones, migration 063).
	outcomeLabelZoneReview = "zone review required"
)

// partialVerificationTitlePrefix and partialVerificationBodyMarker are the
// D2/D3 markers on a PR opened from partially_verified evidence. The dashboard
// and reviewers key off the words; keep them stable.
const (
	partialVerificationTitlePrefix = "[PARTIALLY VERIFIED — human review required] "
	partialVerificationBodyMarker  = "PARTIALLY VERIFIED — human review required"
	// securityReviewTitlePrefix / securityReviewBodyMarker mark a PR whose
	// subtasks are verified but touch security-review paths (M3-03, D3).
	// Partially verified wording is kept for actual partial outcomes only.
	securityReviewTitlePrefix = "[SECURITY REVIEW — human review required] "
	securityReviewBodyMarker  = "SECURITY REVIEW — human review required"
	// zoneReviewTitlePrefix / zoneReviewBodyMarker mark a PR whose subtasks
	// are verified but touch a human_review codex zone (migration 063).
	zoneReviewTitlePrefix = "[ZONE REVIEW — human review required] "
	zoneReviewBodyMarker  = "ZONE REVIEW — human review required"
)

// maxDiffInBody caps the raw diff embedded in a PR body.
const maxDiffInBody = 5000

// subtaskOutcome is the per-subtask PR decision: what the evidence said,
// what the coordinator will do about it, and the git-derived facts the PR
// must carry (D1.4/D1.5). It is produced by pure functions so the rules can
// be table-tested without Kafka, GitHub or a database.
type subtaskOutcome struct {
	TaskID  string
	Summary string

	// Action is the evidence.ReleaseDecision outcome (or the legacy-window
	// equivalent): release → ordinary PR; human_review → PR marked partially
	// verified; block → no PR.
	Action evidence.ReleaseAction
	// Reason is operator-readable and always non-empty.
	Reason string
	// Label is the outcome word shown in the PR body task list.
	Label string

	// HeadSHA is the exact verified head SHA from the evidence (empty for a
	// legacy message, which never verified one).
	HeadSHA string
	// ChangedFiles is git-derived from the evidence when a record is
	// present; for a legacy message it is the planner-sourced list, which
	// the PR body flags as not git-verified. It is never a merge of both.
	ChangedFiles []string
	// Legacy is true when the decision came from the pre-evidence success
	// boolean under the legacy compatibility window.
	Legacy bool
	// SecurityReview is true for a human_review outcome whose evidence is
	// verified but flagged security_review_required (M3-03, D3);
	// SecurityFiles names the changed files that matched.
	SecurityReview bool
	SecurityFiles  string
	// ZoneReview is true for a human_review outcome whose evidence is
	// verified but flagged zone_review_required (migration 063);
	// ZoneReviewNames are its human_review zones.
	ZoneReview      bool
	ZoneReviewNames []string
	// Zones are every codex zone the subtask's changed files fall in, from
	// the evidence, whatever their policy; the PR body lists them.
	Zones []evidence.ZoneTouch

	// Verification is the verification stage's decision for the subtask
	// (R14, applyVerification); nil before it is applied.
	Verification *verification
	// SecondPassPending: the verifier approved pending the second-pass
	// review; the PR title carries stage.SecondPassTitlePrefix.
	SecondPassPending bool
	// EvidenceBlocked: the evidence decision itself blocked the subtask
	// (failed, not releasable), before any verification applied. Such a
	// subtask is not part of the release (all-or-nothing ignores it, as the
	// PR loop always has).
	EvidenceBlocked bool
}

// hasEvidence reports whether the message carries an evidence value at all.
// An absent field and an explicit JSON null are both "no evidence" and fall
// under the compatibility window; anything else (including {} or malformed
// JSON) is present and is judged by evidence.ReleaseDecisionForMessage.
func (p implementationCompletePayload) hasEvidence() bool {
	raw := bytes.TrimSpace(p.Evidence)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// decideSubtaskPR is the pure per-subtask decision for an
// implementation.complete message:
//
//	release      — evidence.ReleaseDecision returned release (verified), or
//	               a legacy message without evidence reported success in
//	               the legacy window. An ordinary PR is opened.
//	human_review — ReleaseDecision returned human_review (partially_verified,
//	               product opted in). A PR is opened with the partial
//	               verification marker in title and body; it can only be
//	               merged after human review (D2, D3).
//	block        — everything else, including no evidence in strict mode,
//	               unknown schema versions in either mode, a failed legacy
//	               message in legacy mode, and a record whose work_id or
//	               task_id is not the message's (identity mismatch). No PR.
//
// Files and head SHA are taken from the evidence whenever a decodable record
// that belongs to the message is present — even for a block, so the log can
// name the SHA — and from the legacy fields only when there is no evidence
// at all. A record for another subtask contributes nothing: its SHA and
// files are never carried. policy comes from
// products.LoadReleasePolicyForItem; its zero value (unknown product or
// lookup failure) is "not opted in", so partially_verified blocks.
//
// The identity checked here is the message's own work_id and task_id;
// decideSubtaskPRFor additionally checks the subtask's repository, which
// the message path and the stored-evidence fallback both obtain from the
// work item row via loadPolicyForTask.
func decideSubtaskPR(payload implementationCompletePayload, policy evidence.ReleasePolicy, window evidence.CompatWindow) subtaskOutcome {
	return decideSubtaskPRFor(payload, policy, window, nil)
}

// decideSubtaskPRFor is decideSubtaskPR with the subtask's repository, when
// the caller has resolved it, added to the identity the record must match.
// nil skips the repository check.
func decideSubtaskPRFor(payload implementationCompletePayload, policy evidence.ReleasePolicy, window evidence.CompatWindow, repositoryID *string) subtaskOutcome {
	out := subtaskOutcome{TaskID: payload.TaskID, Summary: payload.Summary}

	if !payload.hasEvidence() {
		if window == evidence.CompatWindowStrict {
			out.Action = evidence.ActionBlock
			out.Label = outcomeLabelBlocked
			out.Reason = "no evidence attached to implementation.complete (" + evidence.CompatWindowEnv + "=strict)"
			if payload.Reason != "" {
				out.Reason += "; implementation reported: " + payload.Reason
			}
			return out
		}
		// Legacy window: the pre-evidence contract. The planner-sourced
		// file list is all there is; the body says so.
		out.Legacy = true
		out.ChangedFiles = append([]string{}, payload.FilesChanged...)
		if payload.Success {
			out.Action = evidence.ActionRelease
			out.Label = outcomeLabelLegacy
			out.Reason = "implementation complete (legacy message without evidence)"
			return out
		}
		out.Action = evidence.ActionBlock
		out.Label = outcomeLabelFailed
		out.Reason = "implementation failed: " + payload.Reason
		return out
	}

	// The record must be this message's: a verified record for another
	// subtask must never open this subtask's PR (D1.3).
	expect := evidence.MessageIdentity{WorkID: payload.WorkID, TaskID: payload.TaskID, RepositoryID: repositoryID}
	d := evidence.ReleaseDecisionForMessage(payload.Evidence, policy, expect)

	// Carry the git-derived facts whenever the record decodes and belongs
	// to this message, regardless of the decision, so a block is logged
	// with its SHA and a PR never falls back to the planner's guess. A
	// foreign record's facts are not this subtask's and are dropped.
	var ev evidence.ImplementationEvidence
	decoded := json.Unmarshal(payload.Evidence, &ev) == nil && evidence.CheckIdentity(&ev, expect) == nil
	if decoded {
		out.HeadSHA = ev.HeadSHA
		out.ChangedFiles = append([]string{}, ev.ChangedFiles...)
		out.Zones = append([]evidence.ZoneTouch{}, ev.Zones...)
	}

	switch d.Action {
	case evidence.ActionRelease:
		if !payload.Success {
			// A producer that reports success=false alongside verified
			// evidence is contradicting itself; fail closed.
			out.Action = evidence.ActionBlock
			out.Label = outcomeLabelBlocked
			out.Reason = "implementation blocked: evidence is verified but the message reports success=false" + reasonDetail(payload.Reason)
			return out
		}
		out.Action = evidence.ActionRelease
		out.Label = outcomeLabelVerified
		out.Reason = "implementation complete: " + d.Reason
	case evidence.ActionHumanReview:
		out.Action = evidence.ActionHumanReview
		out.Label = outcomeLabelPartiallyVerified
		out.Reason = d.Reason
		if decoded && ev.HasFlag(evidence.FlagZoneReviewRequired) {
			// The zone names travel with every human-review outcome that
			// carries the flag; ZoneReview (the zone is the only reason)
			// only for verified evidence. A partially verified run in a
			// human_review zone is labelled with both reasons.
			out.ZoneReviewNames = ev.ZoneReviewNames()
			if ev.Outcome == evidence.OutcomeVerified {
				out.Label = outcomeLabelZoneReview
				out.ZoneReview = true
			} else {
				out.Label = outcomeLabelPartiallyVerified + "; " + outcomeLabelZoneReview
			}
		}
		if decoded && ev.Outcome == evidence.OutcomeVerified && ev.HasFlag(evidence.FlagSecurityReviewRequired) {
			out.Label = outcomeLabelSecurityReview
			out.SecurityReview = true
			out.SecurityFiles = securityFilesFrom(ev.Reason)
		}
	default:
		out.Action = evidence.ActionBlock
		out.Label = outcomeLabelBlocked
		if decoded && ev.Outcome == evidence.OutcomeFailed {
			out.Label = outcomeLabelFailed
		}
		out.Reason = "implementation blocked: " + d.Reason
	}
	return out
}

// decideSubtaskPRFromStored applies the same rules to a persisted
// implementation_verifications record (migration 044). It is the fallback
// for a subtask that the database says passed but whose
// implementation.complete event this coordinator never saw (restart between
// the event and the PR). The stored record was validated on insert and the
// orchestrator only moved the subtask to passed on release/human_review, so
// the message's success flag is taken as consistent with the record; a
// stored failed/unverified outcome still blocks through ReleaseDecision.
//
// expect is the subtask being decided (the caller's work_id and task_id,
// plus its repository when known), never the record's own ids: the row was
// selected by task id, but the record must still prove it belongs to that
// subtask, or it blocks and contributes no SHA or files.
func decideSubtaskPRFromStored(ev *evidence.ImplementationEvidence, policy evidence.ReleasePolicy, expect evidence.MessageIdentity) subtaskOutcome {
	if ev == nil {
		return subtaskOutcome{
			TaskID: expect.TaskID,
			Action: evidence.ActionBlock,
			Label:  outcomeLabelBlocked,
			Reason: "no stored evidence for subtask",
		}
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return subtaskOutcome{
			TaskID: expect.TaskID,
			Action: evidence.ActionBlock,
			Label:  outcomeLabelBlocked,
			Reason: "stored evidence could not be re-encoded: " + err.Error(),
		}
	}
	return decideSubtaskPRFor(implementationCompletePayload{
		TaskID:   expect.TaskID,
		WorkID:   expect.WorkID,
		Success:  true,
		Evidence: raw,
	}, policy, evidence.CompatWindowStrict, expect.RepositoryID)
}

// outcomeForUnseenSubtask decides a subtask that the database says passed
// but for which neither a completion event nor a stored evidence record
// exists. Work-item state alone is not evidence, so this blocks in both
// compatibility windows: the legacy window only relaxes how a message
// without evidence is read, and here there is no message at all.
func outcomeForUnseenSubtask(taskID string) subtaskOutcome {
	return subtaskOutcome{
		TaskID: taskID,
		Action: evidence.ActionBlock,
		Label:  outcomeLabelBlocked,
		Reason: "no completion event and no stored evidence for subtask " + taskID + "; cannot open a PR from work-item state alone",
	}
}

// decideUnseenSubtask is the restart fallback for a subtask whose
// implementation.complete event this coordinator did not see: it judges
// the subtask's newest persisted evidence under that subtask's own product
// policy (siblings may belong to different products), and blocks when no
// record exists. workID is the parent, which the store keys rows by. The
// record is held to the identity of the subtask being decided, including
// its repository when the work item row names one; a lookup failure leaves
// the repository unknown, which skips only that one check.
func decideUnseenSubtask(ctx context.Context, items products.WorkItemLookup, store products.PolicyStore, evStore storedEvidenceLoader, workID uuid.UUID, taskID string) subtaskOutcome {
	stored := latestStoredEvidence(ctx, evStore, workID, taskID)
	if stored == nil {
		return outcomeForUnseenSubtask(taskID)
	}
	policy, repositoryID := loadPolicyForTask(ctx, items, store, taskID, workID)
	expect := evidence.MessageIdentity{
		WorkID:       workID.String(),
		TaskID:       taskID,
		RepositoryID: repositoryID,
	}
	return decideSubtaskPRFromStored(stored, policy, expect)
}

// repositoryIDString converts a work item's repository id for the evidence
// identity check; nil stays nil.
func repositoryIDString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func reasonDetail(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// requiresHumanReview reports whether any subtask in the PR was accepted on
// partially verified evidence (D2/D3): the PR must then carry the marker.
func requiresHumanReview(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if o.Action == evidence.ActionHumanReview {
			return true
		}
	}
	return false
}

// prTitleFor prefixes the PR title with the partial verification marker
// when any subtask requires human review. Idempotent, so a retried PR is
// not double-marked.
func prTitleFor(base string, outcomes []subtaskOutcome) string {
	if !requiresHumanReview(outcomes) {
		return base
	}
	if strings.HasPrefix(base, partialVerificationTitlePrefix) || strings.HasPrefix(base, securityReviewTitlePrefix) || strings.HasPrefix(base, zoneReviewTitlePrefix) {
		return base
	}
	if requiresPartialReview(outcomes) {
		if namesZoneReview(outcomes) {
			return partialVerificationTitlePrefix + zoneReviewTitlePrefix + base
		}
		return partialVerificationTitlePrefix + base
	}
	if requiresSecurityReview(outcomes) {
		return securityReviewTitlePrefix + base
	}
	return zoneReviewTitlePrefix + base
}

// requiresPartialReview reports whether any human-review subtask is there
// because of partially verified evidence (as opposed to a security or zone
// flag).
func requiresPartialReview(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if o.Action == evidence.ActionHumanReview && !o.SecurityReview && !o.ZoneReview {
			return true
		}
	}
	return false
}

// requiresSecurityReview reports whether any subtask is verified but
// flagged security_review_required (M3-03, D3).
func requiresSecurityReview(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if o.Action == evidence.ActionHumanReview && o.SecurityReview {
			return true
		}
	}
	return false
}

// namesZoneReview reports whether any human-review subtask touches a
// human_review zone, whatever its outcome.
func namesZoneReview(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if o.Action == evidence.ActionHumanReview && len(o.ZoneReviewNames) > 0 {
			return true
		}
	}
	return false
}

// requiresZoneReview reports whether any subtask is verified but flagged
// zone_review_required (codex zones, migration 063).
func requiresZoneReview(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if o.Action == evidence.ActionHumanReview && o.ZoneReview {
			return true
		}
	}
	return false
}

// humanReviewBodyMarker is the marker that heads a human-review PR body and
// the merge prompt: the partial marker whenever any subtask is partially
// verified, else the security-review marker when any reason is security,
// else the zone-review marker.
func humanReviewBodyMarker(outcomes []subtaskOutcome) string {
	if requiresPartialReview(outcomes) {
		return partialVerificationBodyMarker
	}
	if requiresSecurityReview(outcomes) {
		return securityReviewBodyMarker
	}
	return zoneReviewBodyMarker
}

// securityFilesFrom extracts the matched files from a security-flagged
// verified record's reason ("… security_review_paths: a, b"); "see the
// verification evidence" when the reason does not name them.
func securityFilesFrom(reason string) string {
	const key = "security_review_paths: "
	if i := strings.Index(reason, key); i >= 0 {
		files := reason[i+len(key):]
		if j := strings.Index(files, ";"); j >= 0 {
			files = files[:j]
		}
		if files = strings.TrimSpace(files); files != "" {
			return files
		}
	}
	return "see the verification evidence"
}

// changedFilesFor is the deduplicated, sorted union of the outcomes' changed
// files. Always non-nil so it round-trips through pgx as an empty array.
func changedFilesFor(outcomes []subtaskOutcome) []string {
	set := make(map[string]struct{})
	for _, o := range outcomes {
		for _, f := range o.ChangedFiles {
			if f != "" {
				set[f] = struct{}{}
			}
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// verifiedHeadSHA returns the single head SHA the outcomes agree on. ok is
// false when none carry a SHA or when they disagree (the PR body then lists
// each subtask's SHA instead of asserting one).
func verifiedHeadSHA(outcomes []subtaskOutcome) (string, bool) {
	sha := ""
	for _, o := range outcomes {
		if o.HeadSHA == "" {
			continue
		}
		if sha == "" {
			sha = o.HeadSHA
			continue
		}
		if o.HeadSHA != sha {
			return "", false
		}
	}
	return sha, sha != ""
}

// prBodyInput is everything renderPRBody needs. DiffStat and Diff are the
// raw `git diff --stat` / `git diff` outputs gathered by the caller (empty
// when the clone is unavailable) so the renderer itself is pure.
type prBodyInput struct {
	Title      string
	Branch     string
	BaseBranch string
	DiffStat   string
	Diff       string
	Outcomes   []subtaskOutcome
	// Requirements are the criteria whose text targets the PR text
	// (loadPRTextRequirements, R13 D3), listed verbatim under
	// "Requirements stated for this change"; one with no id is an intake
	// sentence (R15 T2).
	Requirements []plan.Criterion
}

// renderPRBody builds the PR description. The task list shows each
// subtask's outcome label and verified head SHA; the changed-file list comes
// from the outcomes (evidence), never from the planner; a partially verified
// PR opens with the human-review marker so it is the first thing a reviewer
// sees.
func renderPRBody(in prBodyInput) string {
	var sb strings.Builder

	if requiresHumanReview(in.Outcomes) {
		sb.WriteString("> **" + humanReviewBodyMarker(in.Outcomes) + "**\n>\n")
		if requiresPartialReview(in.Outcomes) {
			sb.WriteString("> This change was accepted on partially verified evidence under the product's explicit opt-in. ")
			sb.WriteString("It must not be merged without a human reviewing the checks that did not run.\n")
		}
		if requiresSecurityReview(in.Outcomes) {
			sb.WriteString("> This change is verified but touches the repository's security-review paths. ")
			sb.WriteString("It must not be merged without a security review.\n")
		}
		if requiresZoneReview(in.Outcomes) {
			sb.WriteString("> This change is verified but touches codex zones whose policy is human_review. ")
			sb.WriteString("It must not be merged without a human approving it.\n")
		}
		for _, o := range in.Outcomes {
			if o.Action != evidence.ActionHumanReview {
				continue
			}
			if o.SecurityReview || o.ZoneReview {
				if o.SecurityReview {
					sb.WriteString(fmt.Sprintf(">\n> - %s: verified; touches security-review paths: %s\n", subtaskDisplay(o), o.SecurityFiles))
				}
				if o.ZoneReview {
					sb.WriteString(fmt.Sprintf(">\n> - %s: verified; touches human_review codex zones: %s\n", subtaskDisplay(o), strings.Join(o.ZoneReviewNames, ", ")))
				}
				continue
			}
			sb.WriteString(fmt.Sprintf(">\n> - %s: %s\n", subtaskDisplay(o), o.Reason))
			if len(o.ZoneReviewNames) > 0 {
				sb.WriteString(fmt.Sprintf(">\n> - %s: partially verified; touches human_review codex zones: %s\n", subtaskDisplay(o), strings.Join(o.ZoneReviewNames, ", ")))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Summary\n\n")
	sb.WriteString(fmt.Sprintf("%s\n\nBranch: `%s`\n", in.Title, in.Branch))
	if in.BaseBranch != "" {
		sb.WriteString(fmt.Sprintf("Base branch: `%s`\n", in.BaseBranch))
	}
	if sha, ok := verifiedHeadSHA(in.Outcomes); ok {
		sb.WriteString(fmt.Sprintf("Verified head SHA: `%s`\n", sha))
	}
	sb.WriteString("\n")

	sb.WriteString(renderPRRequirements(in.Requirements))
	sb.WriteString(renderVerificationSection(in.Outcomes))
	sb.WriteString(renderZonesSection(in.Outcomes))

	if strings.TrimSpace(in.DiffStat) != "" {
		sb.WriteString("## Changes\n\n```\n")
		sb.WriteString(in.DiffStat)
		if !strings.HasSuffix(in.DiffStat, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}

	if strings.TrimSpace(in.Diff) != "" {
		detail := in.Diff
		if len(detail) > maxDiffInBody {
			detail = detail[:maxDiffInBody] + "\n... (truncated)"
		}
		sb.WriteString("## Diff\n\n```diff\n")
		sb.WriteString(detail)
		sb.WriteString("\n```\n\n")
	}

	if len(in.Outcomes) > 0 {
		sb.WriteString("## Subtasks\n\n")
		for _, o := range in.Outcomes {
			line := fmt.Sprintf("- %s: **%s**", subtaskDisplay(o), o.Label)
			if o.HeadSHA != "" {
				line += fmt.Sprintf(" — head `%s`", o.HeadSHA)
			}
			if o.Action != evidence.ActionRelease || o.Legacy {
				line += " — " + o.Reason
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}

	files := changedFilesFor(in.Outcomes)
	if len(files) > 0 {
		if allLegacy(in.Outcomes) {
			sb.WriteString("## Changed files (from legacy message; not git-verified)\n\n")
		} else {
			sb.WriteString("## Changed files (from verification evidence)\n\n")
		}
		for _, f := range files {
			sb.WriteString(fmt.Sprintf("- `%s`\n", f))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// renderZonesSection lists the codex zones (migration 063) the PR's
// changes fall in, with each zone's policy and matching files, merged over
// the subtasks; empty when no zone was touched.
func renderZonesSection(outcomes []subtaskOutcome) string {
	type zoneRow struct {
		policy string
		files  map[string]bool
	}
	rows := map[string]*zoneRow{}
	var names []string
	for _, o := range outcomes {
		for _, z := range o.Zones {
			r, ok := rows[z.Name]
			if !ok {
				r = &zoneRow{policy: z.Policy, files: map[string]bool{}}
				rows[z.Name] = r
				names = append(names, z.Name)
			}
			for _, f := range z.Files {
				r.files[f] = true
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString("## Codex zones touched\n\n| Zone | Policy | Files |\n|---|---|---|\n")
	for _, n := range names {
		r := rows[n]
		files := make([]string, 0, len(r.files))
		for f := range r.files {
			files = append(files, "`"+f+"`")
		}
		sort.Strings(files)
		sb.WriteString(fmt.Sprintf("| `%s` | %s | %s |\n", n, r.policy, strings.Join(files, ", ")))
	}
	sb.WriteString("\n")
	return sb.String()
}

func allLegacy(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if !o.Legacy {
			return false
		}
	}
	return len(outcomes) > 0
}

// subtaskDisplay is the summary when present (truncated), else the task id.
func subtaskDisplay(o subtaskOutcome) string {
	label := o.TaskID
	if o.Summary != "" {
		label = o.Summary
		if len(label) > 120 {
			label = label[:120] + "..."
		}
	}
	return label
}

// storedEvidenceLoader is the slice of *evidence.Store used for the restart
// fallback.
type storedEvidenceLoader interface {
	GetByWorkItem(ctx context.Context, workID uuid.UUID) ([]evidence.Verification, error)
}

// latestStoredEvidence returns the newest persisted record for taskID under
// workID, or nil when none exists or the store is unavailable.
func latestStoredEvidence(ctx context.Context, store storedEvidenceLoader, workID uuid.UUID, taskID string) *evidence.ImplementationEvidence {
	if store == nil {
		return nil
	}
	rows, err := store.GetByWorkItem(ctx, workID)
	if err != nil {
		log.Printf("pr-coordinator: load stored evidence for work_id=%s: %v", workID, err)
		return nil
	}
	for i := range rows {
		if rows[i].Evidence.TaskID == taskID {
			ev := rows[i].Evidence
			return &ev
		}
	}
	return nil
}
