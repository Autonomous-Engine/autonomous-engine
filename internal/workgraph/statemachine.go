package workgraph

import (
	"context"
	"fmt"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// transitions defines the legal state transitions in the work item pipeline.
// Each key is a "from" state, and its value is the set of legal "to" states.
var transitions = map[models.WorkItemState][]models.WorkItemState{
	models.WorkItemStateIntake: {
		models.WorkItemStatePRD,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStatePRD: {
		// Happy path now goes through prd_approval (human gate). Direct
		// prd → research is still listed for legacy / admin-forced flows,
		// but product-agent routes everything through approval by default.
		models.WorkItemStatePRDApproval,
		models.WorkItemStateResearch,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStatePRDApproval: {
		models.WorkItemStateResearch, // admin approved
		models.WorkItemStatePRD,      // admin requested changes → regenerate
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateRejected, // admin rejected
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateResearch: {
		models.WorkItemStatePlanning,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStatePlanning: {
		// Codex coherence Part 1: re-entry. A codex re-plan of a parent
		// still in planning records it, and the planner's own move to
		// planning on the re-emitted technical brief finds it there.
		models.WorkItemStatePlanning,
		models.WorkItemStateImplementation,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateImplementation: {
		models.WorkItemStateVerification, // R14: every subtask implemented → verified before any PR
		// Codex coherence Part 1: the codex the plan relied on changed and
		// the parent is planned again (status-orchestrator's codex re-plan).
		models.WorkItemStatePlanning,
		models.WorkItemStateReview,
		models.WorkItemStatePassed, // subtasks can complete directly after implementation
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	// R14: the independent verifier judges each subtask's head before the
	// PR. approve → review (or the D2/D3 human gate); escalate →
	// human_review; request_changes → implementation; a stage failure →
	// blocked.
	models.WorkItemStateVerification: {
		models.WorkItemStateReview,
		models.WorkItemStateHumanReview,
		models.WorkItemStateImplementation,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateReview: {
		models.WorkItemStateHumanReview,
		models.WorkItemStateImplementation, // changes requested
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateHumanReview: {
		models.WorkItemStateQA,
		models.WorkItemStateImplementation, // changes requested
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateRejected,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateQA: {
		models.WorkItemStatePassed,
		models.WorkItemStateFailed,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateFailed: {
		models.WorkItemStateResearch,
		models.WorkItemStatePRD, // manual retry from beginning
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateAwaitingUser: {
		models.WorkItemStatePRD,
		models.WorkItemStatePRDApproval,
		models.WorkItemStateResearch,
		models.WorkItemStatePlanning,
		models.WorkItemStateImplementation,
		models.WorkItemStateVerification,
		models.WorkItemStateReview,
		models.WorkItemStateHumanReview,
		models.WorkItemStateQA,
		models.WorkItemStateBlocked,
		models.WorkItemStateCancelled,
	},
	models.WorkItemStateBlocked: {
		models.WorkItemStateIntake,         // full restart (no PRD exists yet)
		models.WorkItemStatePRD,            // reset (PRD exists, skip to research)
		models.WorkItemStateResearch,       // retry from research (brief exists)
		models.WorkItemStatePlanning,       // retry from planning (decomposition failed)
		models.WorkItemStateImplementation, // retry subtask from implementation
		models.WorkItemStateVerification,   // R14: subtask retry completed → parent is verified again
		models.WorkItemStateReview,         // subtask retry completed → parent moves to review
		models.WorkItemStateCancelled,
	},
	models.WorkItemStatePassed: {
		models.WorkItemStateReleased,
	},
}

// IsValidTransition returns true if transitioning from the given state to the
// target state is permitted by the pipeline rules.
func IsValidTransition(from, to models.WorkItemState) bool {
	allowed, ok := transitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// TransitionToAwaitingUser saves the work item's current state into
// previous_state and moves state to 'awaiting_user', pinning the prompt that
// caused the yield. No-op if the row doesn't exist.
//
// Does NOT go through IsValidTransition because this is called atomically
// in response to a Kafka event — the caller (status-orchestrator) trusts
// the event. Multi-prompt-per-item is disallowed in v1: if the work item
// is already awaiting_user, the UPDATE's WHERE clause skips the change.
func TransitionToAwaitingUser(ctx context.Context, db *pgxpool.Pool, workItemID, promptID uuid.UUID) error {
	_, err := db.Exec(ctx, `
		UPDATE work_items
		SET previous_state = state,
		    state = 'awaiting_user',
		    awaiting_prompt_id = $2,
		    updated_at = NOW()
		WHERE id = $1 AND state != 'awaiting_user'
	`, workItemID, promptID)
	return err
}

// TransitionFromAwaitingUser restores the work item to its previous_state
// after the user answers. No-op when the work item isn't awaiting_user or
// previous_state is null.
func TransitionFromAwaitingUser(ctx context.Context, db *pgxpool.Pool, workItemID uuid.UUID) error {
	_, err := db.Exec(ctx, `
		UPDATE work_items
		SET state = previous_state::work_item_state,
		    previous_state = NULL,
		    awaiting_prompt_id = NULL,
		    updated_at = NOW()
		WHERE id = $1 AND state = 'awaiting_user' AND previous_state IS NOT NULL
	`, workItemID)
	return err
}

// TransitionFromAwaitingUserToState atomically transitions a work item to the
// given target state, clearing awaiting_user metadata. Handles the race where
// the item might be in awaiting_user OR already restored to its previous state
// (e.g., prd_approval). Also records a transition audit row. It is
// TransitionFromAwaitingUserToStateTx in a transaction of its own.
func TransitionFromAwaitingUserToState(ctx context.Context, db *pgxpool.Pool, workItemID uuid.UUID, toState models.WorkItemState, agentID, reason string) error {
	return WithTx(ctx, db, func(tx pgx.Tx) error {
		return TransitionFromAwaitingUserToStateTx(ctx, tx, workItemID, toState, agentID, reason)
	})
}

// The statements of TransitionFromAwaitingUserToStateTx, shared with the pooled
// form through it.
const (
	selectAwaitingStateForUpdateSQL = `SELECT state FROM work_items WHERE id = $1 FOR UPDATE`
	updateFromAwaitingUserSQL       = `
		UPDATE work_items
		SET state = $1,
		    previous_state = NULL,
		    awaiting_prompt_id = NULL,
		    updated_at = $2
		WHERE id = $3
	`
	insertAwaitingTransitionSQL = `
		INSERT INTO work_item_transitions (id, work_item_id, from_state, to_state, agent_id, reason, timestamp)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
)

// TransitionFromAwaitingUserToStateTx is TransitionFromAwaitingUserToState in
// the caller's transaction, so dashboard-backend can enqueue the event that
// announces the transition in the same commit. It never commits or rolls back;
// any error must roll the caller's transaction back.
func TransitionFromAwaitingUserToStateTx(ctx context.Context, tx pgx.Tx, workItemID uuid.UUID, toState models.WorkItemState, agentID, reason string) error {
	if tx == nil {
		return fmt.Errorf("transition from awaiting_user: nil transaction")
	}
	var currentState models.WorkItemState
	if err := tx.QueryRow(ctx, selectAwaitingStateForUpdateSQL, workItemID).Scan(&currentState); err != nil {
		return fmt.Errorf("select current state: %w", err)
	}

	if currentState != models.WorkItemStateAwaitingUser && currentState != models.WorkItemStatePRDApproval {
		return fmt.Errorf("invalid transition: state is %s, expected awaiting_user or prd_approval", currentState)
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, updateFromAwaitingUserSQL, toState, now, workItemID); err != nil {
		return fmt.Errorf("update work item state: %w", err)
	}
	if _, err := tx.Exec(ctx, insertAwaitingTransitionSQL, uuid.New(), workItemID, currentState, toState, agentID, reason, now); err != nil {
		return fmt.Errorf("insert transition: %w", err)
	}
	return nil
}
