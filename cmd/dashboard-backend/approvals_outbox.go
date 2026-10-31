package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ─── Manual approvals through the transactional outbox (M1-04b) ─────────────
//
// Every manual approval and retry the dashboard performs moves a work item and
// announces it on Kafka (prd.ready, qa.ready, intake, implementation.tasks,
// pr.ready). Each now commits its state write and its event in one
// transaction (the transactional outbox);
// main's outbox dispatcher delivers the event after the commit. Before, the
// transition committed first and a failed publish was logged and ignored,
// leaving the item in the new state with nobody told to work on it.
//
// Topic, key and payload are unchanged from the direct publishes; the
// payload gains the additive event_id. The aggregate is the parent work item
// when the item has one, otherwise the item itself.

// approvalGraph is the work-graph surface the approval paths use. NewHandlers
// wires it to the WorkGraph; tests supply fakes.
type approvalGraph interface {
	GetWorkItem(ctx context.Context, id uuid.UUID) (*models.WorkItem, error)
	GetArtifacts(ctx context.Context, workItemID uuid.UUID, artifactType models.ArtifactType) ([]*models.WorkItemArtifact, error)
	TransitionStateTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) (models.WorkItemState, error)
	TransitionFromAwaitingUserToStateTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) error
}

// workGraphApprovals adapts *workgraph.WorkGraph to approvalGraph.
type workGraphApprovals struct{ *workgraph.WorkGraph }

func (w workGraphApprovals) TransitionFromAwaitingUserToStateTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, toState models.WorkItemState, agentID, reason string) error {
	return workgraph.TransitionFromAwaitingUserToStateTx(ctx, tx, itemID, toState, agentID, reason)
}

// aggregateFor is the outbox aggregate for an event about id: the parent work
// item when item has one, otherwise id itself.
func aggregateFor(id uuid.UUID, item *models.WorkItem) uuid.UUID {
	if item != nil && item.ParentID != nil && *item.ParentID != uuid.Nil {
		return *item.ParentID
	}
	return id
}

// transitionFailed marks an error from the state transition itself (invalid
// transition, missing item), as opposed to the enqueue or the commit. HTTP
// handlers answer the former with 400, as they did before the conversion,
// and the latter with 500.
type transitionFailed struct{ err error }

func (e *transitionFailed) Error() string { return e.err.Error() }
func (e *transitionFailed) Unwrap() error { return e.err }

// transitionAndEnqueue transitions id to `to` and enqueues one event about it
// in a single transaction. A transition error comes back as
// *transitionFailed; nothing is enqueued and nothing commits.
func (h *Handlers) transitionAndEnqueue(ctx context.Context, id uuid.UUID, item *models.WorkItem, to models.WorkItemState, agentID, reason, topic, key string, payload any) error {
	return workgraph.WithTx(ctx, h.txdb, func(tx pgx.Tx) error {
		if _, err := h.approvals.TransitionStateTx(ctx, tx, id, to, agentID, reason); err != nil {
			return &transitionFailed{err: err}
		}
		return outbox.EnqueueJSON(ctx, tx, aggregateFor(id, item), topic, key, payload)
	})
}

// enqueueSubtaskRetry enqueues the M0-07 retry builder's message:
// implementation.tasks keyed by the subtask, ordered under its parent.
func enqueueSubtaskRetry(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error {
	return outbox.EnqueueJSON(ctx, tx, aggregateFor(item.ID, item), bus.TopicImplementationTasks, item.ID.String(), buildSubtaskRetryMessage(item))
}

// retrySubtask moves a blocked or failed subtask back to implementation and
// enqueues its implementation.tasks retry in one transaction.
func (h *Handlers) retrySubtask(ctx context.Context, item *models.WorkItem, agentID, reason string) error {
	return workgraph.WithTx(ctx, h.txdb, func(tx pgx.Tx) error {
		if _, err := h.approvals.TransitionStateTx(ctx, tx, item.ID, models.WorkItemStateImplementation, agentID, reason); err != nil {
			return &transitionFailed{err: err}
		}
		return enqueueSubtaskRetry(ctx, tx, item)
	})
}

// buildQAReadyMessage is the qa.ready payload for workID with the reviewed
// branch and its verified head SHA. Both manual approval paths
// (ApproveWorkItem and HandleHumanReviewPromptAnswer) use it so they cannot
// drift apart.
func buildQAReadyMessage(workID uuid.UUID, item *models.WorkItem, artifacts []*models.WorkItemArtifact) qaReadyMessage {
	msg := qaReadyMessage{WorkID: workID.String()}
	if item != nil {
		msg.Title = item.Title
		msg.AcceptanceCriteria = item.Description
	}
	msg.Branch, msg.HeadSHA = reviewedHead(artifacts)
	return msg
}

// approveToQA is the shared human_review → qa approval: the transition and
// qa.ready (with the reviewed branch and verified head SHA) in one
// transaction. The item and its review artifacts are read first; reads are
// not part of the transaction.
func (h *Handlers) approveToQA(ctx context.Context, id uuid.UUID, agentID, reason string) error {
	item, _ := h.approvals.GetWorkItem(ctx, id)
	artifacts, _ := h.approvals.GetArtifacts(ctx, id, models.ArtifactTypeReviewComments)
	msg := buildQAReadyMessage(id, item, artifacts)
	return h.transitionAndEnqueue(ctx, id, item, models.WorkItemStateQA, agentID, reason, bus.TopicQAReady, id.String(), msg)
}

// enqueueGatedPRReady enqueues pr.ready for an approved gated change,
// forwarding the stored head SHA and the partial-verification marker.
// pc.WorkItemID is the parent work item pr-coordinator recorded, so it is
// both the key (unchanged) and the aggregate.
func enqueueGatedPRReady(ctx context.Context, tx pgx.Tx, pc pendingPRChange, productID string, prNumber int, prURL string, now time.Time) error {
	aggregate, err := uuid.Parse(pc.WorkItemID)
	if err != nil {
		return fmt.Errorf("pending change %s: work item id %q: %w", pc.ID, pc.WorkItemID, err)
	}
	msg := gatedPRReadyMessage{
		WorkID:            pc.WorkItemID,
		ProductID:         productID,
		RepositoryID:      pc.RepositoryID,
		PRNumber:          prNumber,
		PRURL:             prURL,
		PRDescription:     pc.PRDescription,
		Branch:            pc.Branch,
		FilesChanged:      pc.FilesChanged,
		Timestamp:         now.UTC().Format(time.RFC3339),
		HeadSHA:           pc.HeadSHA,
		PartiallyVerified: pc.partiallyVerified(),
	}
	return outbox.EnqueueJSON(ctx, tx, aggregate, bus.TopicPRReady, pc.WorkItemID, msg)
}

// commitGatedPRReady enqueues pr.ready for a change whose PR github-service
// has just opened. The GitHub call cannot be in a transaction, and the
// change was already marked approved before it, so this is the event's own
// short transaction (spec checklist step 1). If it fails the PR exists
// without its pr.ready; callers log that loudly with the PR URL and fail the
// request (M1-05 idempotency makes a retry safe).
func commitGatedPRReady(ctx context.Context, db workgraph.TxBeginner, pc pendingPRChange, productID string, prNumber int, prURL string, now time.Time) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return enqueueGatedPRReady(ctx, tx, pc, productID, prNumber, prURL, now)
	})
}

