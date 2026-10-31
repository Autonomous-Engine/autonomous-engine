package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
)

// fakeQdrantSearch returns a fixed hit for any search.
func fakeQdrantSearch(t *testing.T, workItemID uuid.UUID) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Match the qdrant search endpoint pattern used by VectorStore.Search.
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/points/search") {
			// Return one hit with the metadata our upsert sets.
			body := `{
				"result": [
					{
						"id": "pt-1",
						"score": 0.98,
						"payload": {
							"question":        "what id type?",
							"answer":          "use GUIDs",
							"classified_as":   "codex_antipattern",
							"work_item_id":    "` + workItemID.String() + `",
							"answered_at":     "2026-04-23T12:00:00Z",
							"revoked":         "false"
						}
					}
				]
			}`
			_, _ = w.Write([]byte(body))
			return
		}
		// Collection describe — 200 with existing collection
		_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
	}))
}

// fakeEmbedServer used as EmbeddingService replacement in tests. VectorStore's
// SearchByText calls EmbeddingService.Embed — we can't sub an interface
// because VectorStore takes a concrete *EmbeddingService. Instead, we rely
// on a test-only fake qdrant responding to /points/search without requiring
// a real embedding (the mock server doesn't care about the vector it receives).
//
// To avoid actually calling an embeddings API, point the embedding service at
// the same fake server and let any /embeddings request succeed.
func fakeEmbedServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond to embeddings + qdrant both.
		if strings.Contains(r.URL.Path, "/points/search") {
			_, _ = w.Write([]byte(`{"result":[]}`))
			return
		}
		if strings.Contains(r.URL.Path, "/embeddings") || strings.Contains(r.URL.Path, "/embed") {
			// OpenAI-style response: 768-dim zero vector
			var sb strings.Builder
			sb.WriteString(`{"data":[{"embedding":[`)
			for i := 0; i < 768; i++ {
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString("0.1")
			}
			sb.WriteString(`]}]}`)
			_, _ = w.Write([]byte(sb.String()))
			return
		}
		// Collection describe
		_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}}}}}`))
	}))
}

func TestPastDecisionsQuery_RejectsGlobalScope(t *testing.T) {
	e := &Executor{
		qdrantEndpoint: "http://fake",
		embeddingSvc:   vector.NewEmbeddingService("http://fake", "", "test"),
	}
	prodID := uuid.New()
	wiID := uuid.New()
	e.productID = &prodID
	e.workItemID = &wiID

	res := e.pastDecisionsQuery(context.Background(), "c1", map[string]any{
		"query": "q", "scope": "global",
	})
	if !res.IsError {
		t.Error("global scope should error in v1")
	}
}

func TestPastDecisionsQuery_RejectsMissingQuery(t *testing.T) {
	e := &Executor{
		qdrantEndpoint: "http://fake",
		embeddingSvc:   vector.NewEmbeddingService("http://fake", "", "test"),
	}
	prodID := uuid.New()
	e.productID = &prodID
	res := e.pastDecisionsQuery(context.Background(), "c1", map[string]any{"scope": "product"})
	if !res.IsError {
		t.Error("missing query should error")
	}
}

func TestPastDecisionsQuery_RequiresQdrantConfig(t *testing.T) {
	e := &Executor{} // no qdrantEndpoint / embeddingSvc
	res := e.pastDecisionsQuery(context.Background(), "c1", map[string]any{
		"query": "q", "scope": "product",
	})
	if !res.IsError {
		t.Error("missing qdrant config should error")
	}
}

func TestPastDecisionsQuery_ReturnsDecisionsFromSearch(t *testing.T) {
	wiID := uuid.New()
	srv := fakeQdrantSearch(t, wiID)
	defer srv.Close()
	embSrv := fakeEmbedServer()
	defer embSrv.Close()

	e := &Executor{
		qdrantEndpoint: srv.URL,
		embeddingSvc:   vector.NewEmbeddingService(embSrv.URL, "test", "test-model"),
	}
	prodID := uuid.New()
	e.productID = &prodID
	e.workItemID = &wiID

	res := e.pastDecisionsQuery(context.Background(), "c1", map[string]any{
		"query": "id type", "scope": "product", "limit": float64(5),
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	var decisions []interactions.Decision
	if err := json.Unmarshal([]byte(res.Content), &decisions); err != nil {
		t.Fatalf("content not JSON array: %v (%s)", err, res.Content)
	}
	if len(decisions) != 1 {
		t.Fatalf("want 1 decision, got %d", len(decisions))
	}
	if decisions[0].Question != "what id type?" {
		t.Errorf("question = %q", decisions[0].Question)
	}
	if decisions[0].ClassifiedAs != interactions.ClassifiedCodexAntipattern {
		t.Errorf("classified_as = %q", decisions[0].ClassifiedAs)
	}
	if decisions[0].SourceWorkItemID != wiID {
		t.Errorf("SourceWorkItemID = %v", decisions[0].SourceWorkItemID)
	}
}

func TestPastDecisionsQuery_ToolDefinition_CorrectName(t *testing.T) {
	if PastDecisionsQuery().Name != "past_decisions_query" {
		t.Errorf("tool name = %q", PastDecisionsQuery().Name)
	}
}
