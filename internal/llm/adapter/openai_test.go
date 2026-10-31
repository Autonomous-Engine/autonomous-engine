package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewOpenAIDefaultEndpoint(t *testing.T) {
	a := NewOpenAI("", "sk-test", "gpt-4o")
	oa := a.(*openAIAdapter)
	if oa.endpoint != "https://api.openai.com/v1" {
		t.Errorf("endpoint = %q, want %q", oa.endpoint, "https://api.openai.com/v1")
	}
}

func TestNewOpenAICustomEndpoint(t *testing.T) {
	a := NewOpenAI("http://localhost:8000/v1/", "key", "model")
	oa := a.(*openAIAdapter)
	// trailing slash should be trimmed
	if oa.endpoint != "http://localhost:8000/v1" {
		t.Errorf("endpoint = %q, want trailing slash trimmed", oa.endpoint)
	}
}

func TestOpenAICapabilities(t *testing.T) {
	a := NewOpenAI("", "key", "gpt-4o")
	caps := a.Capabilities()
	if caps.MaxContextTokens != 128000 {
		t.Errorf("MaxContextTokens = %d, want 128000", caps.MaxContextTokens)
	}
	if !caps.SupportsJSON {
		t.Error("SupportsJSON should be true")
	}
	if !caps.SupportsStreaming {
		t.Error("SupportsStreaming should be true")
	}
	if !caps.SupportsFunctions {
		t.Error("SupportsFunctions should be true")
	}
}

func TestOpenAIName(t *testing.T) {
	a := NewOpenAI("", "key", "gpt-4o")
	if a.Name() != "openai/gpt-4o" {
		t.Errorf("Name() = %q, want %q", a.Name(), "openai/gpt-4o")
	}
}

func TestOpenAICompleteParsesResponse(t *testing.T) {
	cannedResp := openAIResponse{
		ID:    "chatcmpl-123",
		Model: "gpt-4o",
		Choices: []openAIChoice{
			{
				Message:      &openAIMessage{Role: "assistant", Content: "Hello, world!"},
				FinishReason: "stop",
			},
		},
		Usage: openAIUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cannedResp)
	}))
	defer srv.Close()

	a := NewOpenAI(srv.URL, "sk-test", "gpt-4o")
	resp, err := a.Complete(context.Background(), CompletionRequest{
		Messages:  []ChatMessage{{Role: RoleUser, Content: "Say hello"}},
		MaxTokens: 20,
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.Content != "Hello, world!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello, world!")
	}
	if resp.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", resp.InputTokens)
	}
	if resp.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", resp.OutputTokens)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "stop")
	}
}

func TestOpenAICompleteReturnsErrorOnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	a := NewOpenAI(srv.URL, "bad-key", "gpt-4o")
	_, err := a.Complete(context.Background(), CompletionRequest{
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Error("expected error for 401 response, got nil")
	}
}
