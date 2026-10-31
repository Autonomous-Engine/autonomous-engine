package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewAnthropicDefaultEndpoint(t *testing.T) {
	a := NewAnthropic("", "sk-ant-test", "claude-3-5-sonnet-20241022")
	aa := a.(*anthropicAdapter)
	if aa.endpoint != "https://api.anthropic.com" {
		t.Errorf("endpoint = %q, want %q", aa.endpoint, "https://api.anthropic.com")
	}
}

func TestNewAnthropicCustomEndpoint(t *testing.T) {
	a := NewAnthropic("http://proxy.internal/", "key", "claude-3-haiku-20240307")
	aa := a.(*anthropicAdapter)
	if aa.endpoint != "http://proxy.internal" {
		t.Errorf("endpoint = %q, want trailing slash trimmed", aa.endpoint)
	}
}

func TestAnthropicCapabilities(t *testing.T) {
	a := NewAnthropic("", "key", "claude-3-5-sonnet-20241022")
	caps := a.Capabilities()
	if caps.MaxContextTokens != 200000 {
		t.Errorf("MaxContextTokens = %d, want 200000", caps.MaxContextTokens)
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

func TestAnthropicName(t *testing.T) {
	a := NewAnthropic("", "key", "claude-3-5-sonnet-20241022")
	if a.Name() != "anthropic/claude-3-5-sonnet-20241022" {
		t.Errorf("Name() = %q", a.Name())
	}
}

func TestAnthropicCompleteParsesResponse(t *testing.T) {
	cannedResp := anthropicResponse{
		ID:    "msg-123",
		Model: "claude-3-5-sonnet-20241022",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "Hello from Claude!"},
		},
		Usage:      anthropicUsage{InputTokens: 12, OutputTokens: 6},
		StopReason: "end_turn",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cannedResp)
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "sk-ant-test", "claude-3-5-sonnet-20241022")
	resp, err := a.Complete(context.Background(), CompletionRequest{
		Messages:  []ChatMessage{{Role: RoleUser, Content: "Say hello"}},
		MaxTokens: 20,
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.Content != "Hello from Claude!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from Claude!")
	}
	if resp.InputTokens != 12 {
		t.Errorf("InputTokens = %d, want 12", resp.InputTokens)
	}
	if resp.OutputTokens != 6 {
		t.Errorf("OutputTokens = %d, want 6", resp.OutputTokens)
	}
	if resp.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want end_turn", resp.FinishReason)
	}
}

func TestAnthropicCompleteMultipleContentBlocks(t *testing.T) {
	// Verify that multiple text blocks are concatenated
	cannedResp := anthropicResponse{
		ID:    "msg-456",
		Model: "claude-3-5-sonnet-20241022",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "Part one. "},
			{Type: "text", Text: "Part two."},
		},
		Usage:      anthropicUsage{InputTokens: 5, OutputTokens: 8},
		StopReason: "end_turn",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(cannedResp)
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "key", "claude-3-5-sonnet-20241022")
	resp, err := a.Complete(context.Background(), CompletionRequest{
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp.Content != "Part one. Part two." {
		t.Errorf("Content = %q, want %q", resp.Content, "Part one. Part two.")
	}
}

func TestAnthropicCompleteReturnsErrorOnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid API key"}}`))
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "bad-key", "claude-3-5-sonnet-20241022")
	_, err := a.Complete(context.Background(), CompletionRequest{
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Error("expected error for 401 response, got nil")
	}
}
