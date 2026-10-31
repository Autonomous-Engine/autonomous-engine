package main

// Codex coherence Part 3: the answers to a refusal's escalation (a stage
// refused the item on an ungrounded codex, guards.RefuseUngroundedTx; the
// prompt offers Re-index / Retry / Cancel).
//
//   - Re-index records the pending re-run (codex_grounding_refusals.
//     reindex_requested_at) and asks knowledge-indexer for a full index of
//     the product on the branch the guard fell back to. Accepted: the post
//     is recorded (index_posted_at). Busy (409, the indexer runs one index
//     for every product at a time): the request stays pending and
//     status-orchestrator re-posts it. Failed: the request is withdrawn and
//     the escalation raised again. status-orchestrator re-runs the item when
//     codex.indexed for its product and branch reports the codex grounded.
//   - Retry re-runs the refused stage now from the recorded message (the
//     stage checks the codex again); a stale refusal (the item's current
//     block is another one) is dropped and the generic Retry runs.
//   - Cancel, and every other retry path, drops the refusal.

import (
	"context"
	"fmt"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/codexindex"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// codexRefusalDB reads and writes codex_grounding_refusals
// (*pgxpool.Pool satisfies it).
type codexRefusalDB interface {
	guards.Querier
	guards.Execer
}

// reindexForRefusal is the Re-index answer for item.
func (h *Handlers) reindexForRefusal(ctx context.Context, item *models.WorkItem, userID string) error {
	if h.codexDB == nil {
		return fmt.Errorf("re-index is not available")
	}
	r, ok, err := guards.LoadRefusal(ctx, h.codexDB, item.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("work item %s was not refused on an empty codex, so there is nothing to re-index for it; use Retry", item.ID)
	}
	// Mark first: an index that finishes quickly must find the request.
	if err := guards.MarkReindexRequested(ctx, h.codexDB, item.ID, userID); err != nil {
		return err
	}
	res := codexindex.RequestFull(ctx, knowledgeIndexerURL, r.ProductID, r.IndexBranch)
	switch res.Outcome {
	case codexindex.Accepted:
		return guards.MarkIndexPosted(ctx, h.codexDB, item.ID)
	case codexindex.Busy:
		// 409: the indexer is busy with some index (its single flight
		// covers every product), so nothing indexes ours yet. The request
		// stays pending and unposted; status-orchestrator re-posts it
		// until the indexer takes it.
		return nil
	}
	// No index will report back: withdraw the request and raise the
	// escalation again so the item is not left blocked without a prompt,
	// and fail the answer so the operator sees why.
	reason := fmt.Sprintf("The re-index request failed (%s), so nothing is indexing the product. Check knowledge-indexer, then Re-index or Retry.", res.Error())
	if err := workgraph.WithTx(ctx, h.txdb, func(tx pgx.Tx) error {
		if err := guards.ClearReindexRequested(ctx, tx, item.ID); err != nil {
			return err
		}
		_, err := guards.EscalateRefusalTx(ctx, tx, item.ID, "dashboard-backend", reason)
		return err
	}); err != nil {
		return fmt.Errorf("re-index request failed (%s), and re-raising the escalation failed: %w", res.Error(), err)
	}
	return fmt.Errorf("re-index request failed: %s", res.Error())
}

// retryRefusal re-runs a refused item from its recorded message. replayed
// is false when the item has no refusal (the caller then takes the
// generic Retry path).
func (h *Handlers) retryRefusal(ctx context.Context, item *models.WorkItem, userID string) (replayed bool, err error) {
	if h.codexDB == nil || h.txdb == nil {
		return false, nil
	}
	if _, ok, err := guards.LoadRefusal(ctx, h.codexDB, item.ID); err != nil || !ok {
		return false, err
	}
	err = workgraph.WithTx(ctx, h.txdb, func(tx pgx.Tx) error {
		var rerr error
		replayed, rerr = guards.ReplayRefusalTx(ctx, tx, item.ID, userID, "retry via prompt sheet", guards.ReplayRetry)
		return rerr
	})
	if err != nil {
		return false, fmt.Errorf("re-run the refused stage: %w", err)
	}
	// Not replayed: the refusal was stale (the item's current block is not
	// the codex refusal) and ReplayRefusalTx dropped it; the caller takes
	// the generic Retry.
	return replayed, nil
}

// dropRefusal deletes item's refusal (cancelled).
func (h *Handlers) dropRefusal(ctx context.Context, item *models.WorkItem) error {
	return h.dropRefusalByID(ctx, item.ID)
}

// dropRefusalByID deletes the refusal of id, if any: every retry, cancel or
// unblock path other than the refusal's own replay calls it, so a stale
// refusal cannot re-run the item later.
func (h *Handlers) dropRefusalByID(ctx context.Context, id uuid.UUID) error {
	if h.codexDB == nil {
		return nil
	}
	return guards.DeleteRefusal(ctx, h.codexDB, id)
}

// dropRefusalLogged is dropRefusalByID for paths that have already
// committed their transition: a failure is logged (ReplayRefusalTx's
// latest-block check still keeps a leftover row from replaying).
func (h *Handlers) dropRefusalLogged(ctx context.Context, id uuid.UUID) {
	if err := h.dropRefusalByID(ctx, id); err != nil {
		log.Printf("handlers: drop codex refusal of %s: %v", id, err)
	}
}
