package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The compatibility window for implementation.complete messages without
// evidence (EVIDENCE_COMPAT_WINDOW) is evidence.CompatWindow, read through
// evidence.CompatWindowFromEnv so every consumer of the topic applies the
// same parsing and the same strict fallback for unrecognised values. The
// legacy-fallback counter is metrics.EvidenceCompatLegacyFallback, shared
// for the same reason.

// partialVerificationMarker prefixes the transition reason recorded when a
// subtask completes on partially_verified evidence with the product opted
// in. The subtask itself moves to passed (the state machine has no
// implementation→human_review edge for subtasks, and the dashboard resolves
// human_review prompts by moving the item to QA, which is meaningless for a
// subtask); the marker is what parentHumanReviewGate reads to force the
// parent into mandatory human review before it can reach QA (D2, D3).
//
// LOAD-BEARING: this exact prefix on a transition-to-passed reason is the
// only persisted record of the gate. partiallyVerifiedSubtasks matches it
// with strings.HasPrefix; changing the text, or writing a subtask's passed
// transition with a different reason, silently drops the gate for that
// subtask. Keep it stable and never reuse it for another meaning.
const partialVerificationMarker = "partially verified: mandatory human review required before QA"

// securityReviewMarker is the counterpart of partialVerificationMarker for a
// subtask whose evidence is verified but carries the
// security_review_required flag (M3-03, D3): the change touches the
// repository's security-review paths. It gates the parent exactly like the
// partial marker (partiallyVerifiedSubtasks matches both) but says why, so
// a fully verified change is never described as partially verified.
//
// LOAD-BEARING in the same way as partialVerificationMarker.
const securityReviewMarker = "security review required: mandatory human review required before QA"

// zoneReviewMarker is the counterpart for verified evidence carrying
// zone_review_required (codex zones, migration 063): the change touches a
// human_review zone of the repository. It gates the parent like the other
// two markers, whatever HUMAN_REVIEW_REQUIRED says.
//
// LOAD-BEARING in the same way as partialVerificationMarker.
const zoneReviewMarker = "zone review required: mandatory human review required before QA"

// humanReviewMarkerFor picks the transition marker for a subtask that
// ReleaseDecision sent to human review: securityReviewMarker when its
// evidence is verified and flagged for security review, zoneReviewMarker
// when it is verified and flagged for a human_review zone, the partial
// marker otherwise (including when the record cannot be decoded here).
func humanReviewMarkerFor(payload implementationCompletePayload) string {
	if isSecurityReviewEvidence(payload.Evidence) {
		return securityReviewMarker
	}
	if isZoneReviewEvidence(payload.Evidence) {
		return zoneReviewMarker
	}
	return partialVerificationMarker
}

// isGateMarker reports whether a subtask's pass reason carries one of the
// human-review gate markers.
func isGateMarker(reason string) bool {
	return strings.HasPrefix(reason, partialVerificationMarker) ||
		strings.HasPrefix(reason, securityReviewMarker) ||
		strings.HasPrefix(reason, zoneReviewMarker)
}

// noParentHumanReviewReason is the block reason for a human-review decision
// on an item with no parent to gate.
func noParentHumanReviewReason(payload implementationCompletePayload, reason string) string {
	if isSecurityReviewEvidence(payload.Evidence) {
		return "implementation blocked: verified work item touches security-review paths but has no parent to gate into human review (" + reason + ")"
	}
	if isZoneReviewEvidence(payload.Evidence) {
		return "implementation blocked: verified work item touches a human_review codex zone but has no parent to gate into human review (" + reason + ")"
	}
	if zoneReviewTouched(payload.Evidence) {
		return "implementation blocked: partially verified work item touching a human_review codex zone has no parent to gate into human review (" + reason + ")"
	}
	return "implementation blocked: partially verified work item has no parent to gate into human review (" + reason + ")"
}

// isSecurityReviewEvidence reports whether raw decodes to verified evidence
// carrying evidence.FlagSecurityReviewRequired.
func isSecurityReviewEvidence(raw []byte) bool {
	var ev evidence.ImplementationEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false
	}
	return ev.Outcome == evidence.OutcomeVerified && ev.HasFlag(evidence.FlagSecurityReviewRequired)
}

