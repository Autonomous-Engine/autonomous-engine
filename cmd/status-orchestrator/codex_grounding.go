package main

// Codex coherence Part 3: no planning or implementation on an empty codex.
//
//   - work.blocked with kind codex_ungrounded (guards.RefuseUngroundedTx:
//     product-agent, research-coordinator, implementation-planner) raises the
//     escalation with Re-index / Retry / Cancel (codexRefusalPrompt) instead
//     of the generic Retry / Cancel.
//   - codex.indexed (own consumer group, idempotent via processed_events):
//     every refusal of the product whose Re-index is pending
//     (codex_grounding_refusals.reindex_requested_at, set by
//     dashboard-backend) is re-run when the codex is now grounded
//     (guards.CheckCodexGrounded on the refusal's branch, the same check the
//     stage made). An index of the refusal's branch that still leaves no
//     module withdraws the request and raises the escalation again, so the
//     item never waits silently. Events of other products touch nothing.
//     The pending re-run is a Postgres row, so a restart loses nothing.
//     Only an event for the refusal's own branch settles it.
//   - A Re-index knowledge-indexer refused with 409 (its single flight
//     covers every product) stays pending and unposted; a poster re-posts
//     it every 30s until accepted, and a failed post withdraws it and
//     escalates again. A request no codex.indexed settles within
//     CODEX_REINDEX_TIMEOUT (default 2h) is withdrawn and escalated again by
//     a periodic sweep.
//   - codex.gap (same consumer group): every empty codex lookup is recorded
//     per work item and run (internal/codexgap); a run with more than
//     CODEX_GAP_THRESHOLD lookups is marked ungrounded, which the
//     verification stage reads.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/codexgap"
	"github.com/autonomousengine/autonomous-engine/internal/codexindex"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// codexConsumerGroup consumes codex.indexed and codex.gap apart from
	// the pipeline topics, so a burst of gap events never delays a state
	// transition.
	codexConsumerGroup = "status-orchestrator-codex"
	// codexIdempotencyConsumer is its processed_events consumer.
	codexIdempotencyConsumer = "status-orchestrator-codex"
)

// codexTopics are the codex consumer group's topics.
var codexTopics = []string{bus.TopicCodexIndexed, bus.TopicCodexGap}

// codexRefusalPrompt is the escalation for a refusal on an ungrounded
// codex: Re-index (full index of the product, the item re-runs once the
// codex has a module), Retry (re-run the refused stage now) or Cancel.
func codexRefusalPrompt(item *models.WorkItem, reason string) *interactions.Prompt {
	return guards.CodexRefusalPrompt(item.ID, productOf(item), item.Title, agentID, reason)
}

// blockedPromptEffectFor is the escalation raised for a work.blocked of kind.
func blockedPromptEffectFor(kind, reason string) txEffect {
	if kind == guards.BlockKindCodexUngrounded {
		return promptEffect(func(item *models.WorkItem) *interactions.Prompt {
			return codexRefusalPrompt(item, reason)
		})
	}
	return blockedPromptEffect(reason)
}

// buildCodexHandler is the codex consumer group's handler (wrapped by the
// idempotency ledger in main).
func buildCodexHandler(wg *workgraph.WorkGraph, kb bus.MessageBus, gapThreshold int) bus.Handler {
	return func(ctx context.Context, msg *bus.Message) error {
		switch msg.Topic {
		case bus.TopicCodexIndexed:
			return handleCodexIndexed(ctx, wg.DB(), wg, kb, msg)
		case bus.TopicCodexGap:
			return handleCodexGap(ctx, wg.DB(), msg, gapThreshold)
		default:
			log.Printf("status-orchestrator: codex consumer: unhandled topic %s", msg.Topic)
			return nil
		}
	}
}

// replayedItem is one re-run committed by handleCodexIndexed.
type replayedItem struct {
	id uuid.UUID
	to models.WorkItemState
}

