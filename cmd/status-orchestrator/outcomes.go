package main

import (
	"context"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// M4-05: work_item_outcomes is upserted in the transaction of the state
// change that produced the fact (internal/outcomes). outcomeEffect is that
// write as a txEffect: it runs after the transition inside
// transitionAndPublish, so the outcome row and the transition commit or
// roll back together, and a redelivery recomputes the same row.

// outcomeEffect upserts the outcome of the transitioned item's top-level
// work item (its parent, or itself) with f.
func outcomeEffect(f outcomes.Facts) txEffect {
	return func(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error {
		return outcomes.UpsertTx(ctx, tx, aggregateFor(item), f)
	}
}

// subtaskDecision maps the subtask transition decideSubtaskTransitionFor
// chose to the outcome's release decision.
func subtaskDecision(toState models.WorkItemState, hasEvidence bool) string {
	switch toState {
	case models.WorkItemStateBlocked:
		return outcomes.DecisionBlock
	case models.WorkItemStateHumanReview:
		return outcomes.DecisionHumanReview
	default:
		if !hasEvidence {
			return outcomes.DecisionLegacyRelease
		}
		return outcomes.DecisionRelease
	}
}

// mergeFacts returns the merge identity a pr.merged payload carries (nil
// when it carries none: a pre-M4-05 webhook-listener).
func mergeFacts(p prMergedPayload) *outcomes.Merge {
	if p.PRNumber == 0 && p.MergeCommitSHA == "" && p.HeadSHA == "" {
		return nil
	}
	m := &outcomes.Merge{PRNumber: p.PRNumber, Repository: p.Repository, HeadSHA: p.HeadSHA, MergeSHA: p.MergeCommitSHA}
	for _, s := range []string{p.MergedAt, p.Timestamp} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			m.MergedAt = &t
			break
		}
	}
	return m
}

// recordOutcomeAlone runs an outcome effect in a transaction of its own,
// for a fact whose state change did not happen (a held or refused
// transition). Failures are logged.
func recordOutcomeAlone(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, workID uuid.UUID, effect txEffect) {
	if err := raisePromptAlone(ctx, db, wg, workID, effect); err != nil && !idempotency.IsDuplicate(err) {
		log.Printf("status-orchestrator: record outcome for %s: %v", workID, err)
	}
}

// recordHeldMerge records the merge of a held pr.merged (no transition) in
// an unguarded transaction: the outcome upsert is idempotent on its own, and
// the delivery's guard must stay for the codex.updated enqueue that follows,
// which has no other trigger.
func recordHeldMerge(ctx context.Context, db workgraph.TxBeginner, wg workItemStore, workID uuid.UUID, merge txEffect) {
	recordOutcomeAlone(idempotency.Unguarded(ctx), db, wg, workID, merge)
}

// markReverted applies the revert heuristic's result (reverts_pr_numbers,
// computed by webhook-listener from the merged PR's title and body). It is
// unguarded: the update only sets reverted_at where it is still NULL, so a
// redelivery is a no-op, and it must not consume the delivery's guard
// before the handler's own transition commits.
func markReverted(ctx context.Context, db workgraph.TxBeginner, p prMergedPayload) {
	if len(p.RevertsPRNumbers) == 0 || p.Repository == "" {
		return
	}
	at := time.Now().UTC()
	if m := mergeFacts(p); m != nil && m.MergedAt != nil {
		at = *m.MergedAt
	}
	var marked int64
	err := workgraph.WithTx(ctx, idempotency.Wrap(idempotency.Unguarded(ctx), db), func(tx pgx.Tx) error {
		n, err := outcomes.MarkRevertedTx(ctx, tx, p.Repository, p.RevertsPRNumbers, at)
		marked = n
		return err
	})
	if err != nil {
		log.Printf("status-orchestrator: pr.merged: revert heuristic for %s PRs %v: %v", p.Repository, p.RevertsPRNumbers, err)
		return
	}
	log.Printf("status-orchestrator: pr.merged: PR #%d in %s references reverts of %v (heuristic) — %d outcome row(s) marked reverted", p.PRNumber, p.Repository, p.RevertsPRNumbers, marked)
}
