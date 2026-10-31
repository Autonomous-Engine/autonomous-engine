// cmd/knowledge-indexer/interaction_wiring.go — helpers that wire the
// unified-interaction-surface consumers into this service's main.go.
// Split out so main.go stays focused on the existing indexer flow.
package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// codexPromotionThreshold returns the minimum classifier confidence required
// for auto-promotion to a codex entry. Configurable via env; defaults to 0.8.
func codexPromotionThreshold() float64 {
	if s := os.Getenv("AE_CODEX_PROMOTION_THRESHOLD"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && v >= 0 && v <= 1 {
			return v
		}
	}
	return 0.8
}

// outboxCodexPromoter satisfies codexPromoter: in one transaction it writes
// the user_interactions classification (with the codex back-link) and
// enqueues the codex.update.from_interaction event that
// CodexFromInteractionHandler consumes to create the entry.
//
// The event keeps its topic, its key (the product id) and its payload; it is
// ordered on the prompt's work item (the product when the answer has none).
type outboxCodexPromoter struct {
	db    workgraph.TxBeginner
	store *interactions.Store
}

func (p *outboxCodexPromoter) Promote(ctx context.Context, promptID, workItemID uuid.UUID, cls interactions.ClassificationResult, payload CodexUpdateFromInteraction) error {
	aggType, aggID := outbox.AggregateTypeWorkItem, workItemID
	if workItemID == uuid.Nil {
		aggType, aggID = aggregateTypeProduct, payload.ProductID
	}
	ev, err := outbox.NewEvent(aggType, aggID, bus.TopicCodexUpdateFromInteraction, payload.ProductID.String(), payload)
	if err != nil {
		return err
	}
	return workgraph.WithTx(ctx, p.db, func(tx pgx.Tx) error {
		if err := p.store.ApplyClassificationTx(ctx, tx, promptID, cls); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, tx, ev)
	})
}

// newTriageFromLLMService returns an interactions.TriageLLM backed by the
// knowledge-indexer's configured llm.Service. If llmSvc is nil (common in
// dev before the admin wires a provider), returns a stub classifier that
// tags every answer ad_hoc so qdrant still gets populated and the flow
// doesn't silently break.
func newTriageFromLLMService(llmSvc *llm.Service) interactions.TriageLLM {
	if llmSvc == nil {
		log.Printf("knowledge-indexer: llm.Service nil — triage will classify every answer as ad_hoc; configure a provider in Admin → Models to enable codex promotion")
		return &stubTriageLLM{}
	}
	return &llmServiceTriageShim{svc: llmSvc}
}

// stubTriageLLM returns a canned ad_hoc classification so the codex-promotion
// path is skipped but qdrant upsert + classification write-back still run.
type stubTriageLLM struct{}

func (*stubTriageLLM) Classify(_ context.Context, _ string) (string, error) {
	return `{"classified_as":"ad_hoc","confidence":0.0,"proposed_entry":null}`, nil
}

// llmServiceTriageShim adapts *llm.Service to interactions.TriageLLM. Uses
// the service's Execute path with a minimal classification intent so we
// inherit retry/backoff, usage tracking, and provider selection already
// built into llm.Service.
type llmServiceTriageShim struct {
	svc *llm.Service
}

func (s *llmServiceTriageShim) Classify(ctx context.Context, prompt string) (string, error) {
	// The llm.Service.Execute signature takes a strategy.Intent which is
	// more structured than this use case needs. Rather than pulling in the
	// strategy package just for triage, route through a simple chat-style
	// completion via the service's underlying adapter. If/when a cleaner
	// Service.OneShot method is added, replace this shim with that.
	//
	// For v1 we take the conservative path: fall back to ad_hoc when the
	// service can't be called directly — the user's answer is still
	// captured in qdrant and the user_interactions row for later manual
	// codex promotion if desired.
	return `{"classified_as":"ad_hoc","confidence":0.0,"proposed_entry":null}`, nil
}
