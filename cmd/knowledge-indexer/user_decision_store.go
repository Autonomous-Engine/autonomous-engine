// cmd/knowledge-indexer/user_decision_store.go — per-product qdrant
// collection manager for "answered user interactions" semantic recall.
// Creates collection `product_<uuid>_user_decisions` on first upsert per
// product and caches the *vector.VectorStore handle to avoid re-creating it.
// Implements the vectorUpserter interface used by AnsweredConsumer.
package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

// embedder is the narrow contract we need from an embedding source.
// Matches the subset of *vector.EmbeddingService we call, so the
// production wire-up passes the real service and tests pass a stub.
type embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimension(ctx context.Context) (int, error)
}

// UserDecisionStore manages per-product qdrant collections for recording
// answered user interactions as semantic-recallable points.
type UserDecisionStore struct {
	qdrantURL string
	embedder  embedder

	mu    sync.Mutex
	stores map[uuid.UUID]*vector.VectorStore // productID → cached VectorStore
}

func NewUserDecisionStore(qdrantURL string, emb embedder) *UserDecisionStore {
	return &UserDecisionStore{
		qdrantURL: qdrantURL,
		embedder:  emb,
		stores:    map[uuid.UUID]*vector.VectorStore{},
	}
}

// collectionNameFor returns the per-product collection name used by this store.
func collectionNameFor(productID uuid.UUID) string {
	return "product_" + productID.String() + "_user_decisions"
}

// getOrCreate returns a *vector.VectorStore bound to the product's collection,
// creating and caching it on first use. The collection is ensured to exist
// on first use via EnsureCollection.
func (s *UserDecisionStore) getOrCreate(ctx context.Context, productID uuid.UUID) (*vector.VectorStore, error) {
	s.mu.Lock()
	vs, ok := s.stores[productID]
	s.mu.Unlock()
	if ok {
		return vs, nil
	}

	dim, err := s.embedder.Dimension(ctx)
	if err != nil {
		return nil, fmt.Errorf("embedder.Dimension: %w", err)
	}
	vs = vector.New(s.qdrantURL, collectionNameFor(productID))
	if err := vs.EnsureCollection(ctx, dim); err != nil {
		return nil, fmt.Errorf("ensureCollection: %w", err)
	}

	s.mu.Lock()
	// Double-check: another goroutine might have inserted while we were ensuring.
	if existing, ok := s.stores[productID]; ok {
		s.mu.Unlock()
		return existing, nil
	}
	s.stores[productID] = vs
	s.mu.Unlock()
	return vs, nil
}

// UpsertUserDecision satisfies the vectorUpserter interface. Returns the
// qdrant point ID so the caller can persist it on user_interactions for later
// lookup (e.g., revocation flips payload.revoked=true).
func (s *UserDecisionStore) UpsertUserDecision(ctx context.Context, productID uuid.UUID, p UserDecisionPoint) (string, error) {
	vs, err := s.getOrCreate(ctx, productID)
	if err != nil {
		return "", err
	}

	content := p.Question + "\n\n" + p.Answer
	vec, err := s.embedder.Embed(ctx, content)
	if err != nil {
		return "", fmt.Errorf("embed: %w", err)
	}

	pointID := uuid.New().String()
	doc := vector.Document{
		ID:      pointID,
		Content: content,
		Metadata: map[string]string{
			"interaction_id": p.InteractionID.String(),
			"work_item_id":   p.WorkItemID.String(),
			"product_id":     productID.String(),
			"question":       p.Question,
			"answer":         p.Answer,
			"classified_as":  string(p.ClassifiedAs),
			"answered_at":    p.AnsweredAt,
			"revoked":        "false", // Metadata is map[string]string; flip to "true" on revoke
		},
		Vector: vec,
	}
	if err := vs.Index(ctx, []vector.Document{doc}); err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	return pointID, nil
}

// Ensure ClassifiedAs reference doesn't get removed by goimports — it's
// used via the metadata field above. Keep the import explicit for lint.
var _ = interactions.ClassifiedEphemeral
