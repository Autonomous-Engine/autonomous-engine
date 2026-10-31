package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// R14: verification before the PR. When every subtask of a parent is
// implemented, the parent moves implementation → verification (instead of
// straight to review); the verification stage (review-agent) judges each
// subtask's verified head and records a verification_results row with the
// verification.complete it announces. The parent leaves verification when
// every subtask that passed in the current round has a result, on whichever
// event (implementation.complete or verification.complete) completes the
// set — the rows make it restart-safe and independent of the order the two
// topics are consumed in:
//
//	error (the stage could not set up)   → blocked (the stage blocked it; this
//	                                       only repeats the block if needed)
//	escalate, or not_run for a passed    → human_review, with the verdict as
//	  subtask with evidence                the reason and the prompt; no PR
//	request_changes                      → implementation with the defects as
//	                                       the fix subtask's feedback (the same
//	                                       circuit breaker as review)
//	approve / approve_pending_second_pass→ the pre-R14 completion target:
//	  (or not_run for a legacy-window      review, or the D2/D3 human gate
//	  message without evidence)            (resolveParentCompletionTarget)
//
// status-orchestrator is the only consumer that moves the parent;
// pr-coordinator, on the same verification.complete, opens a PR (or records
// the pending change) only on an approval and does nothing else otherwise.
//
// A "round" starts when the parent last re-entered implementation from a
// later state (changes requested, a QA fix, a retry from blocked): subtasks
// that passed before that, and results recorded before it, belong to an
// earlier round and are not judged again.

// verificationRound is what the recorded results say about a parent.
type verificationRound struct {
	// Running: a subtask is still being implemented; the parent stays
	// where it is.
	Running bool
	// Wait: every subtask is done but a passed subtask of this round has
	// no result yet; the parent moves to (or stays in) verification.
	Wait bool
	// Empty: no subtask passed in this round (all blocked/failed): nothing
	// to verify, the parent is left where it is.
	Empty bool
	// Decision is the aggregate (most conservative first: error, escalate,
	// request_changes, approve).
	Decision stage.Decision
	// Reason names each subtask's decision that decided it.
	Reason string
	// ChangesFrom are the results whose verdict requested changes (their
	// feedback is the fix subtask's).
	ChangesFrom []stage.Result
	Waiting     []string
}

// roundStart is the parent's current round start, the definition shared
// with pr-coordinator (workgraph.RoundStart: re-entry to implementation from
// a later state, or a codex re-plan; zero for the first round).
func roundStart(parentTransitions []*models.WorkItemTransition) time.Time {
	return workgraph.RoundStart(parentTransitions)
}

// lastPassed is when the subtask last transitioned to passed (zero: never
// recorded).
func lastPassed(ts []*models.WorkItemTransition) time.Time { return workgraph.LastPassed(ts) }

// decideVerificationRound aggregates the round's results. subtasks are the
// parent's subtask children (for an item without subtasks, the item itself
// in state passed); passedAt their last pass; results the newest result per
// subtask.
func decideVerificationRound(subtasks []*models.WorkItem, passedAt map[uuid.UUID]time.Time, results map[uuid.UUID]stage.Result, start time.Time) verificationRound {
	var r verificationRound
	var participants []*models.WorkItem
	for _, c := range subtasks {
		switch c.State {
		case models.WorkItemStateCancelled, models.WorkItemStateFailed, models.WorkItemStateBlocked:
			continue
		case models.WorkItemStatePassed:
			if at, ok := passedAt[c.ID]; ok && !at.IsZero() && !start.IsZero() && at.Before(start) {
				continue // passed in an earlier round
			}
			participants = append(participants, c)
		default:
			r.Running = true
			r.Waiting = append(r.Waiting, c.ID.String()+" still "+string(c.State))
		}
	}
	if r.Running {
		return r
	}
	if len(participants) == 0 {
		r.Empty = true
		return r
	}
	rank := map[stage.Decision]int{
		stage.DecisionError: 5, stage.DecisionEscalate: 4, stage.DecisionNotRun: 3,
		stage.DecisionRequestChanges: 2, stage.DecisionApprovePendingSecondPass: 1, stage.DecisionApprove: 0,
	}
	worst := -1
	var reasons []string
	for _, c := range participants {
		res, ok := results[c.ID]
		if !ok || (!start.IsZero() && res.CreatedAt.Before(start)) {
			r.Wait = true
			r.Waiting = append(r.Waiting, c.ID.String()+" not verified yet")
			continue
		}
		d := res.Decision
		if d == stage.DecisionNotRun && res.Legacy {
			// A legacy-window message without evidence keeps its
			// pre-evidence path: verified on pr.ready.
			d = stage.DecisionApprove
		}
		if d == stage.DecisionRequestChanges {
			r.ChangesFrom = append(r.ChangesFrom, res)
		}
		reasons = append(reasons, fmt.Sprintf("subtask %s: %s (%s)", c.ID, res.Decision, stage.OneLine(res.Reason, 300)))
		if rank[d] > worst {
			worst, r.Decision = rank[d], d
		}
	}
	if r.Wait {
		return r
	}
	r.Reason = strings.Join(reasons, "; ")
	return r
}