// handleCodexIndexed re-runs the product's refusals waiting for a re-index
// once its codex is grounded, in one transaction with the delivery's guard
// row (each refusal in a savepoint, so one item that cannot be re-run does
// not hold back the others).
func handleCodexIndexed(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, kb bus.MessageBus, msg *bus.Message) error {
	ev, err := codex.DecodeIndexedEvent(msg.Value)
	if err != nil {
		log.Printf("status-orchestrator: codex.indexed: %v", err)
		return nil
	}
	var replayed []replayedItem
	err = workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		replayed = nil
		pending, err := guards.PendingReindex(ctx, tx, ev.ProductID)
		if err != nil {
			return err
		}
		for _, r := range pending {
			sp, err := tx.Begin(ctx)
			if err != nil {
				return fmt.Errorf("savepoint: %w", err)
			}
			to, done, ierr := settleRefusal(ctx, sp, ev, r)
			if ierr != nil {
				log.Printf("status-orchestrator: codex.indexed: product %s v%d: work item %s: %v — left waiting", ev.ProductID, ev.ProductVersion, r.WorkItemID, ierr)
				_ = sp.Rollback(ctx)
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return fmt.Errorf("release savepoint: %w", err)
			}
			if done {
				replayed = append(replayed, replayedItem{id: r.WorkItemID, to: to})
			}
		}
		return nil
	})
	if err != nil {
		if idempotency.IsDuplicate(err) {
			return nil
		}
		return fmt.Errorf("codex.indexed product %s v%d: %w", ev.ProductID, ev.ProductVersion, err)
	}
	for _, r := range replayed {
		log.Printf("status-orchestrator: codex.indexed: product %s v%d on %s: work item %s re-run (blocked → %s)", ev.ProductID, ev.ProductVersion, ev.Branch, r.id, r.to)
		publishStateChanged(ctx, kb, r.id, models.WorkItemStateBlocked, r.to, "codex re-indexed")
	}
	return nil
}

// settleRefusal handles one pending refusal for ev. Only an event for the
// refusal's own branch (index_branch; any branch when that is unknown)
// settles it: re-run when the codex is grounded; when it is still empty and
// our index was posted, withdraw and escalate again; otherwise keep
// waiting (an index of another branch, or one that ran before ours was
// accepted).
func settleRefusal(ctx context.Context, tx pgx.Tx, ev codex.IndexedEvent, r guards.StoredRefusal) (to models.WorkItemState, replayed bool, err error) {
	if r.IndexBranch != "" && r.IndexBranch != ev.Branch {
		return "", false, nil
	}
	check := guards.CheckCodexGrounded(ctx, tx, r.ProductID, r.Branch)
	if check.Valid {
		reason := fmt.Sprintf("codex re-indexed (product_version %d on %s): re-running %s", ev.ProductVersion, ev.Branch, r.Stage)
		ok, err := guards.ReplayRefusalTx(ctx, tx, r.WorkItemID, agentID, reason, guards.ReplayReindexed)
		if err != nil {
			return "", false, err
		}
		// Not replayed: stale (dropped) or withdrawn meanwhile (kept).
		return models.WorkItemState(r.RerunState), ok, nil
	}
	if r.IndexPostedAt == nil {
		return "", false, nil
	}
	reason := fmt.Sprintf("The re-index of the product finished (product_version %d on %s) but the codex still has no module, so the item was not re-run. %s",
		ev.ProductVersion, ev.Branch, check.Message)
	return "", false, withdrawAndEscalate(ctx, tx, r.WorkItemID, reason)
}

// withdrawAndEscalate withdraws workID's Re-index request and raises its
// codex escalation again (unless one is pending).
func withdrawAndEscalate(ctx context.Context, tx pgx.Tx, workID uuid.UUID, reason string) error {
	if err := guards.ClearReindexRequested(ctx, tx, workID); err != nil {
		return err
	}
	_, err := guards.EscalateRefusalTx(ctx, tx, workID, agentID, reason)
	return err
}

// reindexTimeoutEnv bounds how long a Re-index may stay pending without a
// codex.indexed for its product (the index failed before writing a
// version, or never started). Default reindexTimeoutDefault.
const reindexTimeoutEnv = "CODEX_REINDEX_TIMEOUT"

const reindexTimeoutDefault = 2 * time.Hour

func reindexTimeoutFromEnv() time.Duration {
	if d, err := time.ParseDuration(os.Getenv(reindexTimeoutEnv)); err == nil && d > 0 {
		return d
	}
	return reindexTimeoutDefault
}

// staleReindexSQL lists the Re-index requests older than $1 whose refusal
// still describes its item (guards.CurrentRefusalCondition).
const staleReindexSQL = `
	SELECT r.work_item_id FROM codex_grounding_refusals r
	 WHERE r.reindex_requested_at IS NOT NULL AND r.reindex_requested_at < now() - $1::interval
	   AND ` + guards.CurrentRefusalCondition + `
	 ORDER BY r.reindex_requested_at
	 LIMIT 100
	 FOR UPDATE OF r SKIP LOCKED`

