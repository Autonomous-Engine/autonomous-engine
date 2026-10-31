package idempotency

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Validation of expected current state before an external side effect
// (roadmap 1.2). A deduplicated event can still be stale: an
// implementation.tasks for a subtask an operator has since cancelled, a
// qa.ready for an item that was blocked meanwhile. The three handlers whose
// side effects leave the database — implementation-agent (worktree, branch,
// LLM), pr-coordinator (GitHub PR), qa-agent (checkout, test run) — check the
// work item's state first and block it with a reason instead of acting.

// ItemState is a work item's state as read under a lock.
type ItemState struct {
	ID       uuid.UUID
	Found    bool
	State    models.WorkItemState
	Previous models.WorkItemState // previous_state; set while awaiting_user
}

// Effective is the state the item is parked in: previous_state while it is
// awaiting_user (a prompt pauses an item without changing what it is
// waiting in), its state otherwise.
func (s ItemState) Effective() models.WorkItemState {
	if s.State == models.WorkItemStateAwaitingUser && s.Previous != "" {
		return s.Previous
	}
	return s.State
}

func (s ItemState) describe() string {
	if !s.Found {
		return "missing"
	}
	if s.State == models.WorkItemStateAwaitingUser && s.Previous != "" {
		return fmt.Sprintf("%s (awaiting_user)", s.Previous)
	}
	return string(s.State)
}

const (
	lockItemStateSQL  = `SELECT state::text, COALESCE(previous_state, '') FROM work_items WHERE id = $1 FOR UPDATE`
	shareItemStateSQL = `SELECT state::text, COALESCE(previous_state, '') FROM work_items WHERE id = $1 FOR SHARE`
)

func readItemState(ctx context.Context, tx pgx.Tx, sql string, id uuid.UUID) (ItemState, error) {
	s := ItemState{ID: id}
	var state, previous string
	err := tx.QueryRow(ctx, sql, id).Scan(&state, &previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("read state of work item %s: %w", id, err)
	}
	s.Found = true
	s.State = models.WorkItemState(state)
	s.Previous = models.WorkItemState(previous)
	return s, nil
}

// Precondition is the state a handler requires before an external side
// effect.
type Precondition struct {
	// Action names the side effect for the block reason ("create a
	// worktree", "open a pull request", "check out the branch under test").
	Action string
	// AgentID is recorded on the block transition.
	AgentID string
	// AggregateID orders the work.blocked event (the parent work item, or
	// the item itself when it has none).
	AggregateID uuid.UUID
	// WorkID is the item whose state is checked and which is blocked on a
	// mismatch.
	WorkID uuid.UUID
	// Expected lists the acceptable effective states of WorkID (an
	// allow-list). Exactly one of Expected and NotIn must be set;
	// CheckPrecondition rejects a Precondition with neither or both.
	Expected []models.WorkItemState
	// NotIn lists effective states of WorkID that are refused (a
	// deny-list), for a handler racing a writer that may legitimately move
	// the item to states an allow-list would have to enumerate. A missing
	// item is a mismatch under either rule.
	NotIn []models.WorkItemState
	// ParentID, when not uuid.Nil, is read too and must not be in
	// ParentNotIn.
	ParentID    uuid.UUID
	ParentNotIn []models.WorkItemState
}

// validate rejects a Precondition whose state rule is unset or ambiguous.
func (p Precondition) validate() error {
	if p.WorkID == uuid.Nil {
		return errors.New("check precondition: work id is required")
	}
	switch {
	case len(p.Expected) == 0 && len(p.NotIn) == 0:
		return errors.New("check precondition: no state rule: set Expected (allow-list) or NotIn (deny-list)")
	case len(p.Expected) > 0 && len(p.NotIn) > 0:
		return errors.New("check precondition: ambiguous state rule: set Expected (allow-list) or NotIn (deny-list), not both")
	}
	return nil
}

// PreconditionResult is the outcome of CheckPrecondition.
type PreconditionResult struct {
	// OK: the state matched and nothing was written.
	OK bool
	// Reason names the observed and expected states when OK is false.
	Reason string
	// Blocked: WorkID was transitioned to blocked with Reason (and
	// work.blocked enqueued). False on a mismatch whose item cannot be
	// blocked: missing, terminal (passed, cancelled, …), already blocked, or
	// paused in awaiting_user (blocking would strand its pending prompt).
	Blocked bool
}