// handleVerificationComplete processes verification.complete: the parent is
// settled from the recorded results (settleVerification).
func handleVerificationComplete(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, msg *bus.Message) error {
	var m stage.Message
	if err := json.Unmarshal(msg.Value, &m); err != nil {
		log.Printf("status-orchestrator: verification.complete: unmarshal: %v", err)
		return nil
	}
	workID, err := uuid.Parse(m.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: verification.complete: parse work_id %q: %v", m.WorkID, err)
		return nil
	}
	log.Printf("status-orchestrator: verification.complete: work_id=%s task=%s head=%s decision=%s: %s", workID, m.TaskID, m.HeadSHA, m.Decision, m.Reason)
	return settleVerification(ctx, wg, kb, registry, workID)
}

// settleVerification moves the parent through verification (see the file
// comment). The walk to verification is unguarded; the final transition
// carries the delivery's guard. It returns nil whatever it decided (failures
// are logged, as every transition of this service is).
func settleVerification(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, parentID uuid.UUID) error {
	parent, err := wg.GetWorkItem(ctx, parentID)
	if err != nil {
		log.Printf("status-orchestrator: verification: get work item %s: %v", parentID, err)
		return nil
	}
	// parked: a prompt (pr-coordinator's gate, raised on the same
	// verification.complete) moved the parent verification → awaiting_user
	// before this settle ran. The decision is then recorded by rewriting
	// previous_state, so answering the prompt restores the parent to its
	// settled state (deferredSettle); handleUserPromptAnswered settles a
	// parent that comes back to verification unsettled.
	parked := false
	switch parent.State {
	case models.WorkItemStatePlanning, models.WorkItemStateImplementation, models.WorkItemStateVerification:
	case models.WorkItemStateAwaitingUser:
		if previousState(ctx, wg.DB(), parentID) != models.WorkItemStateVerification {
			log.Printf("status-orchestrator: verification: work_id=%s is awaiting_user outside verification — not settling verification", parentID)
			return nil
		}
		parked = true
	default:
		log.Printf("status-orchestrator: verification: work_id=%s is %s — not settling verification", parentID, parent.State)
		return nil
	}

	children, err := wg.GetChildren(ctx, parentID)
	if err != nil {
		log.Printf("status-orchestrator: verification: get children of %s: %v", parentID, err)
		return nil
	}
	var subtasks []*models.WorkItem
	for _, c := range children {
		if c.Type == models.WorkItemTypeSubtask {
			subtasks = append(subtasks, c)
		}
	}
	passedAt := map[uuid.UUID]time.Time{}
	if len(subtasks) == 0 {
		// An item implemented directly: it is its own (passed) subtask.
		self := *parent
		self.State = models.WorkItemStatePassed
		subtasks = []*models.WorkItem{&self}
	} else {
		for _, c := range subtasks {
			if c.State == models.WorkItemStatePassed {
				ts, terr := wg.GetTransitions(ctx, c.ID)
				if terr != nil {
					log.Printf("status-orchestrator: verification: transitions of subtask %s: %v", c.ID, terr)
				}
				passedAt[c.ID] = lastPassed(ts)
			}
		}
	}
	parentTransitions, err := wg.GetTransitions(ctx, parentID)
	if err != nil {
		log.Printf("status-orchestrator: verification: transitions of %s: %v", parentID, err)
	}
	results, err := stage.LatestByTask(ctx, wg.DB(), parentID)
	if err != nil {
		log.Printf("status-orchestrator: verification: %v — waiting", err)
		results = nil
	}
	round := decideVerificationRound(subtasks, passedAt, results, roundStart(parentTransitions))
	if round.Running || round.Empty {
		return nil
	}

	// Walk the parent to verification. Unguarded: re-reading the state
	// makes a redelivery skip a step already taken, and the guard belongs
	// to the final transition (or, when it waits, to the walk's last step).
	walk := func(final bool) bool {
		steps := []models.WorkItemState{}
		if parent.State == models.WorkItemStatePlanning {
			steps = append(steps, models.WorkItemStateImplementation)
		}
		if parent.State != models.WorkItemStateVerification {
			steps = append(steps, models.WorkItemStateVerification)
		}
		for i, to := range steps {
			c := idempotency.Unguarded(ctx)
			reason := "child tasks running"
			if to == models.WorkItemStateVerification {
				reason = "all child tasks implemented — independent verification before any PR"
				if !final && i == len(steps)-1 {
					c = ctx
				}
			}
			if err := transitionAndPublish(c, wg.DB(), wg, kb, parentID, to, reason); err != nil {
				log.Printf("status-orchestrator: verification: transition %s to %s: %v", parentID, to, err)
				return false
			}
		}
		return true
	}

	if round.Wait {
		log.Printf("status-orchestrator: verification: work_id=%s waiting (%s)", parentID, strings.Join(round.Waiting, ", "))
		if !parked {
			walk(false)
		}
		return nil
	}
	if !parked && !walk(true) {
		return nil
	}
	log.Printf("status-orchestrator: verification: work_id=%s settled %s (parked in awaiting_user=%t): %s", parentID, round.Decision, parked, round.Reason)

	// settle moves the parent out of verification, or — parked — records
	// the move on previous_state for when the prompt is answered.
	// Both cases go through settleOutOfVerification, which re-reads the
	// parent under its row lock: a prompt landing between the state read
	// above and the transaction is taken as parked there, never as an
	// awaiting_user→target transition that would strand previous_state and
	// awaiting_prompt_id.
	settle := func(to models.WorkItemState, reason string, effects ...txEffect) {
		err := settleOutOfVerification(ctx, wg.DB(), wg, kb, parentID, to, reason, effects...)
		if err != nil {
			log.Printf("status-orchestrator: verification: settle %s to %s (parked=%t): %v", parentID, to, parked, err)
		}
	}

	switch round.Decision {
	case stage.DecisionError:
		reason := "verification before the PR could not run: " + round.Reason
		settle(models.WorkItemStateBlocked, reason, blockedPromptEffect(reason), outcomeEffect(outcomes.Facts{}))
	case stage.DecisionEscalate, stage.DecisionNotRun:
		reason := "independent verifier escalated before the PR: " + round.Reason
		if round.Decision == stage.DecisionNotRun {
			reason = "independent verification did not run before the PR: " + round.Reason
		}
		settle(models.WorkItemStateHumanReview, reason, humanReviewPromptEffect("No PR was opened. "+reason), outcomeEffect(outcomes.Facts{}))
	case stage.DecisionRequestChanges:
		if parked {
			// A fix subtask must not start under a pending prompt; the
			// settle runs again when the prompt is answered and the parent
			// is back in verification (handleUserPromptAnswered).
			log.Printf("status-orchestrator: verification: work_id=%s requested changes while awaiting_user — dispatching the fix when the prompt is answered", parentID)
			return nil
		}
		feedback, seeds := verificationFeedback(ctx, wg, parentID, round)
		var extra map[string]interface{}
		if len(seeds) > 0 {
			// The implementation agent renders these as the fix task's
			// "## Where to edit" (the field its task message already reads).
			extra = map[string]interface{}{"line_seeds": seeds}
		}
		requestChanges(ctx, wg, kb, parentID, feedback, changesFromVerification, extra)
	default:
		parentDef := loadDefForWorkItem(ctx, registry, parentID)
		target, err := resolveParentCompletionTarget(ctx, wg, parentID, parentDef)
		if err != nil {
			log.Printf("status-orchestrator: verification: resolve parent target for %s: %v", parentID, err)
			return nil
		}
		if target.Gated {
			log.Printf("status-orchestrator: verification: parent %s gated → %s (hold=%t): %s", parentID, target.ToState, target.Hold, target.GateReason)
		}
		if target.Hold {
			return nil
		}
		target = reachableFromVerification(target)
		effects := []txEffect{outcomeEffect(outcomes.Facts{})}
		if target.PublishHumanReviewPrompt {
			prompt := "All child tasks complete and verified — " + target.GateReason
			if target.GateReason == "" {
				prompt = target.Reason
			}
			effects = append(effects, humanReviewPromptEffect(prompt))
		}
		if target.PublishBlockedPrompt {
			effects = append(effects, blockedPromptEffect(target.Reason))
		}
		settle(target.ToState, target.Reason, effects...)
	}
	return nil
}