// zoneReviewTouched reports whether raw decodes to evidence carrying
// evidence.FlagZoneReviewRequired, whatever its outcome — the
// pr-coordinator's ZoneReviewNames rule. A partially verified run in a
// human_review zone keeps the partial marker (the partial gate is the
// stricter reason) and its reasons name the zone.
func zoneReviewTouched(raw []byte) bool {
	var ev evidence.ImplementationEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false
	}
	return ev.HasFlag(evidence.FlagZoneReviewRequired)
}

// isZoneReviewEvidence reports whether raw decodes to verified evidence
// carrying evidence.FlagZoneReviewRequired (the zone is the only reason
// for the review: the pr-coordinator's ZoneReview).
func isZoneReviewEvidence(raw []byte) bool {
	var ev evidence.ImplementationEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false
	}
	return ev.Outcome == evidence.OutcomeVerified && ev.HasFlag(evidence.FlagZoneReviewRequired)
}

// hasEvidence reports whether the message carries an evidence value at all.
// An absent field and an explicit JSON null are both "no evidence" and fall
// under the compatibility window; anything else (including {} or malformed
// JSON) is present and is judged by evidence.ReleaseDecisionForMessage.
func (p implementationCompletePayload) hasEvidence() bool {
	raw := bytes.TrimSpace(p.Evidence)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// identityFor is what the message says about itself plus the subtask's
// repository, used to reject a record that belongs to another subtask (a
// producer bug or a replayed message). The ids are the message's own,
// exactly as received; repositoryID is the subtask's work item row's
// repository, nil when it has none (sibling-repository fallback items) or
// the caller could not load it, which skips only the repository check.
func (p implementationCompletePayload) identityFor(repositoryID *string) evidence.MessageIdentity {
	return evidence.MessageIdentity{WorkID: p.WorkID, TaskID: p.TaskID, RepositoryID: repositoryID}
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

// decideSubtaskTransition is the pure decision for an implementation.complete
// message. It returns the state the subtask should take and an
// operator-readable reason:
//
//	passed       — evidence.ReleaseDecision returned release (verified)
//	human_review — ReleaseDecision returned human_review (partially_verified,
//	               product opted in). The caller realises this as the subtask
//	               passing with partialVerificationMarker and the parent being
//	               gated into human_review (see partiallyVerifiedSubtasks).
//	blocked      — everything else, including no evidence in strict mode,
//	               unknown schema versions in either mode, a failed legacy
//	               message in legacy mode, and a record whose work_id or
//	               task_id is not the message's (identity mismatch).
//
// policy comes from products.LoadReleasePolicyForItem; its zero value
// (unknown product or lookup failure) is "not opted in", so
// partially_verified blocks. The record must also match the message's ids;
// decideSubtaskTransition checks work_id and task_id only,
// decideSubtaskTransitionFor additionally the subtask's repository when the
// caller has it.
func decideSubtaskTransition(payload implementationCompletePayload, policy evidence.ReleasePolicy, window evidence.CompatWindow) (models.WorkItemState, string) {
	return decideSubtaskTransitionFor(payload, policy, window, nil)
}

// decideSubtaskTransitionFor is decideSubtaskTransition with the subtask's
// repository (from its work item row) added to the identity the record
// must match. nil skips the repository check.
func decideSubtaskTransitionFor(payload implementationCompletePayload, policy evidence.ReleasePolicy, window evidence.CompatWindow, repositoryID *string) (models.WorkItemState, string) {
	if !payload.hasEvidence() {
		if window == evidence.CompatWindowStrict {
			reason := "no evidence attached to implementation.complete (" + evidence.CompatWindowEnv + "=strict)"
			if payload.Reason != "" {
				reason += "; implementation reported: " + payload.Reason
			}
			return models.WorkItemStateBlocked, reason
		}
		// Legacy window: the pre-evidence contract.
		if payload.Success {
			return models.WorkItemStatePassed, "implementation complete (legacy message without evidence)"
		}
		return models.WorkItemStateBlocked, "implementation failed: " + payload.Reason
	}

	// The record must be this message's: a verified record for another
	// subtask must never pass this one (D1.3).
	d := evidence.ReleaseDecisionForMessage(payload.Evidence, policy, payload.identityFor(repositoryID))
	switch d.Action {
	case evidence.ActionRelease:
		if !payload.Success {
			// A producer that reports success=false alongside verified
			// evidence is contradicting itself; fail closed.
			return models.WorkItemStateBlocked, "implementation blocked: evidence is verified but the message reports success=false" + reasonDetail(payload.Reason)
		}
		return models.WorkItemStatePassed, "implementation complete: " + d.Reason
	case evidence.ActionHumanReview:
		return models.WorkItemStateHumanReview, d.Reason
	default:
		return models.WorkItemStateBlocked, "implementation blocked: " + d.Reason
	}
}

func reasonDetail(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// workItemStore is the slice of *workgraph.WorkGraph the gate logic and the
// outbox write paths need. It exists so the post-review target resolution and
// the transactional publish helpers can be tested against a fake without a
// database. The writes are the *Tx variants only (M1-04a): every write this
// service makes commits together with the events that announce it.
type workItemStore interface {
	GetWorkItem(ctx context.Context, id uuid.UUID) (*models.WorkItem, error)
	GetChildren(ctx context.Context, parentID uuid.UUID) ([]*models.WorkItem, error)
	GetTransitions(ctx context.Context, workItemID uuid.UUID) ([]*models.WorkItemTransition, error)
	TransitionStateTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) (models.WorkItemState, error)
	CreateWorkItemTx(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error
}

// partiallyVerifiedSubtasks returns the subtask children whose most recent
// transition to passed carries partialVerificationMarker, i.e. whose
// implementation was accepted on partially_verified evidence. A subtask that
// was later retried and passed cleanly is not returned: only its latest pass
// counts. transitionsOf must return the child's transitions in timestamp
// order (workgraph.GetTransitions does).
func partiallyVerifiedSubtasks(children []*models.WorkItem, transitionsOf func(uuid.UUID) []*models.WorkItemTransition) []*models.WorkItem {
	var gated []*models.WorkItem
	for _, child := range children {
		if child == nil || child.Type != models.WorkItemTypeSubtask {
			continue
		}
		var lastPass *models.WorkItemTransition
		for _, t := range transitionsOf(child.ID) {
			if t != nil && t.ToState == models.WorkItemStatePassed {
				lastPass = t
			}
		}
		if lastPass != nil && isGateMarker(lastPass.Reason) {
			gated = append(gated, child)
		}
	}
	return gated
}

// parentHumanReviewGate reports whether parentID must pass mandatory human
// review before QA because at least one subtask completed on partially
// verified evidence. It fails closed: when the children or their transitions
// cannot be read, gated is true and reason says why, so a database error can
// never release a parent past the human gate.
func parentHumanReviewGate(ctx context.Context, wg workItemStore, parentID uuid.UUID) (gated bool, reason string) {
	children, err := wg.GetChildren(ctx, parentID)
	if err != nil {
		return true, fmt.Sprintf("cannot determine subtask verification status (get children: %v); holding for human review", err)
	}
	var readErr error
	gatedChildren := partiallyVerifiedSubtasks(children, func(id uuid.UUID) []*models.WorkItemTransition {
		ts, err := wg.GetTransitions(ctx, id)
		if err != nil && readErr == nil {
			readErr = fmt.Errorf("get transitions for subtask %s: %w", id, err)
		}
		return ts
	})
	if readErr != nil {
		return true, fmt.Sprintf("cannot determine subtask verification status (%v); holding for human review", readErr)
	}
	if len(gatedChildren) == 0 {
		return false, ""
	}
	return true, humanReviewGateReason(gatedChildren, func(id uuid.UUID) []*models.WorkItemTransition {
		ts, _ := wg.GetTransitions(ctx, id)
		return ts
	})
}

// humanReviewGateReason names the actual cause of the gate per subtask so
// operators (and the e2e fixture) see "partially verified evidence" only
// for partial outcomes and "security-review paths" only for the security
// flag, instead of one blended sentence for both.
func humanReviewGateReason(gated []*models.WorkItem, transitionsOf func(uuid.UUID) []*models.WorkItemTransition) string {
	var partial, security, zone []string
	for _, c := range gated {
		name := fmt.Sprintf("%s (%s)", c.Title, c.ID)
		var lastPass *models.WorkItemTransition
		for _, t := range transitionsOf(c.ID) {
			if t != nil && t.ToState == models.WorkItemStatePassed {
				lastPass = t
			}
		}
		switch {
		case lastPass != nil && strings.HasPrefix(lastPass.Reason, securityReviewMarker):
			security = append(security, name)
		case lastPass != nil && strings.HasPrefix(lastPass.Reason, zoneReviewMarker):
			zone = append(zone, name)
		default:
			partial = append(partial, name)
		}
	}
	if len(zone) > 0 {
		parts := []string{fmt.Sprintf("%d touch human_review codex zones (%s)", len(zone), strings.Join(zone, "; "))}
		if len(partial) > 0 {
			parts = append(parts, fmt.Sprintf("%d completed on partially verified evidence (%s)", len(partial), strings.Join(partial, "; ")))
		}
		if len(security) > 0 {
			parts = append(parts, fmt.Sprintf("%d touch security-review paths (%s)", len(security), strings.Join(security, "; ")))
		}
		return fmt.Sprintf("%d subtask(s) require mandatory human review before QA: %s", len(gated), strings.Join(parts, "; "))
	}
	switch {
	case len(security) == 0:
		return fmt.Sprintf("%d subtask(s) completed on partially verified evidence and require mandatory human review before QA: %s", len(partial), strings.Join(partial, "; "))
	case len(partial) == 0:
		return fmt.Sprintf("%d subtask(s) touch security-review paths and require mandatory human review before QA: %s", len(security), strings.Join(security, "; "))
	default:
		return fmt.Sprintf("%d subtask(s) require mandatory human review before QA: %d completed on partially verified evidence (%s); %d touch security-review paths (%s)", len(partial)+len(security), len(partial), strings.Join(partial, "; "), len(security), strings.Join(security, "; "))
	}
}

// postReviewTarget is where a parent goes when it leaves review (on
// review.complete approved or pr.merged), resolved before any transition is
// made so the rule can be tested as a table.
type postReviewTarget struct {
	ToState models.WorkItemState
	Reason  string
	// Hold: the item is already in human_review; make no transition, the
	// pending prompt is the gate.
	Hold bool
	// Gated: the partial-verification gate applies (ToState is human_review
	// or Hold is set). GateReason is the operator text.
	Gated      bool
	GateReason string
	// PublishHumanReviewPrompt / PublishQAReady / PublishBlockedPrompt tell
	// the handler which side effect to perform for this target.
	PublishHumanReviewPrompt bool
	PublishQAReady           bool
	PublishBlockedPrompt     bool
}

// isReleaseTarget reports whether s is a state a gated parent may never be
// handed by this service: qa and everything after it.
func isReleaseTarget(s models.WorkItemState) bool {
	switch s {
	case models.WorkItemStateQA, models.WorkItemStatePassed, models.WorkItemStateReleased:
		return true
	}
	return false
}

// gatedTarget is the deny-by-target rule for a gated parent. normal is the
// state the pipeline would otherwise choose.
//
//   - already in human_review → hold (the pending prompt is the gate);
//   - normal is an earlier, non-release step (review) → keep it: the gate
//     is applied again when the parent leaves that step;
//   - otherwise the only permitted target is human_review when the state
//     machine has an edge to it from the current state;
//   - when it has none (e.g. implementation), blocked with a reason saying
//     human review is required but not reachable, so a human sees it
//     instead of the gate dead-ending silently;
//   - already past the human gate (qa, passed, released: a human approved
//     it out of human_review) or in a terminal / blocked state → hold; a
//     late pr.merged or review.complete must not disturb it.
//
// A gated parent is therefore never handed qa, passed, or released, whatever
// the agent definition or its current state.
func gatedTarget(current, normal models.WorkItemState) (toState models.WorkItemState, hold, blocked bool) {
	if current == models.WorkItemStateHumanReview {
		return current, true, false
	}
	if !isReleaseTarget(normal) && normal != models.WorkItemStateHumanReview {
		return normal, false, false
	}
	if !beforeHumanReview(current) {
		return current, true, false
	}
	if workgraph.IsValidTransition(current, models.WorkItemStateHumanReview) {
		return models.WorkItemStateHumanReview, false, false
	}
	if workgraph.IsValidTransition(current, models.WorkItemStateBlocked) {
		return models.WorkItemStateBlocked, false, true
	}
	return current, true, false
}

// beforeHumanReview reports whether s is an in-progress pipeline state that
// precedes the human gate, i.e. one from which a gated parent still has to
// be steered into human_review (or blocked when that edge is missing).
func beforeHumanReview(s models.WorkItemState) bool {
	switch s {
	case models.WorkItemStateIntake, models.WorkItemStatePRD, models.WorkItemStatePRDApproval,
		models.WorkItemStateResearch, models.WorkItemStatePlanning, models.WorkItemStateImplementation,
		models.WorkItemStateVerification, models.WorkItemStateReview, models.WorkItemStateAwaitingUser:
		return true
	}
	return false
}

// gatedPostReviewTarget builds the postReviewTarget for a gated parent from
// gatedTarget. prefix names the event ("PR merged", "review approved", "all
// child tasks complete").
func gatedPostReviewTarget(ctx context.Context, store workItemStore, workID uuid.UUID, normal models.WorkItemState, prefix, gateReason string) (postReviewTarget, error) {
	item, err := store.GetWorkItem(ctx, workID)
	if err != nil {
		return postReviewTarget{}, fmt.Errorf("get work item: %w", err)
	}
	toState, hold, blocked := gatedTarget(item.State, normal)
	t := postReviewTarget{
		ToState:    toState,
		Reason:     prefix + "; " + gateReason,
		Hold:       hold,
		Gated:      true,
		GateReason: gateReason,
	}
	switch {
	case hold:
	case blocked:
		t.Reason = fmt.Sprintf("%s; mandatory human review required but not reachable from state %s — blocked for operator attention: %s", prefix, item.State, gateReason)
		t.PublishBlockedPrompt = true
	case toState == models.WorkItemStateHumanReview:
		t.PublishHumanReviewPrompt = true
	}
	return t, nil
}

// resolvePRMergedTarget decides the parent's transition for pr.merged.
func resolvePRMergedTarget(ctx context.Context, store workItemStore, workID uuid.UUID, def *agentregistry.AgentDefinition) (postReviewTarget, error) {
	gated, gateReason := parentHumanReviewGate(ctx, store, workID)
	if !gated {
		toState := nextState(def, models.WorkItemStateReview)
		return postReviewTarget{
			ToState:        toState,
			Reason:         "PR merged",
			PublishQAReady: toState == models.WorkItemStateQA,
		}, nil
	}
	return gatedPostReviewTarget(ctx, store, workID, nextState(def, models.WorkItemStateReview), "PR merged", gateReason)
}

// resolveReviewApprovedTarget decides the parent's transition for
// review.complete with decision "approved". The ungated branches reproduce
// the pre-existing behaviour exactly (including which side effect each
// publishes); the gated branch applies gatedTarget regardless of the agent
// definition and of either human_review_required flag.
func resolveReviewApprovedTarget(ctx context.Context, store workItemStore, workID uuid.UUID, def *agentregistry.AgentDefinition, payloadHumanReviewRequired, configHumanReviewRequired bool) (postReviewTarget, error) {
	gated, gateReason := parentHumanReviewGate(ctx, store, workID)
	if gated {
		return gatedPostReviewTarget(ctx, store, workID, nextState(def, models.WorkItemStateReview), "review approved", gateReason)
	}
	toState := nextState(def, models.WorkItemStateReview)
	if payloadHumanReviewRequired || configHumanReviewRequired {
		if toState == models.WorkItemStatePassed {
			toState = models.WorkItemStateHumanReview
		}
		return postReviewTarget{
			ToState:                  toState,
			Reason:                   "review approved, awaiting human review",
			PublishHumanReviewPrompt: toState == models.WorkItemStateHumanReview,
		}, nil
	}
	return postReviewTarget{
		ToState:        toState,
		Reason:         "review approved, auto-merge allowed",
		PublishQAReady: toState == models.WorkItemStateQA,
	}, nil
}

// resolveParentCompletionTarget decides where a parent goes once all of its
// subtasks are complete. Ungated: the next enabled step after implementation
// (review, or qa/passed when the definition disables the steps between). A
// gated parent goes through gatedTarget: review is kept (the gate is applied
// when it leaves review), but qa, passed, and released are denied — the
// parent goes to human_review when the state machine allows it and is
// blocked with a visible reason when it does not.
func resolveParentCompletionTarget(ctx context.Context, store workItemStore, parentID uuid.UUID, def *agentregistry.AgentDefinition) (postReviewTarget, error) {
	normal := nextState(def, models.WorkItemStateImplementation)
	gated, gateReason := parentHumanReviewGate(ctx, store, parentID)
	if !gated {
		return postReviewTarget{ToState: normal, Reason: "all child tasks complete"}, nil
	}
	return gatedPostReviewTarget(ctx, store, parentID, normal, "all child tasks complete", gateReason)
}
