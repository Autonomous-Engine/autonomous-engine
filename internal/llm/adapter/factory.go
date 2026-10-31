package adapter

import "fmt"

// NewFromConfig constructs a ModelAdapter from the provided provider string,
// endpoint, API key, and model name.
//
// Supported provider values:
//   - "openai", "vllm", "lmstudio" → OpenAI-compatible adapter
//   - "anthropic"                   → Anthropic Messages API adapter
//   - "ollama"                      → Ollama REST API adapter
//   - "custom"                      → Generic OpenAI-compatible HTTP adapter
func NewFromConfig(provider, endpoint, apiKey, model string) (ModelAdapter, error) {
	switch provider {
	case "openai", "vllm", "lmstudio":
		return NewOpenAI(endpoint, apiKey, model), nil
	case "anthropic":
		return NewAnthropic(endpoint, apiKey, model), nil
	case "ollama":
		return NewOllama(endpoint, model), nil
	case "custom":
		return NewCustom(endpoint, apiKey, model), nil
	default:
		return nil, fmt.Errorf("adapter: unknown provider %q", provider)
	}
}
