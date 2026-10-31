package main

// Outbox wiring for onboarding (M1-04e).
//
//   - Validation work items: the work item and its intake event commit in one
//     transaction (createValidationItem).
//   - codex.indexed after approve-codex (migration 060): enqueued by
//     codex.RecordVersion in the version row's transaction. The earlier
//     codex.updated notification ({session_id, version_number, source}) is
//     no longer sent: the indexer, its only consumer, skipped it as a
//     trigger-less notification.
//   - codex.updated as the run-analysis trigger: no row write at all; it is
//     enqueued in a short transaction of its own so the trigger cannot be lost
//     to a broker hiccup after the handler reported success.
//
// Telemetry that stays a direct publish: onboarding.prefill.progress (the
// regenerate trigger the dashboard watches; see the M1-04e brief).

import (
	"context"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// aggregateTypeProduct orders a product's codex.updated events.
	aggregateTypeProduct = "product"
	// aggregateTypeOnboardingSession is used for an analysis trigger that
	// names no product: the onboarding session is the only identity it has.
	aggregateTypeOnboardingSession = "onboarding_session"
)

// analysisTriggerAggregate picks the aggregate for the run-analysis trigger:
// the product when the request names a valid one, otherwise the session.
func analysisTriggerAggregate(sessionID, productID string) (string, uuid.UUID) {
	if pid, err := uuid.Parse(productID); err == nil && pid != uuid.Nil {
		return aggregateTypeProduct, pid
	}
	if sid, err := uuid.Parse(sessionID); err == nil {
		return aggregateTypeOnboardingSession, sid
	}
	return aggregateTypeOnboardingSession, uuid.NewSHA1(uuid.NameSpaceURL, []byte("onboarding-session:"+sessionID))
}

// enqueueCodexUpdated enqueues one codex.updated event in a short transaction.
func enqueueCodexUpdated(ctx context.Context, db workgraph.TxBeginner, aggregateType string, aggregateID uuid.UUID, key string, payload any) error {
	ev, err := outbox.NewEvent(aggregateType, aggregateID, bus.TopicCodexUpdated, key, payload)
	if err != nil {
		return err
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return outbox.Enqueue(ctx, tx, ev)
	})
}

// createValidationItem writes a validation work item and its intake event in
// one transaction.
func createValidationItem(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, item *models.WorkItem, payload map[string]interface{}) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := wg.CreateWorkItemTx(ctx, tx, item); err != nil {
			return err
		}
		return outbox.EnqueueJSON(ctx, tx, item.ID, bus.TopicIntake, item.ID.String(), payload)
	})
}
