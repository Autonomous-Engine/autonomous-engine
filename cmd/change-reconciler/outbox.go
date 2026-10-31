package main

// Outbox wiring for the change reconciler (M1-04e).
//
// codex.updated follows the codex upsert for a minor external change. The
// codex package has no transactional write, so the event is enqueued in a
// short transaction straight after the upsert: delivery is durable (retried,
// backed off, dead-lettered by the outbox) but not atomic with the upsert —
// a crash between the two loses the notification, never the codex row.
//
// The reconciler enqueues no work.blocked any more (codex coherence Part 1;
// see main.go).

import (
	"context"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// aggregateTypeProduct orders a product's codex.updated events.
	aggregateTypeProduct = "product"
	// aggregateTypeCodex is the aggregate for codex changes with no product
	// scope (the legacy single-repo namespace).
	aggregateTypeCodex = "codex"
)

// globalCodexAggregateID is the fixed aggregate id for productless codex
// events. Derived, so every service computes the same value.
var globalCodexAggregateID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("autonomous-engine:codex:global"))

// codexAggregate returns the aggregate a codex.updated event is ordered under.
func codexAggregate(productID *uuid.UUID) (string, uuid.UUID) {
	if productID != nil && *productID != uuid.Nil {
		return aggregateTypeProduct, *productID
	}
	return aggregateTypeCodex, globalCodexAggregateID
}

// enqueueCodexUpdated enqueues codex.updated (topic, key and payload as
// published before the conversion, plus product_id when the change resolved
// to a product) in a short transaction.
func enqueueCodexUpdated(ctx context.Context, db workgraph.TxBeginner, productID *uuid.UUID, key string, msg codexUpdatedMessage) error {
	aggType, aggID := codexAggregate(productID)
	if aggType == aggregateTypeProduct {
		msg.ProductID = aggID.String()
	}
	ev, err := outbox.NewEvent(aggType, aggID, bus.TopicCodexUpdated, key, msg)
	if err != nil {
		return err
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, ev)
	})
}
