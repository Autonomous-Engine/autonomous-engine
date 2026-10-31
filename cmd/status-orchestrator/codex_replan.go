package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Codex coherence Part 1: re-plan in-flight work when the codex changes.
//
//  1. codex.indexed (own consumer group, guarded by processed_events) only
//     schedules a sweep of the event's product and branch in
//     codex_replan_pending, due CODEX_REPLAN_DEBOUNCE after the last event
//     (codex.ScheduleSweepTx). Nothing is held in memory: a restart during
//     the quiet window loses nothing.
//  2. A ticker claims due sweeps (FOR UPDATE SKIP LOCKED and a lease, so
//     replicas share the work and a crashed sweep is retried) and judges
//     every pinned parent of the product and branch against the versions
//     after its pin, or after the last version a sweep already acted on for
//     it (codex.StalenessFor: its pinned modules, or a plan-relevant
//     section).
//  3. A stale parent in planning or implementation without an open PR or
//     pending change is re-planned in one transaction (replanTx): its
//     in-progress subtasks are blocked (codex_changed), it goes back to
//     planning with the change as the reason, the activity entry is written
//     and technical-brief.ready is enqueued with its existing brief, so the
//     planner plans again against the current codex. Research is not re-run.
//     The brief carries the replanned event's id; the planner commits a plan
//     only from the latest re-plan's brief, or from an ordinary brief
//     produced after the last re-plan (codex.ReplanBriefAllowedTx), so a
//     planning run in flight, a redelivered old brief or an earlier re-plan
//     cannot add a second plan. The history is re-read under the parent's
//     row lock, so a double sweep re-plans once and counts the cap once.
//  4. Loop guard, budget and verified work: after CODEX_REPLAN_MAX automatic
//     re-plans, when the item has less than CODEX_REPLAN_MIN_TOKENS of its
//     token budget left, or when a subtask of its current round is already
//     verified (an automatic re-plan never discards verified work), the
//     operator is asked instead (Re-plan / Continue as is / Cancel;
//     handleCodexPromptAnswer). The budget is never raised. pr-coordinator
//     holds PRs while that escalation is pending; "Continue" re-enqueues the
//     latest verification.complete; "Re-plan" is refused (noted, the operator
//     told) if a PR, a pending change or newly verified work appeared
//     meanwhile.
//  5. Anything else not terminal (verification, review, human_review, qa,
//     an open PR, blocked, awaiting_user) is left alone and the change is
//     recorded on it once ("codex changed since planning").

const (
	// codexReplanConsumerGroup is codex.indexed's own consumer group, and
	// codexReplanConsumer its processed_events name.
	codexReplanConsumerGroup = "status-orchestrator-codex-replan"
	codexReplanConsumer      = "status-orchestrator-codex-replan"
	// codexChangedPrefix starts every re-plan transition reason (and the
	// reason of the subtasks it blocks).
	codexChangedPrefix = workgraph.CodexChangedPrefix
)

// replanConfig is the re-plan's tuning (replanConfigFromEnv).
type replanConfig struct {
	// Debounce is the quiet window after the last codex.indexed of a
	// product and branch (CODEX_REPLAN_DEBOUNCE, default 10m).
	Debounce time.Duration
	// Max is the number of automatic re-plans per item (CODEX_REPLAN_MAX,
	// default 2); the next staleness escalates.
	Max int
	// Lease is how long a claimed sweep is hidden from other replicas.
	Lease time.Duration
	// Tick is how often due sweeps are looked for (CODEX_REPLAN_TICK,
	// default 30s).
	Tick time.Duration
	// TokenBudget is the work item's token budget (WORK_ITEM_TOKEN_BUDGET,
	// tools.TokenBudget); MinPlanTokens what must be left of it for a
	// re-plan (CODEX_REPLAN_MIN_TOKENS, default a tenth of the budget).
	TokenBudget   int64
	MinPlanTokens int64
}

func replanConfigFromEnv() replanConfig {
	c := replanConfig{Debounce: 10 * time.Minute, Max: 2, Lease: 5 * time.Minute, Tick: 30 * time.Second, TokenBudget: tools.TokenBudget()}
	if d, err := time.ParseDuration(os.Getenv("CODEX_REPLAN_DEBOUNCE")); err == nil && d >= 0 {
		c.Debounce = d
	}
	if n, err := strconv.Atoi(os.Getenv("CODEX_REPLAN_MAX")); err == nil && n >= 0 {
		c.Max = n
	}
	if d, err := time.ParseDuration(os.Getenv("CODEX_REPLAN_TICK")); err == nil && d > 0 {
		c.Tick = d
	}
	c.MinPlanTokens = c.TokenBudget / 10
	if n, err := strconv.ParseInt(os.Getenv("CODEX_REPLAN_MIN_TOKENS"), 10, 64); err == nil && n >= 0 {
		c.MinPlanTokens = n
	}
	return c
}

// replanAction is what a sweep does with one stale parent.
type replanAction int

const (
	actionNone replanAction = iota
	actionNote
	actionReplan
	actionEscalateCap
	actionEscalateBudget
	actionEscalateVerified
)

func (a replanAction) String() string {
	return [...]string{"none", "note", "replan", "escalate(cap)", "escalate(budget)", "escalate(verified)"}[a]
}

