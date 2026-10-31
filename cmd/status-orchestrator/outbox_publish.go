package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// M1-04a: status-orchestrator publishes through the transactional outbox.
//
// Every event that announces a state change this service makes (qa.ready,
// implementation.tasks for a fix subtask, user_prompt.requested for the
// human-review and blocked prompts, codex.updated) is enqueued with
// outbox.EnqueueJSON / interactions.PublishTx inside the same transaction as
// the transition or row write, through workgraph.WithTx. The service's outbox
// dispatcher (started once in main) carries the rows to Kafka.
//
// state.changed is websocket/Jira fan-out telemetry and stays a direct
// publish (publishStateChanged), but only after the transaction has
// committed, so nobody is ever told about a transition that rolled back.

// M1-05: every helper below opens its transaction through
// idempotency.Wrap(ctx, db), so the first of them to commit for a delivery
// carries the processed_events guard row as its first statement, and a
// redelivered event fails Begin with idempotency.ErrDuplicate before writing
// anything. Ledger.Handler (main) acknowledges such a delivery. Steps that
// run in later transactions of the same handler are not covered by the
// guard; the spec's per-consumer table lists them.

// topicStateChanged is the telemetry topic published after every committed
// transition. It is the only topic this service publishes directly.
const topicStateChanged = "state.changed"