// reachableFromVerification keeps a parent from sticking in verification
// (review minor 5): when the completion target has no edge from
// verification — a definition with review, human review and QA all
// disabled makes it passed, which verification may never reach (D1) — the
// parent goes to human_review with a prompt instead.
func reachableFromVerification(t postReviewTarget) postReviewTarget {
	if workgraph.IsValidTransition(models.WorkItemStateVerification, t.ToState) {
		return t
	}
	return postReviewTarget{
		ToState:                  models.WorkItemStateHumanReview,
		Reason:                   fmt.Sprintf("%s; the next step after verification (%s) is not reachable from verification — human review instead", t.Reason, t.ToState),
		PublishHumanReviewPrompt: true,
		Gated:                    t.Gated,
		GateReason:               t.GateReason,
	}
}

// previousState is the state awaiting_user will restore ("" when none or
// unreadable).
func previousState(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, id uuid.UUID) models.WorkItemState {
	var prev *string
	if err := q.QueryRow(ctx, `SELECT previous_state FROM work_items WHERE id = $1`, id).Scan(&prev); err != nil || prev == nil {
		return ""
	}
	return models.WorkItemState(*prev)
}

// insertParkedTransitionSQL records the settled verification→to move of a
// parked parent (the audit row; the state itself is awaiting_user).
const insertParkedTransitionSQL = `INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason, timestamp)
	VALUES ($1, $2, $3, $4, $5, $6, $7)`

