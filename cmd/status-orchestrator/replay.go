package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// M4-06: replay (shadow) work items terminate deterministically. Their
// verified branches are reviewed without a PR (pr-coordinator sends a
// PR-less pr.ready); on the review.complete the item is cancelled with
// "replay: evidence recorded", in the transaction that also upserts its
// outcome (review defects included). An item with nothing to review is
// terminated by replay.complete from pr-coordinator. A replay item never
// reaches human review, QA, a fix cycle or a merge.

const replayCancelReason = "replay: evidence recorded"

// isShadowTerminal is rollout.IsShadowTerminal on the work graph's pool.
var isShadowTerminal = func(ctx context.Context, wg *workgraph.WorkGraph, workID uuid.UUID) (bool, error) {
	return rollout.IsShadowTerminal(ctx, wg.DB(), workID)
}

// cancelReplay moves a replay item to cancelled with its outcome. An item
// already terminal (a redelivery) is left alone.
func cancelReplay(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, workID uuid.UUID, detail string) error {
	reason := replayCancelReason
	if detail != "" {
		reason += " (" + detail + ")"
	}
	err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateCancelled, reason, outcomeEffect(outcomes.Facts{}))
	if errors.Is(err, workgraph.ErrInvalidTransition) {
		log.Printf("status-orchestrator: replay work_id=%s not cancelled (already terminal?): %v", workID, err)
		return nil
	}
	if err != nil {
		return err
	}
	log.Printf("status-orchestrator: replay work_id=%s cancelled: %s", workID, reason)
	return nil
}

// replayCompletePayload is pr-coordinator's replay.complete.
type replayCompletePayload struct {
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
}

// handleReplayComplete cancels a replay item that had nothing to review.
// It acts only on an item that really is a replay item.
func handleReplayComplete(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, msg *bus.Message) error {
	var p replayCompletePayload
	if err := json.Unmarshal(msg.Value, &p); err != nil {
		log.Printf("status-orchestrator: replay.complete: unmarshal: %v", err)
		return nil
	}
	workID, err := uuid.Parse(p.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: replay.complete: parse work_id %q: %v", p.WorkID, err)
		return nil
	}
	terminal, err := isShadowTerminal(ctx, wg, workID)
	if err != nil {
		return err
	}
	if !terminal {
		log.Printf("status-orchestrator: replay.complete for work_id=%s, which is not a replay item — ignored", workID)
		return nil
	}
	return cancelReplay(ctx, wg, kb, workID, p.Reason)
}
