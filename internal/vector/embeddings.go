package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EmbeddingService generates text embeddings against a configured embedding
// provider. Supports multiple provider families:
//
//   - "openai_compatible" (default): OpenAI, Azure OpenAI, vLLM, LM Studio,
//     and anything else that speaks the OpenAI /embeddings contract. One
//     HTTP call embeds N texts in a batch.
//   - "openai": alias of openai_compatible.
//   - "azure_openai": alias of openai_compatible.
//   - "ollama": Ollama's native embeddings API. POSTs to /api/embeddings
//     with {"model","prompt"} per-text and reads {"embedding":[...]} back.
//     Ollama does NOT support batch, so EmbedBatch fans out sequentially.
//
// providerType selection matters because the URL path, request body shape,
// and response shape differ between the two families. Silent misconfiguration
// previously masqueraded as "the embedding API returned no data" — this type
// makes the path explicit.
type EmbeddingService struct {
	providerType string // "openai_compatible" | "openai" | "azure_openai" | "ollama"
	endpoint     string // full base URL, e.g. "https://api.openai.com/v1" or "http://host:11434"
	apiKey       string // optional; Bearer-authed for non-Ollama providers
	model        string // embedding model name
	httpClient   *http.Client
}

// NewEmbeddingService creates an EmbeddingService defaulting to the
// "openai_compatible" provider family. Retained for backwards compatibility
// with callers that predate the provider-type split. New callers should use
// NewEmbeddingServiceWithType.
func NewEmbeddingService(endpoint, apiKey, model string) *EmbeddingService {
	return NewEmbeddingServiceWithType("openai_compatible", endpoint, apiKey, model)
}

// NewEmbeddingServiceWithType creates an EmbeddingService for a specific
// provider family. An empty providerType defaults to "openai_compatible".
// An empty model defaults to "text-embedding-3-small" — a reasonable
// placeholder for openai-family providers; callers should always pass an
// explicit model in practice.
func NewEmbeddingServiceWithType(providerType, endpoint, apiKey, model string) *EmbeddingService {
	if providerType == "" {
		providerType = "openai_compatible"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	return &EmbeddingService{
		providerType: providerType,
		endpoint:     endpoint,
		apiKey:       apiKey,
		model:        model,
		httpClient:   &http.Client{},
	}
}

// ProviderType reports the family this service was constructed for. Exposed
// so callers (e.g. dashboard handlers) can log/display the effective type
// without duplicating the string.
func (es *EmbeddingService) ProviderType() string { return es.providerType }

// Model reports the configured model name.
func (es *EmbeddingService) Model() string { return es.model }

// ─── OpenAI-compatible request/response shapes ────────────────────────────

// openAIEmbeddingRequest is the JSON body sent to an OpenAI-compatible
// /embeddings endpoint.
type openAIEmbeddingRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string or []string
}

// openAIEmbeddingResponse is the JSON body returned by an OpenAI-compatible
// /embeddings endpoint.
type openAIEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ─── Ollama request/response shapes ───────────────────────────────────────

// ollamaEmbeddingRequest is the JSON body sent to Ollama's /api/embeddings
// endpoint. Note the singular "prompt" (not "input") and no batch support.
type ollamaEmbeddingRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// ollamaEmbeddingResponse is the JSON body returned by Ollama. Note the
// singular "embedding" (not "data[].embedding") and no index wrapper.
type ollamaEmbeddingResponse struct {
	Embedding []float32 `json:"embedding"`
	Error     string    `json:"error,omitempty"`
}

// ─── Public API ───────────────────────────────────────────────────────────

// Embed generates an embedding vector for a single text string.
func (es *EmbeddingService) Embed(ctx context.Context, text string) ([]float32, error) {
	results, err := es.embedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("embeddings: no data returned for single input")
	}
	return results[0], nil
}

// EmbedBatch generates embedding vectors for a slice of text strings. For
// OpenAI-compatible providers this is a single HTTP call. For Ollama it is
// fanned out into len(texts) calls because Ollama has no batch endpoint.
func (es *EmbeddingService) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results, err := es.embedBatch(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(results) != len(texts) {
		return nil, fmt.Errorf("embeddings: expected %d results, got %d", len(texts), len(results))
	}
	return results, nil
}

// Dimension returns the vector length produced by the configured provider
// and model. Implemented as a single embed of the word "test" so it works
// uniformly across provider families. Intended for boot-time validation and
// for the admin UI's "Test" button, which stores the returned dimension on
// the provider row.
func (es *EmbeddingService) Dimension(ctx context.Context) (int, error) {
	vec, err := es.Embed(ctx, "test")
	if err != nil {
		return 0, fmt.Errorf("embeddings: dimension probe: %w", err)
	}
	return len(vec), nil
}

// ─── Internal dispatch ────────────────────────────────────────────────────

// embedBatch dispatches to the correct provider-specific implementation.
func (es *EmbeddingService) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	switch es.providerType {
	case "ollama":
		return es.embedOllamaLoop(ctx, texts)
	default:
		// "openai", "azure_openai", "openai_compatible", and anything else
		// defaulted by NewEmbeddingServiceWithType all land here.
		return es.embedOpenAIBatch(ctx, texts)
	}
}

// embedOpenAIBatch sends a single batched /embeddings request.
func (es *EmbeddingService) embedOpenAIBatch(ctx context.Context, texts []string) ([][]float32, error) {
	// OpenAI accepts either a string or []string for "input". Using the
	// slice form works identically for a single-text call.
	reqBody, err := json.Marshal(openAIEmbeddingRequest{
		Model: es.model,
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: marshal request: %w", err)
	}

	url := es.endpoint + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("embeddings: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if es.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+es.apiKey)
	}

	resp, err := es.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: POST %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embeddings: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIEmbeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("embeddings: decode response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("embeddings: API error: %s", result.Error.Message)
	}

	// Sort by index to guarantee ordering matches the input.
	vectors := make([][]float32, len(result.Data))
	for _, item := range result.Data {
		if item.Index >= 0 && item.Index < len(vectors) {
			vectors[item.Index] = item.Embedding
		}
	}
	return vectors, nil
}

// embedOllamaLoop fans out len(texts) sequential calls to Ollama's
// /api/embeddings. Ollama's embedding endpoint has no batch form — each
// request embeds exactly one prompt. We preserve input order by writing each
// result into its original slot before returning.
//
// A single failure aborts the batch with that error; partial results are
// never returned. The indexer uses per-chunk error handling at a higher
// level, so one bad chunk does not poison the whole re-index run.
func (es *EmbeddingService) embedOllamaLoop(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := es.embedOllamaOne(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("embeddings: ollama prompt %d/%d: %w", i+1, len(texts), err)
		}
		out[i] = vec
	}
	return out, nil
}

// embedOllamaOne performs a single POST to /api/embeddings.
func (es *EmbeddingService) embedOllamaOne(ctx context.Context, text string) ([]float32, error) {
	reqBody, err := json.Marshal(ollamaEmbeddingRequest{
		Model:  es.model,
		Prompt: text,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := es.endpoint + "/api/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Ollama doesn't require auth by default, but if an API key is set
	// (e.g. Ollama behind a reverse-proxy doing bearer auth) we send it.
	if es.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+es.apiKey)
	}

	resp, err := es.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var result ollamaEmbeddingResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("ollama error: %s", result.Error)
	}
	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned empty embedding (model=%s) — pull the model with `ollama pull %s` on the ollama host", es.model, es.model)
	}
	return result.Embedding, nil
}
