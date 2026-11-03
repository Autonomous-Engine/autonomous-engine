package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// probeResult is the answer to POST /api/admin/providers/probe: whether an
// endpoint answers and which models it serves, before any provider row
// exists. The onboarding wizard needs it to let the user pick a model
// (F13); the saved-provider model listing needs an id.
type probeResult struct {
	Reachable bool     `json:"reachable"`
	LatencyMS int64    `json:"latency_ms"`
	Error     *string  `json:"error"`
	Models    []string `json:"models"`
}

type probeRequest struct {
	ProviderType string `json:"provider_type"`
	Endpoint     string `json:"endpoint"`
	APIKey       string `json:"api_key"`
}

func probeError(msg string) probeResult {
	return probeResult{Error: &msg, Models: []string{}}
}

// probeProvider checks the endpoint and lists its models. Ollama answers
// /api/tags; an OpenAI-compatible endpoint answers /models; other types
// are health-checked through their adapter and list nothing.
func probeProvider(ctx context.Context, provType, endpoint, apiKey string) probeResult {
	provType = strings.ToLower(strings.TrimSpace(provType))
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if provType == "" {
		return probeError("provider_type is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()

	switch provType {
	case "ollama":
		if endpoint == "" {
			endpoint = "http://localhost:11434"
		}
		body, err := probeGet(ctx, endpoint+"/api/tags", "")
		if err != nil {
			return probeError(fmt.Sprintf("cannot reach Ollama at %s: %v", endpoint, err))
		}
		var tags struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &tags); err != nil {
			return probeError("Ollama answered, but /api/tags was not JSON: " + err.Error())
		}
		models := make([]string, 0, len(tags.Models))
		for _, m := range tags.Models {
			if m.Name != "" {
				models = append(models, m.Name)
			}
		}
		return probeResult{Reachable: true, LatencyMS: time.Since(start).Milliseconds(), Models: models}
	case "openai", "vllm", "custom":
		if endpoint == "" {
			return probeError("endpoint is required")
		}
		body, err := probeGet(ctx, endpoint+"/models", apiKey)
		if err != nil {
			return probeError(fmt.Sprintf("cannot list models at %s/models: %v", endpoint, err))
		}
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		models := []string{}
		if json.Unmarshal(body, &list) == nil {
			for _, m := range list.Data {
				if m.ID != "" {
					models = append(models, m.ID)
				}
			}
		}
		return probeResult{Reachable: true, LatencyMS: time.Since(start).Milliseconds(), Models: models}
	default:
		a, err := adapter.NewFromConfig(provType, endpoint, apiKey, "")
		if err != nil {
			return probeError(err.Error())
		}
		if err := a.HealthCheck(ctx); err != nil {
			return probeError(err.Error())
		}
		return probeResult{Reachable: true, LatencyMS: time.Since(start).Milliseconds(), Models: []string{}}
	}
}

func probeGet(ctx context.Context, url, bearer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// Probe handles POST /api/admin/providers/probe. It always answers 200
// with reachable/error so the UI shows the reason rather than a failed
// request.
func (ph *ProviderHandlers) Probe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req probeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	writeJSON(w, http.StatusOK, probeProvider(r.Context(), req.ProviderType, req.Endpoint, req.APIKey))
}
