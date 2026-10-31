package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/autonomousengine/autonomous-engine/internal/vector"
)

// TestCodexSimilarCodeFiltersByProductID verifies that codex_similar_code
// sends a payload.product_id filter to Qdrant, so results from other
// products cannot leak back into the caller's search. This is
// the product-isolation guarantee the benchmark relies on — if it regresses,
// a product could receive hits from another onboarded product.
func TestCodexSimilarCodeFiltersByProductID(t *testing.T) {
	productA := uuid.MustParse("26b55430-f5cf-46c7-b655-295f073bce05")

	// Fake Qdrant + embedder. The embedder returns a fixed vector so the
	// embed step succeeds; Qdrant inspects the filter block in the search
	// request body and echoes its decision back so the test can assert.
	var searchBody map[string]interface{}

	mux := http.NewServeMux()
	// Embedding endpoint (OpenAI-compatible).
	mux.HandleFunc("/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"data": []map[string]interface{}{
				{"embedding": []float32{0.1, 0.2, 0.3}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	// Qdrant search endpoint. Capture the request body, then return two
	// results — the assertion is on what we SENT, not what we got back.
	mux.HandleFunc("/collections/codebase/points/search", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		defer r.Body.Close() //nolint:errcheck
		_ = json.Unmarshal(raw, &searchBody)
		w.Header().Set("Content-Type", "application/json")
		resp := `{
			"result": [
				{"id": "p1", "score": 0.99, "payload": {"content": "product code", "product_id": "26b55430-f5cf-46c7-b655-295f073bce05", "file": "x.go"}}
			]
		}`
		_, _ = w.Write([]byte(resp))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	embSvc := vector.NewEmbeddingService(srv.URL, "test-key", "text-embedding-3-small")
	vs := vector.NewWithEmbeddings(srv.URL, "codebase", embSvc)

	exec := NewExecutorWithCodex("/tmp", nil, vs, &productA, nil, nil)
	res := exec.codexSimilarCode(context.Background(), "call-1", map[string]interface{}{
		"description": "location map rendering",
		"limit":       float64(5),
	})
	if res.IsError {
		t.Fatalf("codex_similar_code returned error: %s", res.Content)
	}

	// Pull the filter clause out of the captured search request.
	filter, ok := searchBody["filter"].(map[string]interface{})
	if !ok {
		t.Fatalf("search request had no filter block; full body = %v", searchBody)
	}
	must, ok := filter["must"].([]interface{})
	if !ok || len(must) == 0 {
		t.Fatalf("filter.must missing or empty: %v", filter)
	}
	found := false
	for _, entry := range must {
		m, _ := entry.(map[string]interface{})
		if m["key"] == "product_id" {
			match, _ := m["match"].(map[string]interface{})
			if match["value"] == productA.String() {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("expected filter.must to require product_id=%s, got %v", productA, must)
	}

	// Sanity: result should deserialize and include the returned hit.
	if !strings.Contains(res.Content, "x.go") {
		t.Errorf("expected hit payload to surface in result, got: %s", res.Content)
	}
}

// TestCodexSimilarCodeNoFilterWithoutProductID verifies legacy single-repo
// deployments (no productID threaded through) keep working — the tool
// falls back to an unfiltered search rather than erroring out.
func TestCodexSimilarCodeNoFilterWithoutProductID(t *testing.T) {
	var searchBody map[string]interface{}

	mux := http.NewServeMux()
	mux.HandleFunc("/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{{"embedding": []float32{0.1}}},
		})
	})
	mux.HandleFunc("/collections/codebase/points/search", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		defer r.Body.Close() //nolint:errcheck
		_ = json.Unmarshal(raw, &searchBody)
		_, _ = w.Write([]byte(`{"result":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	embSvc := vector.NewEmbeddingService(srv.URL, "test-key", "text-embedding-3-small")
	vs := vector.NewWithEmbeddings(srv.URL, "codebase", embSvc)

	exec := NewExecutorWithCodex("/tmp", nil, vs, nil, nil, nil)
	_ = exec.codexSimilarCode(context.Background(), "call-2", map[string]interface{}{
		"description": "anything",
	})

	if _, present := searchBody["filter"]; present {
		t.Errorf("expected no filter when productID is nil, got: %v", searchBody["filter"])
	}
}
