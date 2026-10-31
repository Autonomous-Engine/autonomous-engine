package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// R14: pr-coordinator consumes verification.complete, not
// implementation.complete. The verification stage (review-agent) verified
// the subtask's head before any PR exists; its decision is applied on top of
// the unchanged evidence decision (decideSubtaskPRFor on the
// implementation.complete payload the message carries verbatim):
//
//	approve / approve_pending_second_pass → the PR (or the manual gate's
//	    pending change) exactly as before, the latter labelled with
//	    stage.SecondPassTitlePrefix, the body gaining "## Verification";
//	escalate / request_changes / error → no PR and no pending change: the
//	    subtask's outcome blocks with the verdict as the reason.
//	    status-orchestrator owns the parent's transition (human_review with
//	    the verdict and its prompt, implementation with the defects, or the
//	    stage's block), so exactly one consumer moves the parent;
//	not_run → the evidence decision stands only for a legacy-window message
//	    without evidence (its pre-evidence path, verified on pr.ready);
//	    anything else blocks.
//
// D1 holds: a PR needs a releasable evidence outcome AND an approve
// decision for the same head.

// verification is the verification decision one subtask's PR depends on.
type verification struct {
	// Known: a decision was received or recorded for the subtask.
	Known    bool
	Decision stage.Decision
	Reason   string
	HeadSHA  string
	Legacy   bool
	Reused   bool
	Criteria []stage.CriterionSummary
	Gaps     []string
	Defects  verifier.DefectCounts
}

// verificationFromMessage reads a verification.complete message.
func verificationFromMessage(m stage.Message) verification {
	return verification{
		Known: m.Decision.Valid(), Decision: m.Decision, Reason: m.Reason, HeadSHA: m.HeadSHA, Legacy: m.Legacy,
		Reused: m.Reused, Criteria: m.Criteria, Gaps: m.Gaps, Defects: m.Defects,
	}
}

// verificationQuerier is what the stored-result fallback reads with
// (*pgxpool.Pool in production).
type verificationQuerier = verifier.Querier

// storedVerification is the restart fallback for a subtask whose
// verification.complete this coordinator did not see: its newest
// verification_results row, with the verdict's criteria when it has one.
// Unknown when there is none or the read fails (which blocks the PR).
func storedVerification(ctx context.Context, q verificationQuerier, taskID string) verification {
	if q == nil {
		return verification{}
	}
	id, err := uuid.Parse(taskID)
	if err != nil {
		return verification{}
	}
	r, ok, err := stage.LatestForTask(ctx, q, id)
	if err != nil {
		log.Printf("pr-coordinator: %v", err)
		return verification{}
	}
	if !ok {
		return verification{}
	}
	v := verification{Known: r.Decision.Valid(), Decision: r.Decision, Reason: r.Reason, HeadSHA: r.HeadSHA, Legacy: r.Legacy}
	if r.VerdictArtifactID != nil {
		if c, err := stage.LoadArtifactContent(ctx, q, *r.VerdictArtifactID); err == nil {
			v.Criteria = stage.Summaries(c.Verdict)
			v.Gaps = c.Verdict.Gaps
			v.Defects = c.Defects
		} else {
			log.Printf("pr-coordinator: task %s: %v", taskID, err)
		}
	}
	return v
}

// verifiedSubtasks are the subtasks of workID with a recorded verification
// decision (verification_results), keyed by id: the restart fallback of
// the "all siblings done" count. A read failure is logged and yields none,
// which can only make the coordinator wait.
func verifiedSubtasks(ctx context.Context, wg *workgraph.WorkGraph, workID uuid.UUID) map[string]bool {
	out := map[string]bool{}
	if wg == nil || wg.DB() == nil {
		return out
	}
	rs, err := stage.LatestByTask(ctx, wg.DB(), workID)
	if err != nil {
		log.Printf("pr-coordinator: work_id=%s: %v", workID, err)
		return out
	}
	for id := range rs {
		out[id.String()] = true
	}
	return out
}

// releaseApproved reports whether an outcome's verification lets the
// parent's change ship: an approval, or a legacy-window message without
// evidence (not_run, its pre-evidence path).
func releaseApproved(o subtaskOutcome) bool {
	v := o.Verification
	if v == nil || !v.Known {
		return false
	}
	return v.Decision.Approves() || (v.Decision == stage.DecisionNotRun && v.Legacy && o.Legacy)
}

// roundParticipant reports whether a subtask child takes part in the
// parent's current round (status-orchestrator's rule): not ended, and not
// passed before the parent last re-entered implementation from a later
// state.
func roundParticipant(c *models.WorkItem, passedAt, roundStart time.Time) bool {
	switch c.State {
	case models.WorkItemStateCancelled, models.WorkItemStateFailed, models.WorkItemStateBlocked:
		return false
	}
	return roundStart.IsZero() || passedAt.IsZero() || !passedAt.Before(roundStart)
}