// txEffect is a write that must commit in the same transaction as a
// transition: an outbox event or a prompt row plus its event. item is the
// transitioned work item as read before the transaction (its id, parent,
// product and title do not change with a transition).
type txEffect func(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error

// aggregateFor is the outbox aggregate of an item's events: its parent when it
// has one, otherwise the item itself (common3 outbox rule).
func aggregateFor(item *models.WorkItem) uuid.UUID {
	if item.ParentID != nil && *item.ParentID != uuid.Nil {
		return *item.ParentID
	}
	return item.ID
}

// enqueueEffect enqueues topic/key/payload, exactly as they were published
// directly before the conversion, under the item's aggregate.
func enqueueEffect(topic, key string, payload any) txEffect {
	return func(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error {
		return outbox.EnqueueJSON(ctx, tx, aggregateFor(item), topic, key, payload)
	}
}

// promptEffect writes the prompt row and its user_prompt.requested event in
// the caller's transaction (interactions.PublishTx).
//
// user_interactions.product_id is NOT NULL with a foreign key, so a prompt for
// an item without a product can never be stored; before the conversion such a
// prompt was published and then failed at every consumer. Inside the
// transaction it would roll back the transition it accompanies, so it is
// skipped with a log line instead and the transition still commits.
func promptEffect(build func(item *models.WorkItem) *interactions.Prompt) txEffect {
	return func(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error {
		p := build(item)
		if p.ProductID == uuid.Nil {
			log.Printf("status-orchestrator: %s prompt for work item %s skipped: the item has no product (user_interactions.product_id is required)", p.Trigger, item.ID)
			return nil
		}
		if err := interactions.NewPublisher(nil).PublishTx(ctx, tx, aggregateFor(item), p); err != nil {
			return fmt.Errorf("raise %s prompt: %w", p.Trigger, err)
		}
		return nil
	}
}

// humanReviewPromptEffect raises the approval prompt for an item entering
// human_review, so the Approvals page surfaces it alongside PRD approvals.
func humanReviewPromptEffect(reason string) txEffect {
	return promptEffect(func(item *models.WorkItem) *interactions.Prompt {
		return humanReviewPrompt(item, reason)
	})
}

// blockedPromptEffect raises the escalation prompt for an item entering
// blocked, so the Approvals page surfaces it with retry/cancel options.
func blockedPromptEffect(reason string) txEffect {
	return promptEffect(func(item *models.WorkItem) *interactions.Prompt {
		return blockedPrompt(item, reason)
	})
}

// transitionAndPublish transitions a work item and, in the same transaction,
// runs effects (outbox events, prompts). After the commit it publishes the
// state.changed telemetry with the from-state read under the row lock.
//
// Any error rolls everything back and is returned; nothing is published. An
// invalid transition is returned wrapping workgraph.ErrInvalidTransition and
// runs no effect, as before the conversion.
func transitionAndPublish(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, workID uuid.UUID, toState models.WorkItemState, reason string, effects ...txEffect) error {
	item, err := wg.GetWorkItem(ctx, workID)
	if err != nil {
		return fmt.Errorf("get work item: %w", err)
	}

	var fromState models.WorkItemState
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		from, err := wg.TransitionStateTx(ctx, tx, workID, toState, agentID, reason)
		if err != nil {
			return fmt.Errorf("transition state: %w", err)
		}
		fromState = from
		for _, effect := range effects {
			if err := effect(ctx, tx, item); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	publishStateChanged(ctx, kb, workID, fromState, toState, reason)
	return nil
}

// pendingEscalationPromptSQL reports whether an escalation prompt is still
// pending for a work item: this service's own "Blocked: …" prompt, or the
// escalation another service raised before it blocked the item.
const pendingEscalationPromptSQL = `
	SELECT EXISTS (
		SELECT 1 FROM user_interactions
		WHERE work_item_id = $1 AND prompt_type = 'escalation' AND state = 'pending'
	)`

// lockWorkItemStateSQL locks the work item row for the rest of the
// transaction and returns its state and the state awaiting_user will restore.
const lockWorkItemStateSQL = `SELECT state::text, previous_state FROM work_items WHERE id = $1 FOR UPDATE`

// blockFromWorkBlocked handles a work.blocked event in one transaction.
//
// Converted guard blocks (guards.BlockWorkItemTx) emit work.blocked AFTER
// they have already blocked the item, so an item that is already blocked is
// not an error and gets no second transition. An item in awaiting_user whose
// previous_state is blocked counts as already blocked too: its escalation
// prompt was consumed before this event, and transitioning it would orphan
// that prompt's awaiting_prompt_id / previous_state. The generic escalation prompt
// is raised only when no escalation prompt is pending for the item, so a
// service that raised its own prompt before blocking, or a redelivered
// event, does not ring the bell twice. The check runs after the row lock
// TransitionStateTx takes, so concurrent deliveries serialise on it.
//
// state.changed is published after the commit, only when a transition was
// made. Any other transition failure (missing item, terminal state) is
// returned and nothing is written.
func blockFromWorkBlocked(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, workID uuid.UUID, reason string) error {
	return blockFromWorkBlockedKind(ctx, db, wg, kb, workID, reason, "")
}

// blockFromWorkBlockedKind is blockFromWorkBlocked for a work.blocked of
// kind (guards.WorkBlockedPayload.Kind): codex_ungrounded raises the
// Re-index / Retry / Cancel escalation (codexRefusalPrompt), anything else
// the generic one.
func blockFromWorkBlockedKind(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, workID uuid.UUID, reason, kind string) error {
	item, err := wg.GetWorkItem(ctx, workID)
	if err != nil {
		return fmt.Errorf("get work item: %w", err)
	}

	var fromState models.WorkItemState
	transitioned := false
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		transitioned = false

		// Take the row lock first and read the parked state. A producer
		// that raised its own escalation prompt before blocking can have
		// that prompt consumed first, which parks the item in awaiting_user
		// with previous_state = blocked. awaiting_user→blocked is a valid
		// edge, but taking it would strand awaiting_prompt_id and
		// previous_state, so the user's answer would restore nothing.
		var state string
		var previous *string
		if err := tx.QueryRow(ctx, lockWorkItemStateSQL, workID).Scan(&state, &previous); err != nil {
			return fmt.Errorf("lock work item: %w", err)
		}
		parkedBlocked := models.WorkItemState(state) == models.WorkItemStateAwaitingUser &&
			previous != nil && models.WorkItemState(*previous) == models.WorkItemStateBlocked

		if !parkedBlocked {
			from, err := wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStateBlocked, agentID, reason)
			switch {
			case err == nil:
				transitioned, fromState = true, from
			case errors.Is(err, workgraph.ErrInvalidTransition) && from == models.WorkItemStateBlocked:
				// Already blocked by whoever emitted the event: no transition.
			default:
				return fmt.Errorf("transition state: %w", err)
			}
		}

		var pending bool
		if err := tx.QueryRow(ctx, pendingEscalationPromptSQL, workID).Scan(&pending); err != nil {
			return fmt.Errorf("check pending escalation prompt: %w", err)
		}
		if pending {
			return nil
		}
		return blockedPromptEffectFor(kind, reason)(ctx, tx, item)
	})
	if err != nil {
		return err
	}
	if transitioned {
		publishStateChanged(ctx, kb, workID, fromState, models.WorkItemStateBlocked, reason)
	}
	return nil
}

// dispatchFixSubtask creates a fix subtask (review feedback or QA bugs) and
// enqueues its implementation.tasks event in one transaction, like the
// planner does for a decomposition: no subtask without its event, no event
// for a subtask that does not exist. The key (the parent id) and payload are
// unchanged from the direct publish.
func dispatchFixSubtask(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, parentID uuid.UUID, fixItem *models.WorkItem, msg map[string]interface{}) error {
	return workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		if err := wg.CreateWorkItemTx(ctx, tx, fixItem); err != nil {
			return fmt.Errorf("create fix subtask: %w", err)
		}
		return outbox.EnqueueJSON(ctx, tx, parentID, bus.TopicImplementationTasks, parentID.String(), msg)
	})
}

