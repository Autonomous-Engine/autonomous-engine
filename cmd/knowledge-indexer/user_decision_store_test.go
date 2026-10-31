// cmd/knowledge-indexer/user_decision_store_test.go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

// newFakeQdrant returns an httptest server that handles the minimal subset
// of Qdrant endpoints our upsert path hits: DescribeCollection (GET), create
// collection (PUT), and index points (PUT /points).
//
// It records every inbound request so tests can assert on URLs / bodies.
func newFakeQdrant(t *testing.T) (*httptest.Server, *qdrantRecorder) {
	t.Helper()
	rec := &qdrantRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.reqs = append(rec.reqs, recordedReq{Method: r.Method, URL: r.URL.Path})

		switch {
		// DescribeCollection — first call per product. Return 404 so the
		// caller creates the collection.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/collections/") &&
			!strings.HasSuffix(r.URL.Path, "/points") && !strings.Contains(r.URL.Path, "/points/"):
			if _, ok := rec.createdCollections[r.URL.Path]; !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))

		// Create collection (PUT /collections/<name>).
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/collections/") &&
			!strings.Contains(r.URL.Path, "/points"):
			if rec.createdCollections == nil {
				rec.createdCollections = map[string]bool{}
			}
			rec.createdCollections[r.URL.Path] = true
			_, _ = w.Write([]byte(`{"result":true}`))

		// Upsert points.
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			_, _ = w.Write([]byte(`{"result":{"status":"completed","operation_id":1}}`))

		default:
			// Print for debug but don't fail; Qdrant has other endpoints we
			// don't stub. Returning OK lets the code continue.
			_, _ = w.Write([]byte(`{"result":true}`))
		}
	}))
	return srv, rec
}

type recordedReq struct{ Method, URL string }

type qdrantRecorder struct {
	mu                 sync.Mutex
	reqs               []recordedReq
	createdCollections map[string]bool
}

// stubEmbedder implements the contract the UserDecisionStore expects from an
// embedder. Returns a fixed-length vector deterministically.
type stubEmbedder struct{ dim int }

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	out := make([]float32, s.dim)
	for i := range out {
		out[i] = 0.1
	}
	return out, nil
}
func (s *stubEmbedder) Dimension(_ context.Context) (int, error) { return s.dim, nil }

func TestUserDecisionStore_Upsert_CreatesCollectionOnFirstCall(t *testing.T) {
	srv, rec := newFakeQdrant(t)
	defer srv.Close()

	store := NewUserDecisionStore(srv.URL, &stubEmbedder{dim: 768})
	productID := uuid.New()

	pointID, err := store.UpsertUserDecision(context.Background(), productID, UserDecisionPoint{
		InteractionID: uuid.New(),
		WorkItemID:    uuid.New(),
		Question:      "what id type?",
		Answer:        "use GUIDs",
		ClassifiedAs:  interactions.ClassifiedCodexAntipattern,
		AnsweredAt:    "2026-04-23T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("UpsertUserDecision: %v", err)
	}
	if pointID == "" {
		t.Error("expected a non-empty point ID")
	}

	// Expect a GET for collection describe, a PUT to create the collection,
	// then a PUT to /points to upsert. Assert the collection name matches.
	expected := "/collections/product_" + productID.String() + "_user_decisions"
	sawCreate := false
	sawPoints := false
	for _, r := range rec.reqs {
		if r.Method == http.MethodPut && r.URL == expected {
			sawCreate = true
		}
		if r.Method == http.MethodPut && r.URL == expected+"/points" {
			sawPoints = true
		}
	}
	if !sawCreate {
		t.Errorf("expected PUT %s (create collection); requests: %+v", expected, rec.reqs)
	}
	if !sawPoints {
		t.Errorf("expected PUT %s/points (upsert); requests: %+v", expected, rec.reqs)
	}
}

func TestUserDecisionStore_Upsert_ReusesCollectionOnSecondCall(t *testing.T) {
	srv, rec := newFakeQdrant(t)
	defer srv.Close()

	store := NewUserDecisionStore(srv.URL, &stubEmbedder{dim: 768})
	productID := uuid.New()

	// First call — creates collection.
	_, err := store.UpsertUserDecision(context.Background(), productID, UserDecisionPoint{
		InteractionID: uuid.New(), WorkItemID: uuid.New(),
		Question: "q1", Answer: "a1",
		AnsweredAt: "t1",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	createCalls1 := 0
	for _, r := range rec.reqs {
		if r.Method == http.MethodPut && !strings.Contains(r.URL, "/points") {
			createCalls1++
		}
	}

	// Second call — must NOT re-create the collection.
	_, err = store.UpsertUserDecision(context.Background(), productID, UserDecisionPoint{
		InteractionID: uuid.New(), WorkItemID: uuid.New(),
		Question: "q2", Answer: "a2",
		AnsweredAt: "t2",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	createCalls2 := 0
	for _, r := range rec.reqs {
		if r.Method == http.MethodPut && !strings.Contains(r.URL, "/points") {
			createCalls2++
		}
	}
	// The collection-create PUT count MUST remain equal to 1 — a cache miss
	// would double-create it.
	if createCalls2 != createCalls1 {
		t.Errorf("second call re-created the collection: before=%d after=%d", createCalls1, createCalls2)
	}
}

func TestUserDecisionStore_Upsert_SeparateProductsGetSeparateCollections(t *testing.T) {
	srv, rec := newFakeQdrant(t)
	defer srv.Close()

	store := NewUserDecisionStore(srv.URL, &stubEmbedder{dim: 768})
	productA := uuid.New()
	productB := uuid.New()

	if _, err := store.UpsertUserDecision(context.Background(), productA, UserDecisionPoint{
		InteractionID: uuid.New(), WorkItemID: uuid.New(),
		Question: "q", Answer: "a",
	}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := store.UpsertUserDecision(context.Background(), productB, UserDecisionPoint{
		InteractionID: uuid.New(), WorkItemID: uuid.New(),
		Question: "q", Answer: "a",
	}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	expectedA := "/collections/product_" + productA.String() + "_user_decisions"
	expectedB := "/collections/product_" + productB.String() + "_user_decisions"
	sawA := false
	sawB := false
	for _, r := range rec.reqs {
		if r.Method == http.MethodPut && r.URL == expectedA {
			sawA = true
		}
		if r.Method == http.MethodPut && r.URL == expectedB {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Errorf("expected creates for both products; sawA=%v sawB=%v; requests=%+v", sawA, sawB, rec.reqs)
	}
}

// Sanity: the real vector package is reachable from this package.
func TestUserDecisionStore_UsesVectorDocumentType(t *testing.T) {
	_ = vector.Document{} // just asserts the import resolves for the implementation.
}