// sweepStaleReindex withdraws every Re-index request older than timeout
// that no codex.indexed settled, and raises the refusal's escalation again
// (unless one is pending), so an index that failed without a version never
// leaves the item waiting silently. A refusal that no longer describes its
// item (it moved on, or was blocked again for another reason) is dropped
// instead, without an escalation. It returns the items it swept.
func sweepStaleReindex(ctx context.Context, db workgraph.TxBeginner, timeout time.Duration) ([]uuid.UUID, error) {
	var swept []uuid.UUID
	err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		swept = nil
		dropped, err := guards.DropStaleReindex(ctx, tx)
		if err != nil {
			return err
		}
		if len(dropped) > 0 {
			log.Printf("status-orchestrator: re-index sweep: dropped %d refusals that no longer describe their item: %v", len(dropped), dropped)
		}
		rows, err := tx.Query(ctx, staleReindexSQL, fmt.Sprintf("%d seconds", int64(timeout.Seconds())))
		if err != nil {
			return fmt.Errorf("list stale re-index requests: %w", err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		reason := fmt.Sprintf("The re-index requested for this item did not report back within %s (no codex.indexed for the product and branch: the index failed, never ran, or the indexer stayed busy), so the item was not re-run. Check knowledge-indexer, then Re-index or Retry.", timeout)
		for _, id := range ids {
			if err := withdrawAndEscalate(ctx, tx, id, reason); err != nil {
				return err
			}
			swept = append(swept, id)
		}
		return nil
	})
	return swept, err
}

// postPendingReindex posts every Re-index the indexer has not accepted yet
// (it answered 409: one index at a time across all products) to indexerURL:
// accepted → recorded as posted; busy → left for the next round; failed →
// withdrawn and escalated again. Several replicas may post the same row; the
// second post meets a 409 and is harmless.
func postPendingReindex(ctx context.Context, db *pgxpool.Pool, indexerURL string) {
	pending, err := guards.UnpostedReindex(ctx, db, 20)
	if err != nil {
		log.Printf("status-orchestrator: re-index poster: %v", err)
		return
	}
	for _, r := range pending {
		res := codexindex.RequestFull(ctx, indexerURL, r.ProductID, r.IndexBranch)
		switch res.Outcome {
		case codexindex.Accepted:
			if err := guards.MarkIndexPosted(ctx, db, r.WorkItemID); err != nil {
				log.Printf("status-orchestrator: re-index poster: %v", err)
			}
		case codexindex.Busy:
			// Still busy: next round.
		default:
			reason := fmt.Sprintf("The re-index request failed (%s), so nothing is indexing the product. Check knowledge-indexer, then Re-index or Retry.", res.Error())
			if err := workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
				return withdrawAndEscalate(ctx, tx, r.WorkItemID, reason)
			}); err != nil {
				log.Printf("status-orchestrator: re-index poster: work item %s: %v", r.WorkItemID, err)
			}
		}
	}
}

// runReindexMaintenance re-posts busy Re-index requests every postInterval
// and sweeps stale ones every sweepInterval, until ctx ends.
func runReindexMaintenance(ctx context.Context, db *pgxpool.Pool, indexerURL string, timeout, postInterval, sweepInterval time.Duration) {
	post := time.NewTicker(postInterval)
	sweep := time.NewTicker(sweepInterval)
	defer post.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-post.C:
			postPendingReindex(ctx, db, indexerURL)
		case <-sweep.C:
			ids, err := sweepStaleReindex(ctx, db, timeout)
			if err != nil {
				log.Printf("status-orchestrator: re-index sweep: %v", err)
			}
			for _, id := range ids {
				log.Printf("status-orchestrator: re-index sweep: work item %s: no codex.indexed within %s — escalated again", id, timeout)
			}
		}
	}
}

// handleCodexGap records one codex.gap event (internal/codexgap) in a
// transaction with the delivery's guard row.
func handleCodexGap(ctx context.Context, db workgraph.TxBeginner, msg *bus.Message, threshold int) error {
	var res codexgap.Result
	err := workgraph.WithTx(ctx, idempotency.Wrap(ctx, db), func(tx pgx.Tx) error {
		var err error
		res, err = codexgap.RecordTx(ctx, tx, msg.Value, threshold)
		return err
	})
	if err != nil {
		if idempotency.IsDuplicate(err) {
			return nil
		}
		if errors.Is(err, codexgap.ErrMalformed) {
			log.Printf("status-orchestrator: codex.gap: %v — dropped", err)
			return nil
		}
		return fmt.Errorf("codex.gap: %w", err)
	}
	if res.Newly {
		log.Printf("status-orchestrator: codex.gap: work item %s: %d empty codex lookups in its %s run (threshold %d) — run marked ungrounded; verification will not approve it automatically (at %s)",
			msg.Key, res.Count, res.Stage, threshold, time.Now().UTC().Format(time.RFC3339))
	}
	return nil
}