// settleOutOfVerification is the single exit from verification (review
// re-review minor 2). Under the parent's row lock it decides what the
// current state allows: in verification, the transition to `to` with its
// effects; parked in awaiting_user from verification, the deferred record
// (previous_state = to); anything else (another consumer moved the parent
// meanwhile), nothing. state.changed is published only for a transition.
func settleOutOfVerification(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, parentID uuid.UUID, to models.WorkItemState, reason string, effects ...txEffect) error {
	if !workgraph.IsValidTransition(models.WorkItemStateVerification, to) {
		return fmt.Errorf("%w: verification → %s", workgraph.ErrInvalidTransition, to)
	}
	item, err := wg.GetWorkItem(ctx, parentID)
	if err != nil {
		return fmt.Errorf("get work item: %w", err)
	}
	transitioned := false
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		transitioned = false
		var state string
		var prev *string
		if err := tx.QueryRow(ctx, lockWorkItemStateSQL, parentID).Scan(&state, &prev); err != nil {
			return fmt.Errorf("lock work item: %w", err)
		}
		switch {
		case models.WorkItemState(state) == models.WorkItemStateVerification:
			if _, err := wg.TransitionStateTx(ctx, tx, parentID, to, agentID, reason); err != nil {
				return fmt.Errorf("transition state: %w", err)
			}
			transitioned = true
			for _, effect := range effects {
				if err := effect(ctx, tx, item); err != nil {
					return err
				}
			}
			return nil
		case models.WorkItemState(state) == models.WorkItemStateAwaitingUser && prev != nil && models.WorkItemState(*prev) == models.WorkItemStateVerification:
			return deferredSettleTx(ctx, tx, item, parentID, to, reason, effects...)
		default:
			log.Printf("status-orchestrator: verification: work_id=%s is %s (previous %v), no longer in or parked from verification — nothing recorded", parentID, state, prev)
			return nil
		}
	})
	if err != nil {
		return err
	}
	if transitioned {
		publishStateChanged(ctx, kb, parentID, models.WorkItemStateVerification, to, reason)
	}
	return nil
}