// dispatchFixOrBlock dispatches a fix subtask and, when that transaction
// fails, blocks the parent with the reason and the blocked prompt instead of
// carrying on as if the fix had been dispatched: before the conversion a
// failed publish left the parent in implementation with a subtask nobody
// would ever pick up; now nothing was written, and an operator must see why.
func dispatchFixOrBlock(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, topic string, parentID uuid.UUID, fixItem *models.WorkItem, msg map[string]interface{}) {
	err := dispatchFixSubtask(ctx, db, wg, parentID, fixItem, msg)
	if err == nil {
		return
	}
	if idempotency.IsDuplicate(err) {
		// M1-05: the event was already processed; its fix subtask exists.
		return
	}
	log.Printf("status-orchestrator: %s: dispatch fix subtask %q for %s: %v — blocking the work item", topic, fixItem.Title, parentID, err)
	// The full error stays in the log line above; the transition reason and
	// the user-visible prompt carry only a short, stable explanation.
	reason := fmt.Sprintf("could not dispatch fix subtask %q (internal error; see status-orchestrator logs) — retry to re-dispatch", fixItem.Title)
	if berr := transitionAndPublish(ctx, db, wg, kb, parentID, models.WorkItemStateBlocked, reason, blockedPromptEffect(reason)); berr != nil {
		log.Printf("status-orchestrator: %s: block %s after failed fix dispatch: %v", topic, parentID, berr)
	}
}

// enqueueAlone enqueues an event that accompanies no row write of this
// service (codex.updated after a merge) in a short transaction of its own.
func enqueueAlone(ctx context.Context, db workgraph.TxBeginner, aggregateID uuid.UUID, topic, key string, payload any) error {
	return workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		return outbox.EnqueueJSON(ctx, tx, aggregateID, topic, key, payload)
	})
}

// raisePromptAlone raises a prompt in its own transaction. It serves the
// legacy branches that raise a prompt even when the transition it would have
// accompanied failed.
func raisePromptAlone(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, workID uuid.UUID, effect txEffect) error {
	item, err := wg.GetWorkItem(ctx, workID)
	if err != nil {
		return fmt.Errorf("get work item: %w", err)
	}
	return workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		return effect(ctx, tx, item)
	})
}

// publishStateChanged publishes the state.changed telemetry for a committed
// transition. A failure is logged: the transition and its events have
// already committed, and state.changed is fan-out only.
func publishStateChanged(ctx context.Context, kb bus.MessageBus, workID uuid.UUID, from, to models.WorkItemState, reason string) {
	data, err := json.Marshal(stateChangedEvent{
		WorkID:    workID.String(),
		FromState: string(from),
		ToState:   string(to),
		Reason:    reason,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		log.Printf("status-orchestrator: marshal state.changed for %s: %v", workID, err)
		return
	}
	if err := kb.Publish(ctx, topicStateChanged, workID.String(), data); err != nil {
		log.Printf("status-orchestrator: publish state.changed for %s: %v", workID, err)
	}
}

// humanReviewPrompt builds the approval prompt for an item entering
// human_review. ProductID is uuid.Nil when the item has none (promptEffect
// then skips it).
func humanReviewPrompt(item *models.WorkItem, reason string) *interactions.Prompt {
	return &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  item.ID,
		ProductID:   productOf(item),
		AskingAgent: "status-orchestrator",
		Trigger:     interactions.TriggerApprovalHumanReview,
		PromptType:  interactions.PromptTypeApproval,
		Headline:    fmt.Sprintf("Human review required: %s", item.Title),
		Question:    reason,
		ContextLink: fmt.Sprintf("/work-items/%s", item.ID),
		Actions: []interactions.Action{
			{ID: "approve", Label: "Approve", Semantic: interactions.SemanticContinue},
			{ID: "reject", Label: "Reject", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

// blockedPrompt builds the escalation prompt for an item entering blocked.
func blockedPrompt(item *models.WorkItem, reason string) *interactions.Prompt {
	return &interactions.Prompt{
		ID:          uuid.New(),
		WorkItemID:  item.ID,
		ProductID:   productOf(item),
		AskingAgent: "status-orchestrator",
		Trigger:     interactions.TriggerBlocked,
		PromptType:  interactions.PromptTypeEscalation,
		Headline:    fmt.Sprintf("Blocked: %s", item.Title),
		Question:    reason,
		ContextLink: fmt.Sprintf("/work-items/%s", item.ID),
		Actions: []interactions.Action{
			{ID: "retry", Label: "Retry", Semantic: interactions.SemanticRetry},
			{ID: "cancel", Label: "Cancel", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

func productOf(item *models.WorkItem) uuid.UUID {
	if item.ProductID != nil {
		return *item.ProductID
	}
	return uuid.Nil
}
