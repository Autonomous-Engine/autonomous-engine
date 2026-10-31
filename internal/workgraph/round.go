package workgraph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Rounds of a parent's subtasks: the one definition status-orchestrator
// (verification settle) and pr-coordinator (which subtasks are done, which
// get a PR) share.
//
// A round starts when the parent re-enters implementation from a later
// state (verification, review, human_review, qa, failed or blocked: a fix
// round), or when a codex re-plan sends it back to planning (codex
// coherence Part 1: a new plan). A subtask takes part in the current round
// unless it was superseded by a codex re-plan (blocked with a
// codex_changed reason), it passed before the round started, or it was
// created before the parent's last codex re-plan (it belongs to the plan the
// re-plan replaced).

// CodexChangedPrefix starts the reason of every codex re-plan transition:
// the parent's move to planning and each superseded subtask's block.
const CodexChangedPrefix = "codex_changed"

// IsCodexReplan reports whether t is a codex re-plan of a parent.
func IsCodexReplan(t *models.WorkItemTransition) bool {
	return t != nil && t.ToState == models.WorkItemStatePlanning && strings.HasPrefix(t.Reason, CodexChangedPrefix)
}

// RoundStart is when the parent's current round started (zero: the first
// round).
func RoundStart(parentTransitions []*models.WorkItemTransition) time.Time {
	var start time.Time
	for _, t := range parentTransitions {
		if t == nil {
			continue
		}
		starts := IsCodexReplan(t)
		if t.ToState == models.WorkItemStateImplementation {
			switch t.FromState {
			case models.WorkItemStateVerification, models.WorkItemStateReview, models.WorkItemStateHumanReview,
				models.WorkItemStateQA, models.WorkItemStateFailed, models.WorkItemStateBlocked:
				starts = true
			}
		}
		if starts && t.Timestamp.After(start) {
			start = t.Timestamp
		}
	}
	return start
}

// LastCodexReplan is when the parent was last re-planned for a codex change
// (zero: never).
func LastCodexReplan(parentTransitions []*models.WorkItemTransition) time.Time {
	var at time.Time
	for _, t := range parentTransitions {
		if IsCodexReplan(t) && t.Timestamp.After(at) {
			at = t.Timestamp
		}
	}
	return at
}

// LastPassed is when the item last transitioned to passed (zero: never).
func LastPassed(ts []*models.WorkItemTransition) time.Time {
	var at time.Time
	for _, t := range ts {
		if t != nil && t.ToState == models.WorkItemStatePassed && t.Timestamp.After(at) {
			at = t.Timestamp
		}
	}
	return at
}

// Superseded reports whether a subtask in state, with transitions ts, was
// stopped by a codex re-plan of its parent: it is blocked and its latest
// move to blocked carries the codex_changed reason.
func Superseded(state models.WorkItemState, ts []*models.WorkItemTransition) bool {
	if state != models.WorkItemStateBlocked {
		return false
	}
	var last *models.WorkItemTransition
	for _, t := range ts {
		if t != nil && t.ToState == models.WorkItemStateBlocked && (last == nil || !t.Timestamp.Before(last.Timestamp)) {
			last = t
		}
	}
	return last != nil && strings.HasPrefix(last.Reason, CodexChangedPrefix)
}

// Round is a parent's current round.
type Round struct {
	// Start is RoundStart of the parent's transitions.
	Start time.Time
	// CodexReplan is LastCodexReplan of them.
	CodexReplan time.Time
}

// NewRound reads the round from the parent's transitions.
func NewRound(parentTransitions []*models.WorkItemTransition) Round {
	return Round{Start: RoundStart(parentTransitions), CodexReplan: LastCodexReplan(parentTransitions)}
}

// RoundChild is what InRound needs of a subtask.
type RoundChild struct {
	State      models.WorkItemState
	CreatedAt  time.Time
	PassedAt   time.Time
	Superseded bool
}

// InRound reports whether the subtask takes part in the round. Whether it
// is done or ended is the caller's question.
func (r Round) InRound(c RoundChild) bool {
	if c.Superseded {
		return false
	}
	if !r.CodexReplan.IsZero() && !c.CreatedAt.IsZero() && c.CreatedAt.Before(r.CodexReplan) {
		return false
	}
	if c.State == models.WorkItemStatePassed && !r.Start.IsZero() && !c.PassedAt.IsZero() && c.PassedAt.Before(r.Start) {
		return false
	}
	return true
}

// rowQuerier is the read surface SupersededTx needs (pgx.Tx, *pgxpool.Pool).
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SupersededTx is Superseded read from the database.
func SupersededTx(ctx context.Context, q rowQuerier, id uuid.UUID) (bool, error) {
	var superseded bool
	err := q.QueryRow(ctx, `
		SELECT w.state = 'blocked' AND COALESCE((
		         SELECT t.reason FROM work_item_transitions t
		          WHERE t.work_item_id = w.id AND t.to_state = 'blocked'
		          ORDER BY t."timestamp" DESC LIMIT 1), '') LIKE $2 || '%'
		  FROM work_items w WHERE w.id = $1`, id, CodexChangedPrefix).Scan(&superseded)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("superseded check for %s: %w", id, err)
	}
	return superseded, nil
}