// deferredSettleTx records the settle of a parent parked in awaiting_user
// (previous_state = verification), in the caller's transaction after its
// row lock showed it parked: previous_state becomes to, the verification→to
// transition row is written, and effects (prompts, outcome) run. When the
// prompt is answered, TransitionFromAwaitingUser restores the parent to to.
func deferredSettleTx(ctx context.Context, tx pgx.Tx, item *models.WorkItem, parentID uuid.UUID, to models.WorkItemState, reason string, effects ...txEffect) error {
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE work_items SET previous_state = $2, updated_at = $3 WHERE id = $1`, parentID, string(to), now); err != nil {
		return fmt.Errorf("rewrite previous_state: %w", err)
	}
	if _, err := tx.Exec(ctx, insertParkedTransitionSQL, uuid.New(), parentID, models.WorkItemStateVerification, to, agentID, reason+" (recorded while awaiting a prompt answer)", now); err != nil {
		return fmt.Errorf("insert transition: %w", err)
	}
	for _, effect := range effects {
		if err := effect(ctx, tx, item); err != nil {
			return err
		}
	}
	return nil
}

// verificationFeedback is the fix subtask's feedback (R16 G3): the fix
// brief rendered from each requesting verdict — per problem the criterion
// id and text, the defect, the file:line references, and the original
// subtask's line seeds on those files (verifier.RenderFixBrief, at most
// verifier.FixBriefMaxChars) — and those seeds for the fix task message's
// line_seeds. A result whose verdict cannot be read contributes its reason.
func verificationFeedback(ctx context.Context, wg *workgraph.WorkGraph, parentID uuid.UUID, round verificationRound) (string, []verifier.LineSeed) {
	var srcs []verifier.FixSource
	var missing []string
	for _, r := range round.ChangesFrom {
		if r.VerdictArtifactID != nil {
			c, err := stage.LoadArtifactContent(ctx, wg.DB(), *r.VerdictArtifactID)
			if err == nil {
				srcs = append(srcs, verifier.FixSource{
					TaskID:   r.TaskID.String(),
					Verdict:  c.Verdict,
					Criteria: criterionTexts(ctx, wg, parentID, r.TaskID),
					Seeds:    planLineSeeds(ctx, wg.DB(), parentID, r.TaskID),
				})
				continue
			}
			log.Printf("status-orchestrator: verification: %v", err)
		}
		missing = append(missing, fmt.Sprintf("Independent verifier requested changes on subtask %s: %s", r.TaskID, stage.OneLine(r.Reason, 600)))
	}
	if len(srcs) == 0 && len(missing) == 0 {
		return capBytes("Independent verifier requested changes: "+round.Reason, verifier.FixBriefMaxChars), nil
	}
	// The reasons of the subtasks whose verdict could not be read get their
	// room first (review minor 2): at most half the budget when verdicts
	// were read too; those that do not fit are counted, never cut silently.
	budget := verifier.FixBriefMaxChars
	if len(srcs) > 0 {
		budget /= 2
	}
	unread := unreadSection(missing, budget)
	if len(srcs) == 0 {
		return unread, nil
	}
	sep := ""
	if unread != "" {
		sep = "\n\n"
	}
	brief, seeds := verifier.RenderFixBriefWithin(srcs, verifier.FixBriefMaxChars-len(unread)-len(sep))
	return brief + sep + unread, seeds
}

// unreadSection joins the unread subtasks' reasons within max bytes,
// counting those that do not fit.
func unreadSection(missing []string, max int) string {
	if len(missing) == 0 {
		return ""
	}
	const moreReserve = 100
	var b strings.Builder
	for i, m := range missing {
		part := m
		if i > 0 {
			part = "\n\n" + m
		}
		if b.Len()+len(part)+moreReserve > max {
			fmt.Fprintf(&b, "\n\n… %d more subtask(s) requested changes whose verdict could not be read; see verification_results.", len(missing)-i)
			break
		}
		b.WriteString(part)
	}
	return capBytes(strings.TrimLeft(b.String(), "\n"), max)
}

// criterionTexts maps the criterion ids the subtask was verified against to
// their text ({} when unreadable: the brief then names ids only).
func criterionTexts(ctx context.Context, wg *workgraph.WorkGraph, parentID, taskID uuid.UUID) map[string]string {
	out := map[string]string{}
	req, err := verifier.LoadRequirementsForTask(ctx, wg.DB(), parentID, taskID)
	if err != nil {
		log.Printf("status-orchestrator: verification: criteria for the fix brief of %s: %v", taskID, err)
	}
	for _, c := range req.Criteria {
		out[c.ID] = c.Text
	}
	return out
}

// planLineSeeds are the line seeds (plan_tasks.message.line_seeds, R10 W5)
// of the subtask's plan task first, then of the parent's other plan tasks
// (a fix subtask has no plan task of its own); RenderFixBrief keeps those on
// the files the verdict references.
func planLineSeeds(ctx context.Context, q verifier.Querier, parentID, taskID uuid.UUID) []verifier.LineSeed {
	rows, err := q.Query(ctx, `SELECT id, message FROM plan_tasks WHERE parent_work_id = $1 OR id = $2
		ORDER BY (id = $2) DESC, created_at, id`, parentID, taskID)
	if err != nil {
		log.Printf("status-orchestrator: verification: line seeds for %s: %v", taskID, err)
		return nil
	}
	defer rows.Close()
	var out []verifier.LineSeed
	for rows.Next() {
		var id uuid.UUID
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			log.Printf("status-orchestrator: verification: line seeds for %s: %v", taskID, err)
			return out
		}
		var m struct {
			LineSeeds []verifier.LineSeed `json:"line_seeds"`
		}
		if json.Unmarshal(raw, &m) == nil {
			out = append(out, m.LineSeeds...)
		}
	}
	return out
}

// capBytes cuts s to at most n bytes on a rune boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
