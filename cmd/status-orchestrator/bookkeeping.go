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
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// R17 L4: transition bookkeeping. A live run's parent (attempt 14) recorded
// planning → blocked (the budget stop) and then awaiting_user → blocked
// (the planner): the move blocked → awaiting_user in between, made when the
// budget prompt was consumed, wrote no row, so the second row's from_state
// matched nothing before it. The parent was also never recorded in
// implementation: it stayed in planning while its subtask ran and only
// walked planning → implementation when every subtask had finished.
//
// Now:
//   - parking an item on a prompt (state → awaiting_user) and restoring it
//     when the prompt is answered record their transitions, with the
//     from_state read under the row lock;
//   - the parent walks planning → implementation when one of its tasks is
//     dispatched (implementation.tasks) — and, should that event be missed,
//     when a subtask's implementation.complete arrives — so a later block
//     or verification follows implementation in its history.

// childTasksRunningReason is the planning → implementation reason (as
// settleVerification's walk has always written it).
const childTasksRunningReason = "child tasks running"

const (
	parkAwaitingUserSQL = `
		UPDATE work_items
		SET previous_state = state,
		    state = 'awaiting_user',
		    awaiting_prompt_id = $2,
		    updated_at = $3
		WHERE id = $1`
	restoreFromAwaitingUserSQL = `
		UPDATE work_items
		SET state = previous_state::work_item_state,
		    previous_state = NULL,
		    awaiting_prompt_id = NULL,
		    updated_at = $2
		WHERE id = $1`
	insertBookkeepingTransitionSQL = `
		INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason, timestamp)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
)

// parkOnPrompt moves workID to awaiting_user for promptID, saving its state
// in previous_state, and records the transition — from the state read under
// the row lock. An item already awaiting_user (one prompt at a time) or
// missing is left alone.
func parkOnPrompt(ctx context.Context, db workgraph.TxBeginner, workID, promptID uuid.UUID) (from models.WorkItemState, parked bool, err error) {
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		parked = false
		var state string
		var previous *string
		if err := tx.QueryRow(ctx, lockWorkItemStateSQL, workID).Scan(&state, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("lock work item: %w", err)
		}
		if models.WorkItemState(state) == models.WorkItemStateAwaitingUser {
			return nil
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, parkAwaitingUserSQL, workID, promptID, now); err != nil {
			return fmt.Errorf("park on prompt: %w", err)
		}
		if _, err := tx.Exec(ctx, insertBookkeepingTransitionSQL, uuid.New(), workID, state, models.WorkItemStateAwaitingUser,
			agentID, "awaiting user: prompt "+promptID.String(), now); err != nil {
			return fmt.Errorf("record awaiting_user transition: %w", err)
		}
		from, parked = models.WorkItemState(state), true
		return nil
	})
	return from, parked, err
}

// restoreFromPrompt moves an item parked in awaiting_user back to its
// previous_state and records the transition. Anything else is left alone
// (the answer's own handler may already have moved it on).
func restoreFromPrompt(ctx context.Context, db workgraph.TxBeginner, workID, promptID uuid.UUID) (to models.WorkItemState, restored bool, err error) {
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		restored = false
		var state string
		var previous *string
		if err := tx.QueryRow(ctx, lockWorkItemStateSQL, workID).Scan(&state, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("lock work item: %w", err)
		}
		if models.WorkItemState(state) != models.WorkItemStateAwaitingUser || previous == nil || *previous == "" {
			return nil
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, restoreFromAwaitingUserSQL, workID, now); err != nil {
			return fmt.Errorf("restore from prompt: %w", err)
		}
		if _, err := tx.Exec(ctx, insertBookkeepingTransitionSQL, uuid.New(), workID, models.WorkItemStateAwaitingUser, *previous,
			agentID, "prompt "+promptID.String()+" answered", now); err != nil {
			return fmt.Errorf("record restore transition: %w", err)
		}
		to, restored = models.WorkItemState(*previous), true
		return nil
	})
	return to, restored, err
}

// walkParentToImplementation moves parentID planning → implementation
// (under the row lock; any other state is left alone) and publishes
// state.changed after the commit.
func walkParentToImplementation(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, parentID uuid.UUID) error {
	transitioned := false
	err := workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		transitioned = false
		var state string
		var previous *string
		if err := tx.QueryRow(ctx, lockWorkItemStateSQL, parentID).Scan(&state, &previous); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("lock work item: %w", err)
		}
		if models.WorkItemState(state) != models.WorkItemStatePlanning {
			return nil
		}
		if _, err := wg.TransitionStateTx(ctx, tx, parentID, models.WorkItemStateImplementation, agentID, childTasksRunningReason); err != nil {
			return fmt.Errorf("transition state: %w", err)
		}
		transitioned = true
		return nil
	})
	if err != nil {
		return err
	}
	if transitioned {
		publishStateChanged(ctx, kb, parentID, models.WorkItemStatePlanning, models.WorkItemStateImplementation, childTasksRunningReason)
	}
	return nil
}

// handleImplementationTasks walks a task's parent planning → implementation
// when the task is dispatched (the planner's decomposition, a released held
// task, a fix or a retry: only a parent still in planning moves).
func handleImplementationTasks(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, msg *bus.Message) error {
	var p struct {
		TaskID string `json:"task_id"`
		WorkID string `json:"work_id"`
	}
	if err := json.Unmarshal(msg.Value, &p); err != nil {
		log.Printf("status-orchestrator: implementation.tasks: unmarshal: %v", err)
		return nil
	}
	parentID, err := uuid.Parse(p.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: implementation.tasks: parse work_id %q: %v", p.WorkID, err)
		return nil
	}
	if err := walkParentToImplementation(ctx, wg.DB(), wg, kb, parentID); err != nil {
		log.Printf("status-orchestrator: implementation.tasks: task %s: walk parent %s to implementation: %v", p.TaskID, parentID, err)
		return err
	}
	return nil
}