// Causes recorded on escalations and notes (work_item_codex_events.cause).
const (
	causeCap          = "cap"
	causeBudget       = "budget"
	causeNoBrief      = "no_brief"
	causeVerifiedWork = "verified_work"
	causeRefused      = "refused"
)

// replannable is where a codex change sends a parent back to planning.
func replannable(s models.WorkItemState) bool {
	return s == models.WorkItemStatePlanning || s == models.WorkItemStateImplementation
}

func terminalState(s models.WorkItemState) bool {
	switch s {
	case models.WorkItemStatePassed, models.WorkItemStateReleased, models.WorkItemStateCancelled, models.WorkItemStateRejected,
		models.WorkItemStateFailed:
		return true
	}
	return false
}

// nonTerminalStates are the states a sweep lists pinned items in.
var nonTerminalStates = []models.WorkItemState{
	models.WorkItemStateIntake, models.WorkItemStatePRD, models.WorkItemStatePRDApproval, models.WorkItemStateResearch,
	models.WorkItemStatePlanning, models.WorkItemStateImplementation, models.WorkItemStateVerification,
	models.WorkItemStateReview, models.WorkItemStateHumanReview, models.WorkItemStateQA,
	models.WorkItemStateAwaitingUser, models.WorkItemStateBlocked,
}

// decideCodexReplan decides for a stale parent: only planning and
// implementation without an open PR or pending change are re-planned, and
// only when no subtask of the current round holds verified work (review
// ruling: an automatic re-plan never discards verified work) and within the
// cap and the budget; every other non-terminal state is noted.
func decideCodexReplan(state models.WorkItemState, openPR, verifiedWork bool, automaticReplans, max int, budgetLeft bool) replanAction {
	switch {
	case terminalState(state):
		return actionNone
	case !replannable(state), openPR:
		return actionNote
	case verifiedWork:
		return actionEscalateVerified
	case automaticReplans >= max:
		return actionEscalateCap
	case !budgetLeft:
		return actionEscalateBudget
	}
	return actionReplan
}

// staleChange is one parent's codex change: versions From (exclusive) to To.
type staleChange struct {
	ProductID uuid.UUID
	Branch    string
	From, To  int
	codex.Staleness
}

func (c staleChange) describe() string { return c.Staleness.Describe(c.From, c.To) }

// codexReplanner is the sweep and its dependencies.
type codexReplanner struct {
	pool     *pgxpool.Pool
	wg       *workgraph.WorkGraph
	kb       bus.MessageBus
	registry *agentregistry.Registry
	cfg      replanConfig
	cx       *codex.Codex
	pins     *codex.PinStore
}

func newCodexReplanner(pool *pgxpool.Pool, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, cfg replanConfig) *codexReplanner {
	return &codexReplanner{pool: pool, wg: wg, kb: kb, registry: registry, cfg: cfg, cx: codex.New(pool), pins: codex.NewPinStore(pool)}
}

// handleCodexIndexed schedules (or pushes back) the sweep of the event's
// product and branch, in a transaction carrying the delivery's guard row.
func (r *codexReplanner) handleCodexIndexed(ctx context.Context, msg *bus.Message) error {
	ev, err := codex.DecodeIndexedEvent(msg.Value)
	if err != nil {
		log.Printf("status-orchestrator: codex.indexed: %v — skipping", err)
		return nil
	}
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, r.pool), func(tx pgx.Tx) error {
		return codex.ScheduleSweepTx(ctx, tx, ev, r.cfg.Debounce)
	})
	if idempotency.IsDuplicate(err) {
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("status-orchestrator: codex.indexed product=%s branch=%s v%d %v: re-plan sweep due in %s", ev.ProductID, ev.Branch, ev.ProductVersion, ev.SectionsChanged, r.cfg.Debounce)
	return nil
}

