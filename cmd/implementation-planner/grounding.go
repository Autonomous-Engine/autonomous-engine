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

// codexGate is the planner's grounding gate (codex coherence Part 3):
// onboarding complete and guards.CheckCodexGrounded for the product and
// branch. A failure refuses the parent through refuseCodex: the planner's
// escalation path (work.blocked → status-orchestrator's prompt), with
// Re-index / Retry / Cancel and the technical-brief.ready message kept for
// the re-run. refused is true whether or not that write failed (err).
func codexGate(ctx context.Context, pool *pgxpool.Pool, workID, productID uuid.UUID, branch string, msg *bus.Message) (refused bool, err error) {
	result := guards.CheckCodexReady(ctx, pool, productID, branch)
	if result.Valid {
		return false, nil
	}
	log.Printf("implementation-planner: work_id=%s: planning blocked (%s): %s", workID, blockContextValidation, result.Message)
	return true, refuseCodex(ctx, pool, workID, productID, branch, result.Message, msg)
}

// refuseCodex refuses the parent in one guarded transaction: blocked,
// work.blocked (kind codex_ungrounded) and the re-run of msg
// (technical-brief.ready from research; the planner moves it to planning).
func refuseCodex(ctx context.Context, pool *pgxpool.Pool, workID, productID uuid.UUID, branch, reason string, msg *bus.Message) error {
	return guards.RefuseUngrounded(ctx, idempotency.Wrap(ctx, pool), workID, workID, agentID, guards.Refusal{
		ProductID:        productID,
		Branch:           branch,
		Stage:            agentID,
		Reason:           reason,
		RerunState:       models.WorkItemStateResearch,
		RerunTopic:       bus.TopicTechnicalBriefReady,
		RerunKey:         msg.Key,
		RerunPayload:     msg.Value,
		RerunAggregateID: workID,
	})
}
