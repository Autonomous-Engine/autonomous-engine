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

// qaWriter commits the QA agent's state writes together with the events
// that announce them (M1-04): qa.result, qa.bugs and work.blocked go
// through the transactional outbox, never straight to Kafka. The outbox
// aggregate is the work item under test; topic, key and payload are
// unchanged from the direct publishes they replace.
type qaWriter struct {
	// db begins the transactions (*pgxpool.Pool in production).
	db workgraph.TxBeginner
	wg *workgraph.WorkGraph
}

// block transitions the work item to blocked (enqueueing work.blocked) and
// fails the assignment in one transaction. If that transaction cannot
// commit — most likely an item that can no longer be blocked — the
// assignment is still failed on its own, as it was when the block was a
// separate, logged write. The error is the block transaction's.
func (w qaWriter) block(ctx context.Context, workID, assignmentID uuid.UUID, reason string) error {
	err := workgraph.WithTx(ctx, w.db, func(tx pgx.Tx) error {
		if err := guards.BlockWorkItemTx(ctx, tx, workID, workID, agentID, reason); err != nil {
			return err
		}
		return w.wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusFailed)
	})
	if err == nil || idempotency.IsDuplicate(err) {
		// M1-05: a duplicate qa.ready was already processed; nothing to do.
		return err
	}
	log.Printf("qa-agent: ERROR block work item %s failed, nothing was committed: %v", workID, err)
	if cerr := workgraph.WithTx(ctx, w.db, func(tx pgx.Tx) error {
		return w.wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusFailed)
	}); cerr != nil {
		log.Printf("qa-agent: ERROR fail assignment %s: %v", assignmentID, cerr)
	}
	return err
}

// commitResult writes the QA report artifact, the verdict event (qa.result
// on a pass, qa.bugs on a fail) and the assignment completion in one
// transaction. On error nothing was committed; the caller must block the
// item rather than carry on.
func (w qaWriter) commitResult(ctx context.Context, workID, assignmentID uuid.UUID, artifact *models.WorkItemArtifact, pass *qaResultMessage, bugs *qaBugsMessage) error {
	return workgraph.WithTx(ctx, w.db, func(tx pgx.Tx) error {
		if err := w.wg.AddArtifactTx(ctx, tx, artifact); err != nil {
			return fmt.Errorf("save qa report artifact: %w", err)
		}
		switch {
		case pass != nil:
			if err := outbox.EnqueueJSON(ctx, tx, workID, bus.TopicQAResult, workID.String(), pass); err != nil {
				return err
			}
		case bugs != nil:
			if err := outbox.EnqueueJSON(ctx, tx, workID, bus.TopicQABugs, workID.String(), bugs); err != nil {
				return err
			}
		default:
			return fmt.Errorf("qa outcome has neither a result nor bugs")
		}
		return w.wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusCompleted)
	})
}

// recordResult commits the QA outcome and, when that fails, blocks the
// work item instead: before the outbox a failed write was logged and the
// event published anyway; now the event is gone with it, so the item must
// not sit as if tested.
func (w qaWriter) recordResult(ctx context.Context, workID, assignmentID uuid.UUID, artifact *models.WorkItemArtifact, pass *qaResultMessage, bugs *qaBugsMessage) bool {
	err := w.commitResult(ctx, workID, assignmentID, artifact, pass, bugs)
	if err == nil {
		return true
	}
	if idempotency.IsDuplicate(err) {
		// M1-05: the first delivery of this qa.ready recorded the verdict.
		log.Printf("qa-agent: work_id=%s verdict not recorded — duplicate delivery of an already-processed qa.ready", workID)
		return false
	}
	log.Printf("qa-agent: ERROR recording QA result for work_id=%s failed, nothing was committed: %v", workID, err)
	verdict := "FAIL"
	if pass != nil {
		verdict = "PASS"
	}
	w.block(ctx, workID, assignmentID, fmt.Sprintf( //nolint:errcheck // logged inside
		"QA completed (%s) but its outcome could not be recorded: %v. Manual verification required.", verdict, err))
	return false
}