// run sweeps due rows every Tick until ctx ends.
func (r *codexReplanner) run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Tick)
	defer t.Stop()
	for {
		if _, err := r.processDue(ctx); err != nil && ctx.Err() == nil {
			log.Printf("status-orchestrator: codex re-plan sweep: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// processDue claims and sweeps every due row (only the products in only,
// when given) and returns how many it swept. A failed sweep keeps its row,
// which is claimable again when its lease expires; the first error is
// returned after the others were tried.
func (r *codexReplanner) processDue(ctx context.Context, only ...uuid.UUID) (int, error) {
	n := 0
	var first error
	for ctx.Err() == nil {
		s, err := codex.ClaimDueSweep(ctx, r.pool, r.cfg.Lease, only...)
		if err != nil {
			return n, err
		}
		if s == nil {
			break
		}
		if err := r.sweep(ctx, *s); err != nil {
			log.Printf("status-orchestrator: codex re-plan sweep %s/%s failed (retried after %s): %v", s.ProductID, s.Branch, r.cfg.Lease, err)
			if first == nil {
				first = err
			}
			continue
		}
		if _, err := codex.CompleteSweep(ctx, r.pool, *s); err != nil {
			log.Printf("status-orchestrator: complete codex re-plan sweep %s/%s: %v", s.ProductID, s.Branch, err)
		}
		n++
	}
	return n, first
}

// sweep judges every pinned parent of the sweep's product and branch.
func (r *codexReplanner) sweep(ctx context.Context, s codex.PendingSweep) error {
	current, err := r.cx.GetCurrentVersion(ctx, s.ProductID, s.Branch)
	if err != nil {
		return err
	}
	items, err := r.pins.ListPinned(ctx, s.ProductID, s.Branch, nonTerminalStates)
	if err != nil {
		return err
	}
	log.Printf("status-orchestrator: codex re-plan sweep product=%s branch=%s at v%d (%d event(s)): %d pinned item(s)", s.ProductID, s.Branch, current, s.EventCount, len(items))
	var errs []error
	for _, it := range items {
		if it.ParentID != nil {
			continue // subtasks follow their parent
		}
		if err := r.evaluate(ctx, it, current); err != nil {
			log.Printf("status-orchestrator: codex re-plan of %s: %v", it.WorkItemID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// evaluate decides and acts for one pinned parent.
func (r *codexReplanner) evaluate(ctx context.Context, it codex.PinnedItem, current int) error {
	if terminalState(it.State) {
		return nil
	}
	h, err := codex.LoadReplanHistory(ctx, r.pool, it.WorkItemID)
	if err != nil {
		return err
	}
	from := max(it.ProductVersion, h.AcknowledgedVersion)
	if from >= current {
		return nil
	}
	changed, err := r.cx.GetChangedSectionsBetween(ctx, it.ProductID, it.Branch, from+1, current)
	if err != nil {
		return err
	}
	st := codex.StalenessFor(it.Modules, changed)
	if !st.Stale() {
		return nil
	}
	ch := staleChange{ProductID: it.ProductID, Branch: it.Branch, From: from, To: current, Staleness: st}

	openPR, verifiedWork, budgetLeft := false, false, true
	var verifiedWhy string
	var spent int64
	if replannable(it.State) {
		if openPR, err = hasOpenPROrPendingChange(ctx, r.pool, it.WorkItemID); err != nil {
			return err
		}
		if verifiedWork, verifiedWhy, err = verifiedWorkInRound(ctx, r.wg, it.WorkItemID); err != nil {
			return err
		}
		if spent, err = tokensSpent(ctx, r.pool, it.WorkItemID); err != nil {
			return err
		}
		budgetLeft = r.cfg.TokenBudget-spent >= r.cfg.MinPlanTokens
	}
	action := decideCodexReplan(it.State, openPR, verifiedWork, h.AutomaticReplans, r.cfg.Max, budgetLeft)
	log.Printf("status-orchestrator: codex re-plan: work_id=%s state=%s %s → %s", it.WorkItemID, it.State, ch.describe(), action)
	switch action {
	case actionNote:
		return r.note(ctx, it, ch)
	case actionReplan:
		err := r.replan(ctx, it.WorkItemID, ch)
		switch {
		case errors.Is(err, errNoTechnicalBrief):
			return r.escalate(ctx, it, ch, fmt.Sprintf("The codex for %s changed since this item was planned (%s), but it has no technical brief to re-plan from. Re-plan once a brief exists, continue as is with the current plan, or cancel.", it.Branch, ch.describe()), causeNoBrief, false)
		case errors.Is(err, errReplanCapReached):
			// Another sweep re-planned it meanwhile (counted once).
			h2, herr := codex.LoadReplanHistory(ctx, r.pool, it.WorkItemID)
			if herr != nil {
				return herr
			}
			return r.escalate(ctx, it, ch, capQuestion(it.Branch, ch, h2.AutomaticReplans, r.cfg.Max), causeCap, false)
		case errors.Is(err, errAlreadyActedOn):
			log.Printf("status-orchestrator: codex re-plan of %s: %s was already acted on by another sweep", it.WorkItemID, ch.describe())
			return nil
		case errors.Is(err, workgraph.ErrInvalidTransition):
			// It left planning/implementation since it was listed; the
			// next codex change judges it again.
			log.Printf("status-orchestrator: codex re-plan of %s skipped: %v", it.WorkItemID, err)
			return nil
		}
		return err
	case actionEscalateVerified:
		return r.escalate(ctx, it, ch, fmt.Sprintf("The codex for %s changed since this item was planned (%s), but part of its current plan is already verified (%s); an automatic re-plan never discards verified work. Re-plan anyway (the verified work is set aside), continue as is with the current plan, or cancel.",
			it.Branch, ch.describe(), verifiedWhy), causeVerifiedWork, true)
	case actionEscalateCap:
		return r.escalate(ctx, it, ch, capQuestion(it.Branch, ch, h.AutomaticReplans, r.cfg.Max), causeCap, false)
	case actionEscalateBudget:
		left := r.cfg.TokenBudget - spent
		if left < 0 {
			left = 0
		}
		return r.escalate(ctx, it, ch, fmt.Sprintf("The codex for %s changed since this item was planned (%s), but a re-plan needs at least %d tokens and only %d of its %d-token budget are left (budgets are never raised). Re-plan anyway, continue as is with the current plan, or cancel.",
			it.Branch, ch.describe(), r.cfg.MinPlanTokens, left, r.cfg.TokenBudget), causeBudget, false)
	}
	return nil
}

func capQuestion(branch string, ch staleChange, replans, max int) string {
	return fmt.Sprintf("The codex for %s changed again since this item was planned (%s), and it has already been re-planned automatically %d time(s) (CODEX_REPLAN_MAX=%d). Re-plan against the current codex, continue as is with the current plan, or cancel.",
		branch, ch.describe(), replans, max)
}

// verifiedWorkInRound reports whether a subtask of the parent's current
// round holds verified work: it is in verification or review, or it passed
// and its latest verification result approves.
func verifiedWorkInRound(ctx context.Context, wg *workgraph.WorkGraph, parentID uuid.UUID) (bool, string, error) {
	children, err := wg.GetChildren(ctx, parentID)
	if err != nil {
		return false, "", err
	}
	pts, err := wg.GetTransitions(ctx, parentID)
	if err != nil {
		return false, "", err
	}
	round := workgraph.NewRound(pts)
	results, err := stage.LatestByTask(ctx, wg.DB(), parentID)
	if err != nil {
		return false, "", err
	}
	for _, c := range children {
		if c.Type != models.WorkItemTypeSubtask {
			continue
		}
		cts, err := wg.GetTransitions(ctx, c.ID)
		if err != nil {
			return false, "", err
		}
		if !round.InRound(workgraph.RoundChild{State: c.State, CreatedAt: c.CreatedAt, PassedAt: workgraph.LastPassed(cts), Superseded: workgraph.Superseded(c.State, cts)}) {
			continue
		}
		switch c.State {
		case models.WorkItemStateVerification, models.WorkItemStateReview:
			return true, fmt.Sprintf("subtask %s is in %s", c.ID, c.State), nil
		case models.WorkItemStatePassed:
			if res, ok := results[c.ID]; ok && res.Decision.Approves() && (round.Start.IsZero() || !res.CreatedAt.Before(round.Start)) {
				return true, fmt.Sprintf("subtask %s was approved by the verifier", c.ID), nil
			}
		}
	}
	return false, "", nil
}

// errAlreadyActedOn: another sweep acted on this change first (a lease that
// expired mid-sweep); nothing was written.
var errAlreadyActedOn = errors.New("codex change already acted on")

// errReplanCapReached: under the row lock, the item turned out to have
// reached CODEX_REPLAN_MAX (another sweep re-planned it meanwhile).
var errReplanCapReached = errors.New("automatic re-plan cap reached")

// lockFreshTx locks the item's row and re-reads its history under the lock,
// so two sweeps of the same change (one whose lease expired) act on it
// once: errAlreadyActedOn when an event already covers ch.To.
func lockFreshTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, ch staleChange) (codex.ReplanHistory, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE id = $1 FOR UPDATE`, id); err != nil {
		return codex.ReplanHistory{}, fmt.Errorf("lock %s: %w", id, err)
	}
	h, err := codex.LoadReplanHistory(ctx, tx, id)
	if err != nil {
		return h, err
	}
	if h.AcknowledgedVersion >= ch.To {
		return h, errAlreadyActedOn
	}
	return h, nil
}

// note records the change on an item that is not re-planned.
func (r *codexReplanner) note(ctx context.Context, it codex.PinnedItem, ch staleChange) error {
	err := workgraph.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := lockFreshTx(ctx, tx, it.WorkItemID, ch); err != nil {
			return err
		}
		_, err := codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
			WorkItemID: it.WorkItemID, ProductID: ch.ProductID, Branch: ch.Branch, Kind: codex.EventNoted, Automatic: true,
			FromVersion: ch.From, ToVersion: ch.To, Modules: ch.Modules, Sections: ch.Sections, ItemState: string(it.State),
			Reason: fmt.Sprintf("codex changed since planning: %s; not re-planned (%s)", ch.describe(), it.State),
		})
		return err
	})
	if errors.Is(err, errAlreadyActedOn) {
		return nil
	}
	return err
}

// escalate asks the operator (Re-plan / Continue as is / Cancel) instead of
// re-planning; withNote also records the "codex changed since planning"
// note. The prompt's user_prompt.requested parks the item in awaiting_user
// like every prompt; its subtasks are not stopped (pr-coordinator holds
// their PRs until the answer).
func (r *codexReplanner) escalate(ctx context.Context, it codex.PinnedItem, ch staleChange, question, cause string, withNote bool) error {
	item, err := r.wg.GetWorkItem(ctx, it.WorkItemID)
	if err != nil {
		return err
	}
	err = workgraph.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := lockFreshTx(ctx, tx, it.WorkItemID, ch); err != nil {
			return err
		}
		if withNote {
			if _, err := codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
				WorkItemID: it.WorkItemID, ProductID: ch.ProductID, Branch: ch.Branch, Kind: codex.EventNoted, Automatic: true,
				FromVersion: ch.From, ToVersion: ch.To, Modules: ch.Modules, Sections: ch.Sections, ItemState: string(it.State),
				Reason: fmt.Sprintf("codex changed since planning: %s; not re-planned automatically (%s)", ch.describe(), cause), Cause: cause,
			}); err != nil {
				return err
			}
		}
		return raiseCodexEscalationTx(ctx, tx, item, ch, string(it.State), question, cause, true)
	})
	if errors.Is(err, errAlreadyActedOn) {
		return nil
	}
	return err
}

// raiseCodexEscalationTx raises the codex:replan prompt (with Re-plan when
// offerReplan) and records the escalated event that answers are matched by.
func raiseCodexEscalationTx(ctx context.Context, tx pgx.Tx, item *models.WorkItem, ch staleChange, state, question, cause string, offerReplan bool) error {
	p := codexReplanPrompt(item, ch, question)
	if !offerReplan {
		p.Actions = p.Actions[1:]
	}
	if err := interactions.NewPublisher(nil).PublishTx(ctx, tx, aggregateFor(item), p); err != nil {
		return fmt.Errorf("raise codex re-plan escalation: %w", err)
	}
	_, err := codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
		WorkItemID: item.ID, ProductID: ch.ProductID, Branch: ch.Branch, Kind: codex.EventEscalated, Automatic: true,
		FromVersion: ch.From, ToVersion: ch.To, Modules: ch.Modules, Sections: ch.Sections, ItemState: state,
		Reason: question, Cause: cause, PromptID: &p.ID,
	})
	return err
}

func codexReplanPrompt(item *models.WorkItem, ch staleChange, question string) *interactions.Prompt {
	return &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  item.ID,
		ProductID:   ch.ProductID,
		AskingAgent: agentID,
		Trigger:     interactions.TriggerCodexReplan,
		PromptType:  interactions.PromptTypeEscalation,
		Headline:    fmt.Sprintf("Codex changed since planning: %s", item.Title),
		Question:    question,
		ContextLink: fmt.Sprintf("/work-items/%s", item.ID),
		ContextPayload: map[string]any{"codex_change": map[string]any{
			"branch": ch.Branch, "from_version": ch.From, "to_version": ch.To,
			"modules": orEmpty(ch.Modules), "sections": orEmpty(ch.Sections),
		}},
		Actions: []interactions.Action{
			{ID: "replan", Label: "Re-plan", Semantic: interactions.SemanticRetry},
			{ID: "continue", Label: "Continue as is", Semantic: interactions.SemanticContinue},
			{ID: "cancel", Label: "Cancel", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// replan runs replanTx in a transaction of its own and publishes the
// state.changed telemetry after the commit.
func (r *codexReplanner) replan(ctx context.Context, parentID uuid.UUID, ch staleChange) error {
	parent, err := r.wg.GetWorkItem(ctx, parentID)
	if err != nil {
		return err
	}
	var res replanResult
	err = workgraph.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		res, err = replanTx(ctx, tx, r.wg, parent, ch, true, r.cfg.Max)
		return err
	})
	if err != nil {
		return err
	}
	res.publish(ctx, r.kb)
	return nil
}

// errNoTechnicalBrief: the parent has no brief to plan from again.
var errNoTechnicalBrief = errors.New("no technical brief to re-plan from")

// stateChange is a committed transition to announce on state.changed.
type stateChange struct {
	id       uuid.UUID
	from, to models.WorkItemState
	reason   string
}

type replanResult struct{ changes []stateChange }

func (r replanResult) publish(ctx context.Context, kb bus.MessageBus) {
	for _, c := range r.changes {
		publishStateChanged(ctx, kb, c.id, c.from, c.to, c.reason)
	}
}

// technicalBriefReplanMessage is the technical-brief.ready a re-plan
// enqueues: the planner's input as research-coordinator sends it
// (work_id, product_id, brief_content, timestamp, criteria), from the
// parent's newest technical brief, marked as a re-plan.
type technicalBriefReplanMessage struct {
	WorkID       string          `json:"work_id"`
	ProductID    string          `json:"product_id"`
	BriefContent string          `json:"brief_content"`
	Timestamp    string          `json:"timestamp"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
	Replan       bool            `json:"replan"`
	// ReplanEventID is the replanned event that emitted the brief; the
	// planner commits its plan only while it is the latest re-plan.
	ReplanEventID string `json:"replan_event_id"`
	Reason        string `json:"reason"`
}

// briefContentTx reads the parent's newest technical brief (the planner's
// own query) as the message's prose and criteria.
func briefContentTx(ctx context.Context, tx pgx.Tx, parentID uuid.UUID) (prose string, criteria json.RawMessage, err error) {
	var content []byte
	err = tx.QueryRow(ctx, `
		SELECT content FROM work_item_artifacts
		 WHERE work_item_id = $1 AND artifact_type = 'technical_brief'
		   AND COALESCE(content->>'name', '') <> 'plan-validation'
		 ORDER BY created_at DESC LIMIT 1`, parentID).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, errNoTechnicalBrief
	}
	if err != nil {
		return "", nil, fmt.Errorf("read technical brief: %w", err)
	}
	var b struct {
		Narrative string          `json:"narrative"`
		Brief     string          `json:"brief"`
		Criteria  json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(content, &b); err != nil {
		return "", nil, fmt.Errorf("decode technical brief: %w", err)
	}
	prose = b.Narrative
	if strings.TrimSpace(prose) == "" {
		prose = b.Brief
	}
	if string(b.Criteria) == "null" {
		b.Criteria = nil
	}
	return prose, b.Criteria, nil
}

// codexReplanReason is the parent's transition reason.
func codexReplanReason(ch staleChange, automatic bool) string {
	reason := codexChangedPrefix + ": " + ch.describe()
	if !automatic {
		reason += ", re-plan chosen by the operator"
	}
	return reason
}

// replanTx sends parent back to planning in tx: parent → planning (from
// planning or implementation, under its row lock), every child still in
// implementation → blocked with its plan task closed, the replanned
// activity entry, and technical-brief.ready with the existing brief. Any
// error rolls all of it back (errNoTechnicalBrief when there is no brief;
// an invalid transition when the parent left planning/implementation).
//
// An automatic re-plan (maxAutomatic >= 0) re-reads the item's history
// under the parent's row lock (which the transition takes first): a change
// another sweep already acted on is errAlreadyActedOn, and a cap another
// sweep reached meanwhile errReplanCapReached, so a double sweep (a lease
// that expired mid-sweep) re-plans once and counts once. An operator's
// re-plan passes -1.
func replanTx(ctx context.Context, tx pgx.Tx, wg workItemStore, parent *models.WorkItem, ch staleChange, automatic bool, maxAutomatic int) (replanResult, error) {
	var res replanResult
	reason := codexReplanReason(ch, automatic)
	from, err := wg.TransitionStateTx(ctx, tx, parent.ID, models.WorkItemStatePlanning, agentID, reason)
	if err != nil {
		return res, fmt.Errorf("re-plan %s: %w", parent.ID, err)
	}
	if automatic && maxAutomatic >= 0 {
		h, err := codex.LoadReplanHistory(ctx, tx, parent.ID)
		if err != nil {
			return res, err
		}
		if h.AcknowledgedVersion >= ch.To {
			return res, errAlreadyActedOn
		}
		if h.AutomaticReplans >= maxAutomatic {
			return res, errReplanCapReached
		}
	}
	res.changes = append(res.changes, stateChange{parent.ID, from, models.WorkItemStatePlanning, reason})
	prose, criteria, err := briefContentTx(ctx, tx, parent.ID)
	if err != nil {
		return res, err
	}

	rows, err := tx.Query(ctx, `SELECT id FROM work_items WHERE parent_id = $1 AND state = 'implementation' ORDER BY id FOR UPDATE`, parent.ID)
	if err != nil {
		return res, fmt.Errorf("lock in-progress subtasks: %w", err)
	}
	var children []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		children = append(children, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	childReason := fmt.Sprintf("%s: superseded by the re-plan of the parent (%s)", codexChangedPrefix, ch.describe())
	for _, id := range children {
		cf, err := wg.TransitionStateTx(ctx, tx, id, models.WorkItemStateBlocked, agentID, childReason)
		if err != nil {
			return res, fmt.Errorf("block subtask %s: %w", id, err)
		}
		res.changes = append(res.changes, stateChange{id, cf, models.WorkItemStateBlocked, childReason})
	}
	if len(children) > 0 {
		// The planner never releases (or counts as done) a task of the
		// plan this re-plan replaces.
		if _, err := tx.Exec(ctx, `UPDATE plan_tasks SET status = 'failed', completed_at = now() WHERE id = ANY($1) AND status IN ('held', 'released')`, children); err != nil {
			return res, fmt.Errorf("close superseded plan tasks: %w", err)
		}
	}

	ev, err := codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
		WorkItemID: parent.ID, ProductID: ch.ProductID, Branch: ch.Branch, Kind: codex.EventReplanned, Automatic: automatic,
		FromVersion: ch.From, ToVersion: ch.To, Modules: ch.Modules, Sections: ch.Sections, ItemState: string(from), Reason: reason,
	})
	if err != nil {
		return res, err
	}
	productID := ch.ProductID
	if parent.ProductID != nil {
		productID = *parent.ProductID
	}
	msg := technicalBriefReplanMessage{
		WorkID: parent.ID.String(), ProductID: productID.String(), BriefContent: prose,
		Timestamp: time.Now().UTC().Format(time.RFC3339), Criteria: criteria, Replan: true, ReplanEventID: ev.ID.String(), Reason: reason,
	}
	if err := outbox.EnqueueJSON(ctx, tx, aggregateFor(parent), bus.TopicTechnicalBriefReady, parent.ID.String(), msg); err != nil {
		return res, fmt.Errorf("enqueue technical-brief.ready: %w", err)
	}
	return res, nil
}

// hasOpenPROrPendingChange reports whether the parent (or one of its
// subtasks) has a pending PR-gate change, or a PR was opened for it (a
// pr.ready carrying a PR URL; a replay hand-off carries none).
func hasOpenPROrPendingChange(ctx context.Context, q codex.Querier, parentID uuid.UUID) (bool, error) {
	var open bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
		           SELECT 1 FROM pending_pr_changes
		            WHERE status = 'pending'
		              AND (work_item_id = $1 OR work_item_id IN (SELECT id FROM work_items WHERE parent_id = $1)))
		    OR EXISTS (
		           SELECT 1 FROM outbox_events
		            WHERE topic = 'pr.ready' AND (aggregate_id = $1 OR key = $1::text)
		              AND COALESCE(payload->>'pr_url', '') <> '')
	`, parentID).Scan(&open)
	if err != nil {
		return false, fmt.Errorf("open PR check for %s: %w", parentID, err)
	}
	return open, nil
}

// tokensSpent is the work item's token spend: the sum the tool loop's and
// the planner's budget checks read.
func tokensSpent(ctx context.Context, q codex.Querier, workID uuid.UUID) (int64, error) {
	var spent int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs WHERE work_item_id = $1`, workID).Scan(&spent); err != nil {
		return 0, fmt.Errorf("token spend of %s: %w", workID, err)
	}
	return spent, nil
}

// outsideCodexRound reports whether a subtask's completion is from outside
// its parent's current round in the codex re-plan sense: the subtask was
// superseded by a codex re-plan, or created before the parent's last one.
// A read failure counts it in (the earlier behaviour).
func outsideCodexRound(ctx context.Context, wg workItemStore, sub *models.WorkItem) (bool, string) {
	ts, err := wg.GetTransitions(ctx, sub.ID)
	if err == nil && workgraph.Superseded(sub.State, ts) {
		return true, "superseded by a codex re-plan of its parent"
	}
	pts, err := wg.GetTransitions(ctx, *sub.ParentID)
	if err != nil {
		return false, ""
	}
	r := workgraph.NewRound(pts)
	if !r.CodexReplan.IsZero() && !sub.CreatedAt.IsZero() && sub.CreatedAt.Before(r.CodexReplan) {
		return true, "created before the parent's codex re-plan (a plan that was replaced)"
	}
	return false, ""
}

// reenqueueLatestVerificationTx enqueues the parent's latest
// verification.complete again (a new event id, the same payload) so
// pr-coordinator, which held the PR while a codex re-plan escalation was
// pending, decides it now. Nothing when the parent was not verified yet (no
// verification_results row: no PR can have been held).
//
// redecided is false when the parent WAS verified (pr-coordinator may hold its
// PR) but no verification.complete row is left to re-enqueue (pruned): the
// held PR cannot be re-decided, and the caller must escalate instead of
// leaving the item waiting silently.
func reenqueueLatestVerificationTx(ctx context.Context, tx pgx.Tx, parentID uuid.UUID) (redecided bool, err error) {
	var raw []byte
	err = tx.QueryRow(ctx, `
		SELECT payload FROM outbox_events
		 WHERE topic = $1 AND aggregate_id = $2
		 ORDER BY created_at DESC, id DESC LIMIT 1`, bus.TopicVerificationComplete, parentID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		var verified bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM verification_results WHERE work_item_id = $1)`, parentID).Scan(&verified); err != nil {
			return false, fmt.Errorf("check verification results: %w", err)
		}
		return !verified, nil
	}
	if err != nil {
		return false, fmt.Errorf("read latest verification.complete: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, fmt.Errorf("decode latest verification.complete: %w", err)
	}
	delete(payload, outbox.FieldEventID)
	if err := outbox.EnqueueJSON(ctx, tx, parentID, bus.TopicVerificationComplete, parentID.String(), payload); err != nil {
		return false, err
	}
	return true, nil
}

// heldPRNotRedecidedReason is the blocked escalation's question when
// "Continue as is" finds no verification.complete to re-enqueue for a
// verified parent.
func heldPRNotRedecidedReason(parentID uuid.UUID) string {
	return fmt.Sprintf("Continue as is was chosen on the codex re-plan escalation of %s, but its held PR could not be re-decided: "+
		"the item was verified while the escalation was pending, and its latest verification.complete event is no longer in the outbox "+
		"(pruned), so pr-coordinator cannot decide the PR. Retry to run the item again, or Cancel.", parentID)
}

// lockPromptStateSQL locks the item and reads what an answer needs.
const lockPromptStateSQL = `SELECT state::text, previous_state, awaiting_prompt_id FROM work_items WHERE id = $1 FOR UPDATE`

// handleCodexPromptAnswer acts on the answer to a codex re-plan escalation
// in one transaction carrying the delivery's guard row: the item is restored
// from awaiting_user (when the prompt parked it) and then
//
//   - replan: re-planned as by a sweep (replanTx), not counted against
//     CODEX_REPLAN_MAX;
//   - continue: the change is acknowledged (a continued entry); the item
//     goes on with its plan and is settled as after any answer;
//   - cancel: cancelled.
//
// handled is false for any other prompt.
func handleCodexPromptAnswer(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, promptID, workItemID uuid.UUID, action string) (handled bool, err error) {
	pool := wg.DB()
	if pool == nil || promptID == uuid.Nil {
		return false, nil
	}
	ev, found, err := codex.ItemEventByPrompt(ctx, pool, promptID)
	if err != nil {
		return true, err
	}
	if !found || ev.Kind != codex.EventEscalated {
		return false, nil
	}
	item, err := wg.GetWorkItem(ctx, workItemID)
	if err != nil {
		log.Printf("status-orchestrator: codex re-plan answer: get work item %s: %v", workItemID, err)
		return true, nil
	}
	current, err := codex.New(pool).GetCurrentVersion(ctx, ev.ProductID, ev.Branch)
	if err != nil {
		return true, err
	}
	// Review fix 3: what changed while the prompt was pending. A PR or
	// pending change opened meanwhile refuses a re-plan (and needs no
	// re-enqueued verification); verified work refuses it too unless the
	// escalation was raised because of that verified work (the operator
	// chose to set it aside).
	openPR, err := hasOpenPROrPendingChange(ctx, pool, workItemID)
	if err != nil {
		return true, err
	}
	verified, verifiedWhy := false, ""
	if action == "replan" {
		if verified, verifiedWhy, err = verifiedWorkInRound(ctx, wg, workItemID); err != nil {
			return true, err
		}
	}
	var refusal string
	switch {
	case action != "replan":
	case openPR:
		refusal = "a PR or pending change was opened for it while the escalation was pending"
	case verified && ev.Cause != causeVerifiedWork:
		refusal = "part of its plan was verified while the escalation was pending (" + verifiedWhy + ")"
	}
	var res replanResult
	settle := false
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, pool), func(tx pgx.Tx) error {
		res, settle = replanResult{}, false
		var state string
		var previous *string
		var awaiting *uuid.UUID
		if err := tx.QueryRow(ctx, lockPromptStateSQL, workItemID).Scan(&state, &previous, &awaiting); err != nil {
			return fmt.Errorf("lock work item: %w", err)
		}
		cur := models.WorkItemState(state)
		if cur == models.WorkItemStateAwaitingUser && awaiting != nil && *awaiting == promptID && previous != nil && *previous != "" {
			now := time.Now().UTC()
			if _, err := tx.Exec(ctx, restoreFromAwaitingUserSQL, workItemID, now); err != nil {
				return fmt.Errorf("restore from prompt: %w", err)
			}
			reason := "prompt " + promptID.String() + " answered"
			if _, err := tx.Exec(ctx, insertBookkeepingTransitionSQL, uuid.New(), workItemID, cur, *previous, agentID, reason, now); err != nil {
				return fmt.Errorf("record restore transition: %w", err)
			}
			res.changes = append(res.changes, stateChange{workItemID, cur, models.WorkItemState(*previous), reason})
			cur = models.WorkItemState(*previous)
		}
		switch action {
		case "replan":
			if !replannable(cur) {
				log.Printf("status-orchestrator: codex re-plan answer for %s: item is %s — not re-planned", workItemID, cur)
				return nil
			}
			ch := staleChange{ProductID: ev.ProductID, Branch: ev.Branch, From: ev.FromVersion, To: max(current, ev.ToVersion),
				Staleness: codex.Staleness{Modules: ev.Modules, Sections: ev.Sections}}
			if refusal != "" {
				// Recorded and told: the operator may continue or cancel.
				reason := "re-plan refused: " + refusal + "; " + ch.describe()
				if _, err := codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
					WorkItemID: workItemID, ProductID: ev.ProductID, Branch: ev.Branch, Kind: codex.EventNoted, Automatic: false,
					FromVersion: ch.From, ToVersion: ch.To, Modules: ch.Modules, Sections: ch.Sections, ItemState: string(cur),
					Reason: reason, Cause: causeRefused,
				}); err != nil {
					return err
				}
				question := fmt.Sprintf("The re-plan you chose was not done: %s. Re-planning now would throw away that work. Continue as is with the current plan, or cancel.", refusal)
				return raiseCodexEscalationTx(ctx, tx, item, ch, string(cur), question, causeRefused, false)
			}
			rr, err := replanTx(ctx, tx, wg, item, ch, false, -1)
			if err != nil {
				return err
			}
			res.changes = append(res.changes, rr.changes...)
		case "continue":
			if _, err := codex.RecordItemEventTx(ctx, tx, codex.ItemEvent{
				WorkItemID: workItemID, ProductID: ev.ProductID, Branch: ev.Branch, Kind: codex.EventContinued, Automatic: false,
				FromVersion: ev.FromVersion, ToVersion: ev.ToVersion, Modules: ev.Modules, Sections: ev.Sections, ItemState: string(cur),
				Reason:   "continue as is chosen by the operator: " + codex.Staleness{Modules: ev.Modules, Sections: ev.Sections}.Describe(ev.FromVersion, ev.ToVersion),
				PromptID: &promptID,
			}); err != nil {
				return err
			}
			// pr-coordinator held the PR while the prompt was pending; the
			// latest verification.complete, enqueued again, lets it open it.
			if !openPR {
				redecided, err := reenqueueLatestVerificationTx(ctx, tx, workItemID)
				if err != nil {
					return err
				}
				if !redecided {
					// The held PR cannot be re-decided: never silent. The
					// item is blocked with the Retry / Cancel escalation.
					reason := heldPRNotRedecidedReason(workItemID)
					log.Printf("status-orchestrator: ERROR codex re-plan answer for %s: no verification.complete to re-enqueue for a verified item — blocking with an escalation", workItemID)
					from, err := wg.TransitionStateTx(ctx, tx, workItemID, models.WorkItemStateBlocked, agentID, reason)
					if err != nil {
						return fmt.Errorf("block after an unre-decidable held PR: %w", err)
					}
					if err := blockedPromptEffect(reason)(ctx, tx, item); err != nil {
						return err
					}
					res.changes = append(res.changes, stateChange{workItemID, from, models.WorkItemStateBlocked, reason})
					return nil
				}
			}
			settle = true
		case "cancel":
			reason := "cancelled: codex changed since planning; Cancel chosen on the re-plan escalation"
			from, err := wg.TransitionStateTx(ctx, tx, workItemID, models.WorkItemStateCancelled, agentID, reason)
			if err != nil {
				return fmt.Errorf("cancel: %w", err)
			}
			res.changes = append(res.changes, stateChange{workItemID, from, models.WorkItemStateCancelled, reason})
		default:
			log.Printf("status-orchestrator: codex re-plan answer for %s: unknown action %q", workItemID, action)
		}
		return nil
	})
	if idempotency.IsDuplicate(err) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	res.publish(ctx, kb)
	log.Printf("status-orchestrator: codex re-plan escalation for %s answered %q", workItemID, action)
	if settle {
		// Subtasks may have finished while the prompt was pending.
		return true, settleVerification(idempotency.Unguarded(ctx), wg, kb, registry, workItemID)
	}
	return true, nil
}
