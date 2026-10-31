package stage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Outcome is everything one verification commits, in one transaction with
// the delivery's idempotency guard (M1-04, M1-05): the verdict artifact
// (when a new verdict was computed), the verification_results row, the
// verification.complete outbox event, the parent's block (decision error)
// and the stage's assignment.
type Outcome struct {
	Message Message
	Result  Result
	// Artifact is the new verdict artifact; nil when the decision has none
	// (not_run, error) or reuses a stored verdict.
	Artifact *models.WorkItemArtifact
	// Block, when set, blocks the parent with this reason (work.blocked:
	// status-orchestrator raises the escalation prompt).
	Block string
	// AssignmentID is an already-recorded agent assignment to complete
	// (failed when Block is set); uuid.Nil when none.
	AssignmentID uuid.UUID
	// Assignment, when set, is the stage's agent assignment: it is
	// inserted and completed in this transaction, so a rolled-back commit
	// leaves no dangling active assignment behind.
	Assignment *models.AgentAssignment
}

// assignmentCompleter is the slice of *workgraph.WorkGraph Commit needs.
type assignmentCompleter interface {
	AddArtifactTx(ctx context.Context, tx pgx.Tx, artifact *models.WorkItemArtifact) error
	AssignAgentTx(ctx context.Context, tx pgx.Tx, assignment *models.AgentAssignment) error
	CompleteAssignmentTx(ctx context.Context, tx pgx.Tx, assignmentID uuid.UUID, status models.AssignmentStatus) error
}

// lockStateSQL locks the parent before a block decision.
const lockStateSQL = `SELECT state FROM work_items WHERE id = $1 FOR UPDATE`

// unblockable are the parent states a stage failure leaves alone: there is
// no edge to blocked and nothing left to protect (the item ended, or a
// person already decided it). The result and the event still commit.
func unblockable(s models.WorkItemState) bool {
	switch s {
	case models.WorkItemStateCancelled, models.WorkItemStateRejected, models.WorkItemStatePassed, models.WorkItemStateReleased:
		return true
	}
	return false
}

// Commit writes o in one transaction on db (idempotency.Wrap of the pool
// in production, so the delivery's guard row commits with it). On success
// o.Message and o.Result carry the committed ids.
func Commit(ctx context.Context, db workgraph.TxBeginner, wg assignmentCompleter, agentID string, o *Outcome) error {
	parent, err := uuid.Parse(o.Message.WorkID)
	if err != nil {
		return fmt.Errorf("verification.complete: work_id %q: %w", o.Message.WorkID, err)
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		res := o.Result
		msg := o.Message
		if o.Artifact != nil {
			art := *o.Artifact
			if err := wg.AddArtifactTx(ctx, tx, &art); err != nil {
				return fmt.Errorf("save verdict artifact: %w", err)
			}
			o.Artifact.ID = art.ID
			id := art.ID
			res.VerdictArtifactID = &id
		}
		if res.VerdictArtifactID != nil {
			msg.VerdictArtifactID = res.VerdictArtifactID.String()
		}
		if err := InsertResultTx(ctx, tx, &res); err != nil {
			return err
		}
		msg.ResultID = res.ID.String()
		if msg.Timestamp == "" {
			msg.Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		if err := outbox.EnqueueJSON(ctx, tx, parent, bus.TopicVerificationComplete, parent.String(), msg); err != nil {
			return err
		}
		if o.Block != "" {
			var state models.WorkItemState
			if err := tx.QueryRow(ctx, lockStateSQL, parent).Scan(&state); err != nil {
				return fmt.Errorf("lock parent %s: %w", parent, err)
			}
			// Codex coherence Part 1 review: a subtask a codex re-plan
			// superseded belongs to a plan that no longer exists; its
			// failure must not block the parent being planned again.
			superseded := false
			if task, perr := uuid.Parse(o.Message.TaskID); perr == nil && !unblockable(state) {
				s, err := workgraph.SupersededTx(ctx, tx, task)
				if err != nil {
					return err
				}
				superseded = s
			}
			switch {
			case unblockable(state):
				log.Printf("verification stage: work_id=%s is %s — not blocking it for the stage failure (%s); the result and the event are recorded", parent, state, o.Block)
			case superseded:
				log.Printf("verification stage: work_id=%s task=%s was superseded by a codex re-plan — not blocking the parent for the stage failure (%s); the result and the event are recorded", parent, o.Message.TaskID, o.Block)
			default:
				if err := guards.BlockWorkItemTx(ctx, tx, parent, parent, agentID, o.Block); err != nil {
					return err
				}
			}
		}
		status := models.AssignmentStatusCompleted
		if o.Block != "" {
			status = models.AssignmentStatusFailed
		}
		assignmentID := o.AssignmentID
		if o.Assignment != nil {
			a := *o.Assignment
			if err := wg.AssignAgentTx(ctx, tx, &a); err != nil {
				return err
			}
			assignmentID = a.ID
		}
		if assignmentID != uuid.Nil {
			if err := wg.CompleteAssignmentTx(ctx, tx, assignmentID, status); err != nil {
				return err
			}
		}
		o.Result = res
		o.Message = msg
		return nil
	})
}

