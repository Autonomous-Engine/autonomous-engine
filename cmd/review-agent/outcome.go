package main

import (
	"context"
	"fmt"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// reviewWriter commits the review agent's state writes together with the
// events that announce them (M1-04): review.complete and work.blocked go
// through the transactional outbox, never straight to Kafka. The outbox
// aggregate is the reviewed work item (pr.ready is per parent item); topic,
// key and payload are unchanged from the direct publishes they replace.
type reviewWriter struct {
	// db begins the transactions (*pgxpool.Pool in production).
	db workgraph.TxBeginner
	wg *workgraph.WorkGraph
	// pending are artifacts committed with whichever outcome ends this
	// review (commitReview or block): the verifier's verdict when it
	// approved and the second-pass review decides (M3-07).
	pending []*models.WorkItemArtifact
}

// reviewDecision is the outcome of one review.
type reviewDecision struct {
	// approve is the agent's verdict; false means changes requested.
	approve bool
	// humanReviewRequired routes an approval to human_review instead of
	// review.complete.
	humanReviewRequired bool
	feedback            string
	// escalate sends the item to human_review whatever approve says: the
	// independent verifier could not verify it (M3-07). reason is the
	// transition reason.
	escalate bool
	reason   string
}

// block transitions the work item to blocked (enqueueing work.blocked) and
// fails the assignment in one transaction. If that transaction cannot
// commit — most likely an item that can no longer be blocked — the
// assignment is still failed on its own, as it was when the block was a
// separate, logged write. The error is the block transaction's.
func (w reviewWriter) block(ctx context.Context, workID, assignmentID uuid.UUID, reason string) error {
	err := workgraph.WithTx(ctx, w.db, func(tx pgx.Tx) error {
		if err := w.addPendingTx(ctx, tx); err != nil {
			return err
		}
		if err := guards.BlockWorkItemTx(ctx, tx, workID, workID, agentID, reason); err != nil {
			return err
		}
		return w.wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusFailed)
	})
	if err == nil || idempotency.IsDuplicate(err) {
		// M1-05: a duplicate pr.ready was already processed; nothing to do.
		return err
	}
	log.Printf("review-agent: ERROR block work item %s failed, nothing was committed: %v", workID, err)
	if cerr := workgraph.WithTx(ctx, w.db, func(tx pgx.Tx) error {
		return w.wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusFailed)
	}); cerr != nil {
		log.Printf("review-agent: ERROR fail assignment %s: %v", assignmentID, cerr)
	}
	return err
}

// commitReview writes the review artifact, then either the human_review
// transition (approved, human review required) or the review.complete event
// (approved without human review, or changes requested), then completes the
// assignment — all in one transaction. On error nothing was committed; the
// caller must block the item rather than carry on.
func (w reviewWriter) commitReview(ctx context.Context, workID, assignmentID uuid.UUID, artifact *models.WorkItemArtifact, d reviewDecision) error {
	return workgraph.WithTx(ctx, w.db, func(tx pgx.Tx) error {
		if err := w.addPendingTx(ctx, tx); err != nil {
			return err
		}
		if err := w.wg.AddArtifactTx(ctx, tx, artifact); err != nil {
			return fmt.Errorf("save review artifact: %w", err)
		}
		switch {
		case d.escalate:
			reason := d.reason
			if reason == "" {
				reason = "independent verifier escalated — human review required"
			}
			if _, err := w.wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStateHumanReview, agentID, reason); err != nil {
				return fmt.Errorf("transition to human_review: %w", err)
			}
		case d.approve && d.humanReviewRequired:
			// The dashboard queries work_items WHERE state='human_review'
			// directly; the former human-review.queue publish had no
			// subscriber and is omitted. The PR description is already in
			// the review artifact.
			if _, err := w.wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStateHumanReview, agentID, "agent review approved — awaiting human confirmation"); err != nil {
				return fmt.Errorf("transition to human_review: %w", err)
			}
		case d.approve:
			msg := reviewCompleteMessage{WorkID: workID.String(), Decision: "approved", HumanReviewRequired: false}
			if err := outbox.EnqueueJSON(ctx, tx, workID, bus.TopicReviewComplete, workID.String(), msg); err != nil {
				return err
			}
		default:
			msg := reviewCompleteMessage{WorkID: workID.String(), Decision: "changes_requested", Feedback: d.feedback}
			if err := outbox.EnqueueJSON(ctx, tx, workID, bus.TopicReviewComplete, workID.String(), msg); err != nil {
				return err
			}
		}
		return w.wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusCompleted)
	})
}

// recordReview commits the review and, when that fails, blocks the work
// item instead: before the outbox a failed write was logged and the event
// published anyway; now the event is gone with it, so the item must not sit
// as if reviewed.
func (w reviewWriter) recordReview(ctx context.Context, workID, assignmentID uuid.UUID, artifact *models.WorkItemArtifact, d reviewDecision) bool {
	err := w.commitReview(ctx, workID, assignmentID, artifact, d)
	if err == nil {
		return true
	}
	if idempotency.IsDuplicate(err) {
		// M1-05: the first delivery of this pr.ready recorded the review.
		log.Printf("review-agent: work_id=%s review not recorded — duplicate delivery of an already-processed pr.ready", workID)
		return false
	}
	log.Printf("review-agent: ERROR recording review for work_id=%s failed, nothing was committed: %v", workID, err)
	verdict := "changes requested"
	switch {
	case d.escalate:
		verdict = "escalated to human review"
	case d.approve:
		verdict = "approved"
	}
	w.block(ctx, workID, assignmentID, fmt.Sprintf( //nolint:errcheck // logged inside
		"Review completed (%s) but its outcome could not be recorded: %v. A human must review this PR manually.", verdict, err))
	return false
}

// addPendingTx writes the pending artifacts in tx. Copies are written so a
// rolled-back attempt leaves the originals unchanged for a fallback block.
func (w reviewWriter) addPendingTx(ctx context.Context, tx pgx.Tx) error {
	for _, a := range w.pending {
		if a == nil {
			continue
		}
		cp := *a
		if err := w.wg.AddArtifactTx(ctx, tx, &cp); err != nil {
			return fmt.Errorf("save verifier artifact: %w", err)
		}
	}
	return nil
}