// The statements update-and-retry runs on the parent in its transaction.
// blocked → intake is not in the state machine, so the state is forced and
// the transition recorded by hand, as before the conversion.
const (
	updateDescriptionSQL      = `UPDATE work_items SET description = $1, updated_at = NOW() WHERE id = $2`
	forceIntakeSQL            = `UPDATE work_items SET state = 'intake', updated_at = NOW() WHERE id = $1`
	insertIntakeTransitionSQL = `INSERT INTO work_item_transitions (work_item_id, from_state, to_state, agent_id, reason)
		 VALUES ($1, $2, 'intake', $3, 'description updated — rerunning PRD flow')`
)

// commitUpdateAndRetry persists the new description, resets the item to
// intake with its transition row, and enqueues intake, all in one
// transaction. The payload shape must match intakeMessage in
// cmd/product-agent.
func commitUpdateAndRetry(ctx context.Context, db workgraph.TxBeginner, item *models.WorkItem, description, userID string) error {
	productID := ""
	if item.ProductID != nil {
		productID = item.ProductID.String()
	}
	payload := map[string]interface{}{
		"work_id":     item.ID.String(),
		"product_id":  productID,
		"description": description,
		"source":      "dashboard-retry",
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, updateDescriptionSQL, description, item.ID); err != nil {
			return fmt.Errorf("update description: %w", err)
		}
		if _, err := tx.Exec(ctx, forceIntakeSQL, item.ID); err != nil {
			return fmt.Errorf("reset to intake: %w", err)
		}
		if _, err := tx.Exec(ctx, insertIntakeTransitionSQL, item.ID, item.State, userID); err != nil {
			return fmt.Errorf("record transition: %w", err)
		}
		return outbox.EnqueueJSON(ctx, tx, aggregateFor(item.ID, item), bus.TopicIntake, item.ID.String(), payload)
	})
}

// blockedRetryPRDReady builds the prd.ready a blocked-prompt retry of a
// non-subtask item enqueues with its move to research: the payload
// research-coordinator reads (prdReadyMessage), rebuilt from the newest PRD
// artifact like RetryWorkItem's, with the answer text as user_guidance. It
// fails when there is no usable PRD, because research-coordinator blocks a
// prd.ready with empty prd_content.
func blockedRetryPRDReady(ctx context.Context, g approvalGraph, item *models.WorkItem, answerText string) (map[string]interface{}, error) {
	artifacts, _ := g.GetArtifacts(ctx, item.ID, models.ArtifactTypePRD)
	if len(artifacts) == 0 {
		return nil, fmt.Errorf("cannot retry into research: work item %s has no PRD artifact; use Retry or Update & Retry on the work item to rerun from intake", item.ID)
	}
	var prd struct {
		PRD      string `json:"prd"`
		WorkType string `json:"work_type"`
	}
	if err := json.Unmarshal(artifacts[len(artifacts)-1].Content, &prd); err != nil {
		return nil, fmt.Errorf("corrupted PRD artifact: %w", err)
	}
	if strings.TrimSpace(prd.PRD) == "" {
		return nil, fmt.Errorf("cannot retry into research: work item %s has an empty PRD; use Retry or Update & Retry on the work item to rerun from intake", item.ID)
	}
	productID := ""
	if item.ProductID != nil {
		productID = item.ProductID.String()
	}
	return map[string]interface{}{
		"work_id":       item.ID.String(),
		"product_id":    productID,
		"work_type":     prd.WorkType,
		"prd_content":   prd.PRD,
		"user_guidance": strings.TrimSpace(answerText),
		"retry":         true,
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// writeTransitionError answers a failed approval: 400 with the transition's
// message when the transition itself was refused (as before the conversion),
// 500 when the event could not be recorded or the commit failed.
func writeTransitionError(w http.ResponseWriter, err error) {
	var tf *transitionFailed
	if errors.As(err, &tf) {
		writeError(w, http.StatusBadRequest, tf.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "approval not recorded: "+err.Error())
}
