package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// codexGate is product-agent's grounding gate (codex coherence Part 3):
// onboarding complete and guards.CheckCodexGrounded for the product and
// branch. A failure refuses the item through refuseCodex, so it escalates
// (work.blocked, Re-index / Retry / Cancel) with the intake message kept for
// the re-run; refused is true whether or not that write failed (err).
func codexGate(ctx context.Context, pool *pgxpool.Pool, aggregateID, workID, productID uuid.UUID, branch string, msg *bus.Message) (refused bool, err error) {
	result := guards.CheckCodexReady(ctx, pool, productID, branch)
	if result.Valid {
		return false, nil
	}
	log.Printf("product-agent: context validation failed for work_id=%s: %s", workID, result.Message)
	return true, refuseCodex(ctx, pool, aggregateID, workID, productID, branch, result.Message, msg)
}

// refuseCodex refuses workID in one guarded transaction: blocked,
// work.blocked (kind codex_ungrounded) and the re-run of msg (intake).
func refuseCodex(ctx context.Context, pool *pgxpool.Pool, aggregateID, workID, productID uuid.UUID, branch, reason string, msg *bus.Message) error {
	return guards.RefuseUngrounded(ctx, idempotency.Wrap(ctx, pool), aggregateID, workID, agentID, guards.Refusal{
		ProductID:        productID,
		Branch:           branch,
		Stage:            agentID,
		Reason:           reason,
		RerunState:       models.WorkItemStateIntake,
		RerunTopic:       bus.TopicIntake,
		RerunKey:         msg.Key,
		RerunPayload:     msg.Value,
		RerunAggregateID: aggregateID,
	})
}
