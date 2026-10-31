package vector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewCreatesClientWithCorrectFields(t *testing.T) {
	vs := New("http://localhost:6333", "my-collection")
	if vs == nil {
		t.Fatal("New returned nil")
	}
	if vs.endpoint != "http://localhost:6333" {
		t.Errorf("endpoint = %q, want %q", vs.endpoint, "http://localhost:6333")
	}
	if vs.collection != "my-collection" {
		t.Errorf("collection = %q, want %q", vs.collection, "my-collection")
	}
	if vs.httpClient == nil {
		t.Error("httpClient is nil")
	}
}

func TestDocumentJSONSerialization(t *testing.T) {
	doc := Document{
		ID:      "doc-1",
		Content: "some text content",
		Metadata: map[string]string{
			"source": "file.go",
			"type":   "function",
		},
		Vector: []float32{0.1, 0.2, 0.3},
	}

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Document
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != doc.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, doc.ID)
	}
	if decoded.Content != doc.Content {
		t.Errorf("Content = %q, want %q", decoded.Content, doc.Content)
	}
	if decoded.Metadata["source"] != "file.go" {
		t.Errorf("Metadata[source] = %q, want %q", decoded.Metadata["source"], "file.go")
	}
	if len(decoded.Vector) != 3 {
		t.Errorf("Vector len = %d, want 3", len(decoded.Vector))
	}
}

func TestSearchResultJSONSerialization(t *testing.T) {
	sr := SearchResult{
		ID:      "result-1",
		Score:   0.95,
		Content: "matched content",
		Metadata: map[string]string{
			"file": "main.go",
		},
	}

	data, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SearchResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != sr.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, sr.ID)
	}
	if decoded.Score != sr.Score {
		t.Errorf("Score = %v, want %v", decoded.Score, sr.Score)
	}
	if decoded.Content != sr.Content {
		t.Errorf("Content = %q, want %q", decoded.Content, sr.Content)
	}
}

func TestSearchParsesResponse(t *testing.T) {
	mockResp := `{
		"result": [
			{"id": "abc", "score": 0.92, "payload": {"content": "hello world", "lang": "go"}},
			{"id": "def", "score": 0.80, "payload": {"content": "second result"}}
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockResp))
	}))
	defer srv.Close()

	vs := New(srv.URL, "test-col")
	results, err := vs.Search(context.Background(), []float32{0.1, 0.2}, nil, 10)
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].ID != "abc" {
		t.Errorf("results[0].ID = %q, want %q", results[0].ID, "abc")
	}
	if results[0].Content != "hello world" {
		t.Errorf("results[0].Content = %q, want %q", results[0].Content, "hello world")
	}
	// "lang" key should be in metadata, not content
	if results[0].Metadata["lang"] != "go" {
		t.Errorf("results[0].Metadata[lang] = %q, want %q", results[0].Metadata["lang"], "go")
	}
	if results[1].Score != 0.80 {
		t.Errorf("results[1].Score = %v, want 0.80", results[1].Score)
	}
}

func TestSearchReturnsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	vs := New(srv.URL, "test-col")
	_, err := vs.Search(context.Background(), []float32{0.1}, nil, 5)
	if err == nil {
		t.Error("expected error on 500 response, got nil")
	}
}
