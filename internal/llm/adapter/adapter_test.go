package adapter

import (
	"testing"
)

func TestNewFromConfigOpenAI(t *testing.T) {
	a, err := NewFromConfig("openai", "", "sk-test", "gpt-4o")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil")
	}
	if _, ok := a.(*openAIAdapter); !ok {
		t.Errorf("expected *openAIAdapter, got %T", a)
	}
}

func TestNewFromConfigVLLM(t *testing.T) {
	a, err := NewFromConfig("vllm", "http://localhost:8000", "", "mistral-7b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := a.(*openAIAdapter); !ok {
		t.Errorf("vllm should return *openAIAdapter, got %T", a)
	}
}

func TestNewFromConfigLMStudio(t *testing.T) {
	a, err := NewFromConfig("lmstudio", "http://localhost:1234", "", "phi-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := a.(*openAIAdapter); !ok {
		t.Errorf("lmstudio should return *openAIAdapter, got %T", a)
	}
}

func TestNewFromConfigAnthropic(t *testing.T) {
	a, err := NewFromConfig("anthropic", "", "sk-ant-test", "claude-3-5-sonnet-20241022")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := a.(*anthropicAdapter); !ok {
		t.Errorf("expected *anthropicAdapter, got %T", a)
	}
}

func TestNewFromConfigOllama(t *testing.T) {
	a, err := NewFromConfig("ollama", "", "", "llama3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := a.(*ollamaAdapter); !ok {
		t.Errorf("expected *ollamaAdapter, got %T", a)
	}
}

func TestNewFromConfigCustom(t *testing.T) {
	a, err := NewFromConfig("custom", "http://my-llm.internal", "key", "custom-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := a.(*customAdapter); !ok {
		t.Errorf("expected *customAdapter, got %T", a)
	}
}

func TestNewFromConfigUnknownProvider(t *testing.T) {
	_, err := NewFromConfig("unknown-provider", "", "", "model")
	if err == nil {
		t.Error("expected error for unknown provider, got nil")
	}
}

func TestNewFromConfigEmptyProvider(t *testing.T) {
	_, err := NewFromConfig("", "", "", "model")
	if err == nil {
		t.Error("expected error for empty provider, got nil")
	}
}
