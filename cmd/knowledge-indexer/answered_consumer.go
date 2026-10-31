// cmd/knowledge-indexer/answered_consumer.go — on user_prompt.answered:
//
//	(1) always embed + upsert to qdrant (best-effort; failure degrades gracefully),
//	(2) triage-classify the answer,
//	(3) if ShouldPromote, enqueue codex.update.from_interaction and write the
//	    classification + codex-table back-link in one transaction (outbox),
//	(4) otherwise (or if that fails) write the classification alone.
//
// The narrow interfaces (vectorUpserter, codexPromoter, triager, answeredStore)
// exist so Tasks 13 & 14 can implement the real behaviors against the
// existing internal/vector + internal/codex packages without touching this file.
package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// answeredStore is the narrow contract on interactions.Store.
type answeredStore interface {
	ApplyClassification(ctx context.Context, id uuid.UUID, r interactions.ClassificationResult) error
}

// vectorUpserter persists an answer as a semantic recall point. Task 13
// implements against internal/vector.
type vectorUpserter interface {
	UpsertUserDecision(ctx context.Context, productID uuid.UUID, p UserDecisionPoint) (string, error)
}

// codexPromoter records a promoted answer: the classification with its codex
// back-link and the codex.update.from_interaction event that creates the
// entry commit together (outboxCodexPromoter), so the row never points at a
// codex entry whose creating event was lost.
type codexPromoter interface {
	Promote(ctx context.Context, promptID, workItemID uuid.UUID, cls interactions.ClassificationResult, payload CodexUpdateFromInteraction) error
}

// triager is the narrow contract on interactions.Triage.
type triager interface {
	ClassifyWithOverride(ctx context.Context, question, answer string, override *interactions.SaveAsOverride) (*interactions.TriageResult, error)
}

// UserDecisionPoint is the qdrant payload for an answered interaction.
type UserDecisionPoint struct {
	InteractionID uuid.UUID
	WorkItemID    uuid.UUID
	Question      string
	Answer        string
	ClassifiedAs  interactions.ClassifiedAs
	AnsweredAt    string
}

// CodexUpdateFromInteraction is the Kafka payload for codex.update.from_interaction.
type CodexUpdateFromInteraction struct {
	Source        string         `json:"source"` // "user_interaction"
	InteractionID uuid.UUID      `json:"interaction_id"`
	ProductID     uuid.UUID      `json:"product_id"`
	CodexEntryID  uuid.UUID      `json:"codex_entry_id"`
	Section       string         `json:"codex_section"` // 'codex_modules' | 'codex_patterns' | 'codex_anti_patterns' | 'codex_sections'
	EntryType     string         `json:"entry_type"`    // the ClassifiedAs value
	EntryPayload  map[string]any `json:"entry_payload"`
}

type AnsweredConsumer struct {
	store   answeredStore
	vectors vectorUpserter
	codex   codexPromoter
	triage  triager
}

func NewAnsweredConsumer(s answeredStore, v vectorUpserter, c codexPromoter, t triager) *AnsweredConsumer {
	return &AnsweredConsumer{store: s, vectors: v, codex: c, triage: t}
}

func (c *AnsweredConsumer) Handle(ctx context.Context, m *bus.Message) error {
	var a interactions.AnsweredPayload
	if err := json.Unmarshal(m.Value, &a); err != nil {
		log.Printf("answered_consumer: bad payload: %v", err)
		return nil // unparseable — don't redeliver
	}

	// Step 2 — classify. (Doing this first so we can include the classification
	// in the qdrant payload for filtered retrieval in past_decisions_query.)
	res, err := c.triage.ClassifyWithOverride(ctx, a.Question, a.AnswerText, a.SaveAsOverride)
	if err != nil {
		// Real LLM error — degrade to ad_hoc rather than blocking the user.
		log.Printf("answered_consumer: triage failed prompt=%s: %v", a.PromptID, err)
		res = &interactions.TriageResult{ClassifiedAs: interactions.ClassifiedAdHoc}
	}

	// Step 1 — always attempt qdrant upsert. Failure doesn't block the rest.
	pointID, err := c.vectors.UpsertUserDecision(ctx, a.ProductID, UserDecisionPoint{
		InteractionID: a.PromptID,
		WorkItemID:    a.WorkItemID,
		Question:      a.Question,
		Answer:        a.AnswerText,
		ClassifiedAs:  res.ClassifiedAs,
		AnsweredAt:    a.AnsweredAt,
	})
	if err != nil {
		log.Printf("answered_consumer: qdrant upsert failed prompt=%s: %v", a.PromptID, err)
		pointID = "" // continue; classification write-back still matters
	}

	cls := interactions.ClassificationResult{
		ClassifiedAs:  res.ClassifiedAs,
		Confidence:    res.Confidence,
		QdrantPointID: pointID,
	}

	// Step 3 — promote to codex if eligible: the back-link and the event
	// that creates the entry commit together.
	if res.ShouldPromote {
		entryID := uuid.New()
		table := res.ClassifiedAs.CodexTable()
		promoted := cls
		promoted.CodexEntryID = &entryID
		promoted.CodexEntryTable = table
		err := c.codex.Promote(ctx, a.PromptID, a.WorkItemID, promoted, CodexUpdateFromInteraction{
			Source:        "user_interaction",
			InteractionID: a.PromptID,
			ProductID:     a.ProductID,
			CodexEntryID:  entryID,
			Section:       table,
			EntryType:     string(res.ClassifiedAs),
			EntryPayload:  res.ProposedEntry,
		})
		if err == nil {
			return nil
		}
		// Degrade as before: record the classification without a codex
		// back-link rather than lose the answer's classification.
		log.Printf("answered_consumer: codex promotion failed prompt=%s: %v", a.PromptID, err)
	}

	// Step 4 — write classification back to user_interactions.
	return c.store.ApplyClassification(ctx, a.PromptID, cls)
}
