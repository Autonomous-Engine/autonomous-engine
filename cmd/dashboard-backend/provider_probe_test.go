package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The wizard needs to test an endpoint and list its models before a
// provider row exists; the only model listing took a saved provider id
// (F13, F14).
func TestProbeProviderListsOllamaModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3.6:35b","size":1},{"name":"nomic-embed-text:latest","size":2}]}`))
	}))
	defer srv.Close()

	res := probeProvider(context.Background(), "ollama", srv.URL, "")
	if !res.Reachable || res.Error != nil {
		t.Fatalf("res = %+v, want reachable", res)
	}
	if len(res.Models) != 2 || res.Models[0] != "qwen3.6:35b" || res.Models[1] != "nomic-embed-text:latest" {
		t.Fatalf("models = %v", res.Models)
	}
}

func TestProbeProviderReportsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	res := probeProvider(context.Background(), "ollama", url, "")
	if res.Reachable || res.Error == nil || *res.Error == "" {
		t.Fatalf("res = %+v, want unreachable with an error", res)
	}
	if len(res.Models) != 0 {
		t.Fatalf("models = %v, want none", res.Models)
	}
}

func TestProbeProviderRejectsUnknownType(t *testing.T) {
	res := probeProvider(context.Background(), "", "http://127.0.0.1:1", "")
	if res.Reachable || res.Error == nil {
		t.Fatalf("res = %+v, want an error for a missing provider type", res)
	}
}