// MaxCommitAttempts is how many times a commit is run in process before the
// parent is blocked (F2).
const MaxCommitAttempts = 3

// CommitRetryBackoff is the wait before the 2nd and 3rd attempt; tests
// shorten it.
var CommitRetryBackoff = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

// PermanentError reports an error a retry cannot fix: an invalid
// state-machine transition, a missing row, or a PostgreSQL data, integrity
// constraint or schema error (classes 22, 23, 42).
func PermanentError(err error) bool {
	if errors.Is(err, workgraph.ErrInvalidTransition) || errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "22"), strings.HasPrefix(pgErr.Code, "23"), strings.HasPrefix(pgErr.Code, "42"):
			return true
		}
	}
	return false
}

// Retry runs fn up to MaxCommitAttempts times (F2: the bus does not
// redeliver a failed message in process). It stops on success, on a
// duplicate delivery, on a permanent error and when ctx is done, and
// returns the last error. Each failed attempt rolled back its own
// transaction (with the guard row), so running it again is safe.
func Retry(ctx context.Context, what string, fn func() error) error {
	var err error
	for attempt := 1; attempt <= MaxCommitAttempts; attempt++ {
		if err = fn(); err == nil || idempotency.IsDuplicate(err) || PermanentError(err) {
			return err
		}
		if attempt == MaxCommitAttempts {
			break
		}
		wait := CommitRetryBackoff[len(CommitRetryBackoff)-1]
		if attempt-1 < len(CommitRetryBackoff) {
			wait = CommitRetryBackoff[attempt-1]
		}
		log.Printf("verification stage: %s failed (attempt %d/%d): %v — retrying in %s", what, attempt, MaxCommitAttempts, err, wait)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
	return err
}

// Record commits o (Commit), retrying a transient failure in process. A
// duplicate delivery commits nothing and is not a failure. When the commit
// still fails the parent is blocked (with work.blocked, so a person gets the
// escalation prompt) naming what was lost; the returned error is non-nil
// only when that block failed too, and the caller must then not acknowledge
// the delivery (F2).
func Record(ctx context.Context, db workgraph.TxBeginner, wg assignmentCompleter, agentID string, o *Outcome) error {
	err := Retry(ctx, "verification commit", func() error { return Commit(ctx, db, wg, agentID, o) })
	if err == nil {
		return nil
	}
	if idempotency.IsDuplicate(err) {
		log.Printf("verification stage: work_id=%s task=%s duplicate delivery — nothing recorded", o.Message.WorkID, o.Message.TaskID)
		return nil
	}
	reason := fmt.Sprintf("verification of subtask %s (head %s) decided %s but could not be recorded: %v", o.Message.TaskID, orNone(o.Message.HeadSHA), o.Message.Decision, err)
	log.Printf("verification stage: ERROR work_id=%s: %s — blocking", o.Message.WorkID, reason)
	return BlockAfterFailure(ctx, db, agentID, o.Message.WorkID, reason)
}

// BlockAfterFailure blocks the parent with reason in a transaction of its
// own (on the guarded beginner it carries the delivery's guard row unless an
// earlier commit already recorded it). The error is the block's: non-nil
// means nothing was recorded and the delivery must not be acknowledged.
func BlockAfterFailure(ctx context.Context, db workgraph.TxBeginner, agentID, workID, reason string) error {
	parent, err := uuid.Parse(workID)
	if err != nil {
		return fmt.Errorf("block: work_id %q: %w", workID, err)
	}
	err = Retry(ctx, "block commit", func() error {
		return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
			return guards.BlockWorkItemTx(ctx, tx, parent, parent, agentID, reason)
		})
	})
	if err == nil || idempotency.IsDuplicate(err) {
		return nil
	}
	log.Printf("verification stage: ERROR could not block work_id=%s (%s): %v — delivery not acknowledged", workID, reason, err)
	return fmt.Errorf("block work_id=%s: %w", workID, err)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
