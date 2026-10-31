package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewOllamaDefaultEndpoint(t *testing.T) {
	a := NewOllama("", "llama3")
	oa := a.(*ollamaAdapter)
	if oa.endpoint != "http://localhost:11434" {
		t.Errorf("endpoint = %q, want %q", oa.endpoint, "http://localhost:11434")
	}
}

func TestNewOllamaCustomEndpoint(t *testing.T) {
	a := NewOllama("http://gpu-server:11434/", "mistral")
	oa := a.(*ollamaAdapter)
	if oa.endpoint != "http://gpu-server:11434" {
		t.Errorf("endpoint = %q, want trailing slash trimmed", oa.endpoint)
	}
}

func TestOllamaCapabilities(t *testing.T) {
	a := NewOllama("", "llama3")
	caps := a.Capabilities()
	if caps.MaxContextTokens != 32768 {
		t.Errorf("MaxContextTokens = %d, want 32768", caps.MaxContextTokens)
	}
	if !caps.SupportsJSON {
		t.Error("SupportsJSON should be true")
	}
	if !caps.SupportsStreaming {
		t.Error("SupportsStreaming should be true")
	}
	if caps.SupportsFunctions {
		t.Error("SupportsFunctions should be false for Ollama")
	}
}

func TestOllamaName(t *testing.T) {
	a := NewOllama("", "llama3")
	if a.Name() != "ollama/llama3" {
		t.Errorf("Name() = %q, want %q", a.Name(), "ollama/llama3")
	}
}

func TestOllamaCompleteParsesResponse(t *testing.T) {
	cannedResp := ollamaChatResponse{
		Model:           "llama3",
		Message:         ollamaMessage{Role: "assistant", Content: "Hello from Ollama!"},
		Done:            true,
		PromptEvalCount: 8,
		EvalCount:       5,
		DoneReason:      "stop",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cannedResp)
	}))
	defer srv.Close()

	a := NewOllama(srv.URL, "llama3")
	resp, err := a.Complete(context.Background(), CompletionRequest{
		Messages:  []ChatMessage{{Role: RoleUser, Content: "Say hello"}},
		MaxTokens: 50,
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.Content != "Hello from Ollama!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from Ollama!")
	}
	if resp.InputTokens != 8 {
		t.Errorf("InputTokens = %d, want 8", resp.InputTokens)
	}
	if resp.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", resp.OutputTokens)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "stop")
	}
}

func TestOllamaCompleteReturnsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("model not found"))
	}))
	defer srv.Close()

	a := NewOllama(srv.URL, "unknown-model")
	_, err := a.Complete(context.Background(), CompletionRequest{
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Error("expected error on non-200 response, got nil")
	}
}

func TestOllamaHealthCheckSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := NewOllama(srv.URL, "llama3")
	if err := a.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck returned error: %v", err)
	}
}
