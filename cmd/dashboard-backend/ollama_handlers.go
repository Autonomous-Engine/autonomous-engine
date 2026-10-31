package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OllamaHandlers groups HTTP handlers for the /api/admin/ollama/* routes.
// It proxies requests to a locally-running Ollama instance, resolving the
// target endpoint from the credential store, an environment variable, or
// the Docker-to-host default.
type OllamaHandlers struct {
	db    *pgxpool.Pool
	store *configstore.ConfigStore
}

// NewOllamaHandlers returns an OllamaHandlers backed by the given pool and
// config store.
func NewOllamaHandlers(db *pgxpool.Pool, store *configstore.ConfigStore) *OllamaHandlers {
	return &OllamaHandlers{db: db, store: store}
}

// resolveEndpoint returns the Ollama base URL, walking: credential store →
// Docker-to-host default. The OLLAMA_ENDPOINT env var is no longer consulted;
// configure the endpoint via Admin → Models in the dashboard.
func (oh *OllamaHandlers) resolveEndpoint(ctx context.Context) string {
	if oh.store != nil {
		ep, err := oh.store.ResolveCredential(ctx, "llm", "endpoint", "", "")
		if err == nil && ep != "" {
			return ep
		}
	}
	return "http://host.docker.internal:11434"
}

// ─── OllamaStatus ────────────────────────────────────────────────────────────

// OllamaStatus handles GET /api/admin/ollama/status.
// It pings the Ollama /api/tags endpoint and reports reachability.
func (oh *OllamaHandlers) OllamaStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	endpoint := oh.resolveEndpoint(r.Context())
	url := endpoint + "/api/tags"

	resp, err := http.Get(url) //nolint:gosec,noctx
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"reachable":   false,
			"endpoint":    endpoint,
			"model_count": 0,
		})
		return
	}
	defer resp.Body.Close()

	var tagsResp struct {
		Models []json.RawMessage `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tagsResp)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reachable":   resp.StatusCode == http.StatusOK,
		"endpoint":    endpoint,
		"model_count": len(tagsResp.Models),
	})
}

// ─── ListModels ───────────────────────────────────────────────────────────────

// ListModels handles GET /api/admin/ollama/models.
// Proxies to GET {ollama}/api/tags and returns the model list.
func (oh *OllamaHandlers) ListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	endpoint := oh.resolveEndpoint(r.Context())
	resp, err := http.Get(endpoint + "/api/tags") //nolint:gosec,noctx
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("cannot reach Ollama at %s: %v", endpoint, err))
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read Ollama response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// ─── PullModel ────────────────────────────────────────────────────────────────

// pullRequest is the request body for PullModel.
type pullRequest struct {
	Model string `json:"model"`
}

// PullModel handles POST /api/admin/ollama/pull.
// Streams download progress back to the client as Server-Sent Events.
func (oh *OllamaHandlers) PullModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req pullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeError(w, http.StatusBadRequest, "body must be {\"model\": \"<name>\"}")
		return
	}

	endpoint := oh.resolveEndpoint(r.Context())
	ollamaURL := endpoint + "/api/pull"

	payload, _ := json.Marshal(map[string]interface{}{
		"name":   req.Model,
		"stream": true,
	})

	ollamaReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, ollamaURL, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build request")
		return
	}
	ollamaReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(ollamaReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("cannot reach Ollama: %v", err))
		return
	}
	defer resp.Body.Close()

	// Set SSE headers before writing the first byte.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		if canFlush {
			flusher.Flush()
		}
	}
}

// ─── DeleteModel ─────────────────────────────────────────────────────────────

// DeleteModel handles DELETE /api/admin/ollama/models?name={model}.
// Proxies to DELETE {ollama}/api/delete.
func (oh *OllamaHandlers) DeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}

	endpoint := oh.resolveEndpoint(r.Context())
	ollamaURL := endpoint + "/api/delete"

	payload, _ := json.Marshal(map[string]string{"name": name})

	ollamaReq, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, ollamaURL, bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build request")
		return
	}
	ollamaReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(ollamaReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("cannot reach Ollama: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "model": name})
		return
	}

	body, _ := io.ReadAll(resp.Body)
	writeError(w, resp.StatusCode, string(body))
}

// ─── RunningModels ────────────────────────────────────────────────────────────

// RunningModels handles GET /api/admin/ollama/running.
// Proxies to GET {ollama}/api/ps — models currently loaded in VRAM.
func (oh *OllamaHandlers) RunningModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	endpoint := oh.resolveEndpoint(r.Context())
	resp, err := http.Get(endpoint + "/api/ps") //nolint:gosec,noctx
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("cannot reach Ollama at %s: %v", endpoint, err))
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read Ollama response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