// parentRoundStart is the parent's current round start, the definition
// shared with status-orchestrator (workgraph.RoundStart: re-entry to
// implementation from verification, review, human_review, qa, failed or
// blocked, or a codex re-plan; zero: the first round).
func parentRoundStart(ts []*models.WorkItemTransition) time.Time { return workgraph.RoundStart(ts) }

func lastPassedAt(ts []*models.WorkItemTransition) time.Time { return workgraph.LastPassed(ts) }

// roundView is the parent's current round (codex coherence Part 1 review):
// which subtask children take part in it. A subtask superseded by a codex
// re-plan, created before the parent's last codex re-plan, or passed before
// the round started is outside it: it is neither counted as done nor given
// a PR, and takes no part in all-or-nothing.
type roundView struct {
	round workgraph.Round
	in    map[uuid.UUID]bool
}

// loadRoundView reads the round from the work graph. Without a database, or
// when the parent's transitions cannot be read, every subtask is in the
// round (the earlier behaviour).
func loadRoundView(ctx context.Context, wg *workgraph.WorkGraph, workID uuid.UUID, children []*models.WorkItem) roundView {
	rv := roundView{in: map[uuid.UUID]bool{}}
	for _, c := range children {
		rv.in[c.ID] = true
	}
	if wg == nil || wg.DB() == nil {
		return rv
	}
	ts, err := wg.GetTransitions(ctx, workID)
	if err != nil {
		log.Printf("pr-coordinator: work_id=%s: read transitions for the round: %v", workID, err)
		return rv
	}
	rv.round = workgraph.NewRound(ts)
	for _, c := range children {
		if c.Type != models.WorkItemTypeSubtask {
			continue
		}
		rc := workgraph.RoundChild{State: c.State, CreatedAt: c.CreatedAt}
		if cts, err := wg.GetTransitions(ctx, c.ID); err == nil {
			rc.PassedAt = workgraph.LastPassed(cts)
			rc.Superseded = workgraph.Superseded(c.State, cts)
		}
		rv.in[c.ID] = rv.round.InRound(rc)
	}
	return rv
}

