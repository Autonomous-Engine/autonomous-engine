package main

// Outbox wiring for the knowledge indexer (M1-04e).
//
//   - codex.indexed (migration 060): every codex version the indexer writes
//     (full index, incremental index, interaction answer, revoked answer)
//     enqueues its codex.indexed event in the version row's transaction
//     (codex.RecordVersion). The indexer no longer enqueues codex.updated
//     after a run: codex.updated is the index request, and nothing consumed
//     those completion notices (the indexer itself skipped them).
//   - codex.update.from_interaction: enqueued in the same transaction as the
//     user_interactions classification back-link (outboxCodexPromoter).
//   - codex.extract.requested from the commit-linked reconcile sweep: no row
//     write; a short transaction of its own on the repository aggregate.
//
// Telemetry that stays a direct publish: indexing.progress,
// onboarding.prefill.drift_detected and onboarding.prefill.completed.

import (
	"context"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/jackc/pgx/v5"
)

const (
	// aggregateTypeProduct orders a product's codex events.
	aggregateTypeProduct = "product"
	// aggregateTypeRepository orders a repository's extract requests (the
	// same addressing the webhook listener uses for them).
	aggregateTypeRepository = "repository"
)

// enqueueInShortTx writes one event in a transaction of its own.
func enqueueInShortTx(ctx context.Context, db workgraph.TxBeginner, ev outbox.Event) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, ev)
	})
}

// enqueueExtractRequest enqueues codex.extract.requested for one repository
// branch head.
func enqueueExtractRequest(ctx context.Context, db workgraph.TxBeginner, req extractRequest) error {
	ev, err := outbox.NewEvent(aggregateTypeRepository, req.RepoID, bus.TopicCodexExtractRequested, req.RepoID.String(), req)
	if err != nil {
		return err
	}
	return enqueueInShortTx(ctx, db, ev)
}
