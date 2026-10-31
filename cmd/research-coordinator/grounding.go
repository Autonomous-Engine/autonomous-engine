package main

import (
	"context"
	"fmt"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// contextGate is research-coordinator's context validation before research
// is dispatched: onboarding and guards.CheckCodexGrounded (codex coherence
// Part 3) for the product and branch, plus others (repository on disk,
// vector store). Every failure escalates, none blocks silently:
//
//   - a codex failure (or one alongside others) refuses through refuseCodex:
//     work.blocked with kind codex_ungrounded (Re-index / Retry / Cancel)
//     and the prd.ready message kept for the re-run;
//   - any other failure, or an item without a product, blocks through the
//     outbox (work.blocked, Retry / Cancel).
//
// refused is true whether or not that write failed (err).
func contextGate(ctx context.Context, pool *pgxpool.Pool, aggregateID, workID uuid.UUID, productID *uuid.UUID, branch string, msg *bus.Message, others guards.ValidationResult) (refused bool, err error) {
	var codexCheck guards.ValidationResult
	if productID != nil {
		codexCheck = guards.CheckCodexReady(ctx, pool, *productID, branch)
	} else {
		codexCheck = guards.ValidationResult{
			Valid:   false,
			Missing: []string{"product_id"},
			Message: fmt.Sprintf("work item %s has no product_id; cannot check codex grounding", workID),
		}
	}
	result := guards.RunAll(codexCheck, others)
	if result.Valid {
		return false, nil
	}
	log.Printf("research-coordinator: context validation failed for work_id=%s: %s", workID, result.Message)
	if productID != nil && guards.IsCodexRefusal(codexCheck) {
		return true, refuseCodex(ctx, pool, aggregateID, workID, *productID, branch, result.Message, msg)
	}
	return true, blockInOwnTx(ctx, idempotency.Wrap(ctx, pool), aggregateID, workID, result.Message)
}

// refuseCodex refuses workID in one guarded transaction: blocked,
// work.blocked (kind codex_ungrounded) and the re-run of msg (prd.ready
// from research).
func refuseCodex(ctx context.Context, pool *pgxpool.Pool, aggregateID, workID, productID uuid.UUID, branch, reason string, msg *bus.Message) error {
	return guards.RefuseUngrounded(ctx, idempotency.Wrap(ctx, pool), aggregateID, workID, agentID, guards.Refusal{
		ProductID:        productID,
		Branch:           branch,
		Stage:            agentID,
		Reason:           reason,
		RerunState:       models.WorkItemStateResearch,
		RerunTopic:       bus.TopicPRDReady,
		RerunKey:         msg.Key,
		RerunPayload:     msg.Value,
		RerunAggregateID: aggregateID,
	})
}