// inRound keeps the children that take part in the round (non-subtasks are
// kept: they are not judged here).
func (rv roundView) inRound(children []*models.WorkItem) []*models.WorkItem {
	out := make([]*models.WorkItem, 0, len(children))
	for _, c := range children {
		if c.Type != models.WorkItemTypeSubtask || rv.in[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// codexReplanHold reports why the parent's PR must wait for a codex re-plan
// (codex coherence Part 1 review): the parent is in planning (a new plan is
// being made), or a codex:replan escalation is pending for it (it is parked
// in awaiting_user while its subtasks run). "" when there is no hold.
func codexReplanHold(ctx context.Context, wg *workgraph.WorkGraph, workID uuid.UUID) string {
	if wg == nil || wg.DB() == nil {
		return ""
	}
	if item, err := wg.GetWorkItem(ctx, workID); err == nil && item != nil && item.State == models.WorkItemStatePlanning {
		return "the parent is in planning"
	}
	var pending bool
	if err := wg.DB().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_interactions WHERE work_item_id = $1 AND trigger = $2 AND state = 'pending')`,
		workID, string(interactions.TriggerCodexReplan)).Scan(&pending); err != nil {
		log.Printf("pr-coordinator: work_id=%s: codex re-plan escalation check: %v — holding the PR", workID, err)
		return "the codex re-plan escalation check failed"
	}
	if pending {
		return "a codex re-plan escalation (Re-plan / Continue as is / Cancel) is pending"
	}
	return ""
}

// allOrNothingRefusal applies the owner's all-or-nothing ruling (R14
// review): once every sibling is done, no subtask of the parent gets a PR
// or a pending change when any subtask taking part in the round has a
// verification decision other than an approval (or the legacy-window
// pre-evidence path). outcomeOf yields a subtask's decided outcome. A
// subtask whose evidence already blocked it is not part of the release and
// does not refuse the others (unchanged behaviour). It returns "" when the
// release may proceed, else the refusal naming the first refusing subtask.
func allOrNothingRefusal(children []*models.WorkItem, passedAt map[uuid.UUID]time.Time, roundStart time.Time, outcomeOf func(c *models.WorkItem) subtaskOutcome) string {
	for _, c := range children {
		if c.Type != models.WorkItemTypeSubtask || !roundParticipant(c, passedAt[c.ID], roundStart) {
			continue
		}
		o := outcomeOf(c)
		if o.EvidenceBlocked || releaseApproved(o) {
			continue
		}
		decision := "none"
		if o.Verification != nil && o.Verification.Known {
			decision = string(o.Verification.Decision)
		}
		return fmt.Sprintf("subtask %s verification decision %s (%s): no PR or pending change for any subtask of the parent (all-or-nothing)", c.ID, decision, o.Reason)
	}
	return ""
}

// allOrNothingCheck gathers what allOrNothingRefusal needs from the work
// graph: each subtask's outcome (the session's, else the stored evidence
// and verification), its last pass and the parent's round start. A
// transitions read failure leaves the times zero, which counts the subtask
// in (the stricter reading).
func allOrNothingCheck(ctx context.Context, wg *workgraph.WorkGraph, store products.PolicyStore, evStore storedEvidenceLoader, session *workSession, children []*models.WorkItem, workID uuid.UUID) string {
	var vq verificationQuerier
	if wg != nil && wg.DB() != nil {
		vq = wg.DB()
	}
	passedAt := map[uuid.UUID]time.Time{}
	var start time.Time
	if vq != nil {
		if ts, err := wg.GetTransitions(ctx, workID); err == nil {
			start = parentRoundStart(ts)
		}
		for _, c := range children {
			if c.Type == models.WorkItemTypeSubtask && c.State == models.WorkItemStatePassed {
				if ts, err := wg.GetTransitions(ctx, c.ID); err == nil {
					passedAt[c.ID] = lastPassedAt(ts)
				}
			}
		}
	}
	return allOrNothingRefusal(children, passedAt, start, func(c *models.WorkItem) subtaskOutcome {
		session.mu.Lock()
		o, seen := session.outcomes[c.ID.String()]
		session.mu.Unlock()
		if seen {
			return o
		}
		o = decideUnseenSubtask(ctx, wg, store, evStore, workID, c.ID.String())
		return applyVerification(o, storedVerification(ctx, vq, c.ID.String()))
	})
}

// Outcome labels for a verification that stops the PR.
const (
	outcomeLabelVerifierEscalated      = "escalated by the independent verifier"
	outcomeLabelVerifierChanges        = "changes requested by the independent verifier"
	outcomeLabelVerificationFailed     = "verification could not run"
	outcomeLabelVerificationNotRun     = "not verified"
	outcomeLabelVerificationMissing    = "no verification decision"
	outcomeLabelVerificationHeadDiffer = "verified a different head"
)

// applyVerification applies the verification decision to the evidence
// outcome (see the file comment). A subtask whose evidence already blocks
// keeps that block.
func applyVerification(o subtaskOutcome, v verification) subtaskOutcome {
	vv := v
	o.Verification = &vv
	if o.Action == evidence.ActionBlock {
		o.EvidenceBlocked = true
		return o
	}
	block := func(label, reason string) subtaskOutcome {
		o.Action, o.Label, o.Reason = evidence.ActionBlock, label, reason
		return o
	}
	if !v.Known {
		return block(outcomeLabelVerificationMissing, "no verification decision recorded for the subtask; no PR without the independent verifier's approval")
	}
	switch {
	case v.Decision.Approves():
		if v.HeadSHA != "" && o.HeadSHA != "" && v.HeadSHA != o.HeadSHA {
			return block(outcomeLabelVerificationHeadDiffer, fmt.Sprintf("the independent verifier approved head %s but the evidence's verified head is %s; no PR", v.HeadSHA, o.HeadSHA))
		}
		o.SecondPassPending = v.Decision == stage.DecisionApprovePendingSecondPass
		return o
	case v.Decision == stage.DecisionNotRun:
		if o.Legacy && v.Legacy {
			return o
		}
		return block(outcomeLabelVerificationNotRun, "the independent verifier did not run ("+v.Reason+"); no PR without its approval")
	case v.Decision == stage.DecisionEscalate:
		return block(outcomeLabelVerifierEscalated, "independent verifier escalated before the PR: "+v.Reason)
	case v.Decision == stage.DecisionRequestChanges:
		return block(outcomeLabelVerifierChanges, "independent verifier requested changes before the PR: "+v.Reason)
	case v.Decision == stage.DecisionError:
		return block(outcomeLabelVerificationFailed, "verification before the PR could not run: "+v.Reason)
	}
	return block(outcomeLabelVerificationMissing, fmt.Sprintf("unknown verification decision %q; no PR", v.Decision))
}

// payloadFromVerification is the implementation.complete payload the
// verification.complete message carries. A message without one (it should
// not happen) is read as a message with no evidence that did not succeed,
// which blocks in both windows.
func payloadFromVerification(m stage.Message) implementationCompletePayload {
	var p implementationCompletePayload
	if len(m.Implementation) > 0 {
		if err := json.Unmarshal(m.Implementation, &p); err != nil {
			log.Printf("pr-coordinator: verification.complete work_id=%s task=%s: implementation payload unreadable: %v", m.WorkID, m.TaskID, err)
			p = implementationCompletePayload{}
		}
	}
	if p.WorkID == "" {
		p.WorkID = m.WorkID
	}
	if p.TaskID == "" {
		p.TaskID = m.TaskID
	}
	return p
}

// secondPassPending reports whether any subtask of the PR was approved by
// the verifier pending the second-pass review.
func secondPassPending(outcomes []subtaskOutcome) bool {
	for _, o := range outcomes {
		if o.SecondPassPending {
			return true
		}
	}
	return false
}

// verificationTitle prefixes the title with stage.SecondPassTitlePrefix
// when the second-pass review decides. Idempotent. prTitleFor's
// human-review markers go in front of it.
func verificationTitle(base string, outcomes []subtaskOutcome) string {
	if !secondPassPending(outcomes) || strings.Contains(base, stage.SecondPassTitlePrefix) {
		return base
	}
	return stage.SecondPassTitlePrefix + base
}

// maxCriteriaInBody bounds the criteria table per subtask.
const maxCriteriaInBody = 40

// renderVerificationSection is the PR body's "## Verification": per subtask
// the decision, its reason, every criterion's result and the recorded gaps.
// "" when no outcome carries a verification.
func renderVerificationSection(outcomes []subtaskOutcome) string {
	var sb strings.Builder
	for _, o := range outcomes {
		v := o.Verification
		if v == nil {
			continue
		}
		if sb.Len() == 0 {
			sb.WriteString("## Verification\n\n")
			sb.WriteString("The independent verifier checked this change against its acceptance criteria before the PR was opened; it never sees this description.\n\n")
		}
		line := fmt.Sprintf("- %s: **%s**", subtaskDisplay(o), v.Decision)
		if v.HeadSHA != "" {
			line += fmt.Sprintf(" — head `%s`", v.HeadSHA)
		}
		if v.Reason != "" {
			line += " — " + stage.OneLine(v.Reason, 500)
		}
		if v.Reused {
			line += " (verdict recorded earlier for this head)"
		}
		sb.WriteString(line + "\n")
		if v.Defects.Critical+v.Defects.Major+v.Defects.Minor > 0 {
			sb.WriteString(fmt.Sprintf("  - Defects: %d critical, %d major, %d minor\n", v.Defects.Critical, v.Defects.Major, v.Defects.Minor))
		}
		if len(v.Criteria) > 0 {
			sb.WriteString("\n  | Criterion | Result | Note |\n  |---|---|---|\n")
			for i, c := range v.Criteria {
				if i == maxCriteriaInBody {
					sb.WriteString(fmt.Sprintf("  | … | | %d more (see the verdict) |\n", len(v.Criteria)-maxCriteriaInBody))
					break
				}
				sb.WriteString(fmt.Sprintf("  | %s | %s | %s |\n", tableCell(c.ID), tableCell(c.Status), tableCell(c.Note)))
			}
		}
		if len(v.Gaps) > 0 {
			sb.WriteString("\n  Gaps a human must check:\n")
			for _, g := range v.Gaps {
				sb.WriteString("  - " + stage.OneLine(g, 500) + "\n")
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func tableCell(s string) string {
	return strings.ReplaceAll(stage.OneLine(s, 300), "|", `\|`)
}

// verificationPromptLine is the gate prompt's verdict line (V5: the operator
// decides with the verdict in front of them).
func verificationPromptLine(outcomes []subtaskOutcome) string {
	var parts []string
	for _, o := range outcomes {
		if v := o.Verification; v != nil {
			met, total := 0, len(v.Criteria)
			for _, c := range v.Criteria {
				if c.Status == string(verifier.StatusMet) {
					met++
				}
			}
			part := fmt.Sprintf("Independent verifier: %s (%d/%d criteria met)", v.Decision, met, total)
			if v.Reason != "" {
				part += " — " + stage.OneLine(v.Reason, 300)
			}
			if len(v.Gaps) > 0 {
				part += fmt.Sprintf("; %d gap(s) to check by hand", len(v.Gaps))
			}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

// verificationPayload is the gate prompt's context_payload "verification".
func verificationPayload(outcomes []subtaskOutcome) []map[string]any {
	var out []map[string]any
	for _, o := range outcomes {
		if v := o.Verification; v != nil {
			out = append(out, map[string]any{
				"task_id":  o.TaskID,
				"decision": string(v.Decision),
				"reason":   v.Reason,
				"head_sha": v.HeadSHA,
				"criteria": v.Criteria,
				"gaps":     v.Gaps,
				"defects":  v.Defects,
				"reused":   v.Reused,
			})
		}
	}
	return out
}