// CheckPrecondition reads the work item (and its parent) in one transaction
// on db, which should be the handler's Wrap'd beginner so the delivery's
// guard row is that transaction's first statement.
//
// On a match the transaction is rolled back: nothing is written and the
// guard stays pending for the handler's own state-change transaction. On a
// mismatch the item is blocked with a reason naming the observed and the
// expected state (guards.BlockWorkItemTx) and the transaction commits with
// the guard row, so a redelivery of the stale event is a duplicate. An
// ErrDuplicate from Begin is returned as is.
//
// A misconfigured Precondition fails loudly, before any transaction is
// opened: exactly one of Expected (allow-list) and NotIn (deny-list) must be
// set. With neither, every state would pass; with both, one rule would be
// silently ignored.
func CheckPrecondition(ctx context.Context, db TxBeginner, p Precondition) (PreconditionResult, error) {
	if err := p.validate(); err != nil {
		return PreconditionResult{}, err
	}
	if db == nil {
		return PreconditionResult{}, errors.New("check precondition: nil database")
	}
	aggregate := p.AggregateID
	if aggregate == uuid.Nil {
		aggregate = p.WorkID
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return PreconditionResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	item, err := readItemState(ctx, tx, lockItemStateSQL, p.WorkID)
	if err != nil {
		return PreconditionResult{}, err
	}
	var reasons []string
	// validate guarantees exactly one rule is set.
	switch {
	case len(p.Expected) > 0:
		if !item.Found || !containsState(p.Expected, item.Effective()) {
			reasons = append(reasons, fmt.Sprintf("work item %s is %s, expected %s",
				p.WorkID, item.describe(), joinStates(p.Expected)))
		}
	case !item.Found:
		reasons = append(reasons, fmt.Sprintf("work item %s is missing", p.WorkID))
	case containsState(p.NotIn, item.Effective()):
		reasons = append(reasons, fmt.Sprintf("work item %s is %s, expected not %s",
			p.WorkID, item.describe(), joinStates(p.NotIn)))
	}
	if p.ParentID != uuid.Nil {
		parent, err := readItemState(ctx, tx, shareItemStateSQL, p.ParentID)
		if err != nil {
			return PreconditionResult{}, err
		}
		if parent.Found && containsState(p.ParentNotIn, parent.Effective()) {
			reasons = append(reasons, fmt.Sprintf("parent work item %s is %s, expected not %s",
				p.ParentID, parent.describe(), joinStates(p.ParentNotIn)))
		}
	}
	if len(reasons) == 0 {
		// Rolled back by the deferred call: the guard row goes with it.
		return PreconditionResult{OK: true}, nil
	}

	res := PreconditionResult{Reason: fmt.Sprintf("refusing to %s: %s", p.Action, strings.Join(reasons, "; "))}
	if canBlock(item) {
		if err := guards.BlockWorkItemTx(ctx, tx, aggregate, p.WorkID, p.AgentID, res.Reason); err != nil {
			return PreconditionResult{}, err
		}
		res.Blocked = true
	}
	committed = true
	if err := tx.Commit(ctx); err != nil {
		return PreconditionResult{}, fmt.Errorf("check precondition: commit: %w", err)
	}
	return res, nil
}

// canBlock reports whether a mismatched item should be transitioned to
// blocked: it exists, is not already blocked, is not paused on a prompt, and
// the state machine has an edge to blocked from where it is.
func canBlock(item ItemState) bool {
	if !item.Found || item.State == models.WorkItemStateAwaitingUser || item.State == models.WorkItemStateBlocked {
		return false
	}
	return workgraph.IsValidTransition(item.State, models.WorkItemStateBlocked)
}

func containsState(states []models.WorkItemState, s models.WorkItemState) bool {
	for _, x := range states {
		if x == s {
			return true
		}
	}
	return false
}

func joinStates(states []models.WorkItemState) string {
	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = string(s)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "one of " + strings.Join(parts, ", ")
}
