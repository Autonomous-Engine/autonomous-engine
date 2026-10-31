package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProviderHandlers groups HTTP handlers for the /api/admin/providers/* routes.
type ProviderHandlers struct {
	db    *pgxpool.Pool
	store *configstore.ConfigStore
}

// NewProviderHandlers returns a ProviderHandlers backed by the given pool and
// config store.
func NewProviderHandlers(db *pgxpool.Pool, store *configstore.ConfigStore) *ProviderHandlers {
	return &ProviderHandlers{db: db, store: store}
}

// guardDB is the pool as the M4-06 configuration guard's database.
func (ph *ProviderHandlers) guardDB() guardBeginner {
	if ph.db == nil {
		return nil
	}
	return ph.db
}

// ─── Domain types ─────────────────────────────────────────────────────────────

// providerRow is the DB row shape from llm_providers.
type providerRow struct {
	ID                 uuid.UUID       `json:"id"`
	Name               string          `json:"name"`
	ProviderType       string          `json:"provider_type"`
	Endpoint           string          `json:"endpoint"`
	APIKeyCredentialID *uuid.UUID      `json:"-"`
	HasAPIKey          bool            `json:"has_api_key"`
	IsDefault          bool            `json:"is_default"`
	Enabled            bool            `json:"enabled"`
	Settings           json.RawMessage `json:"settings"`
	CreatedAt          time.Time       `json:"created_at"`
}

// createProviderRequest is the body for POST /create.
type createProviderRequest struct {
	Name         string                 `json:"name"`
	ProviderType string                 `json:"provider_type"`
	Endpoint     string                 `json:"endpoint"`
	APIKey       string                 `json:"api_key"`
	Settings     map[string]interface{} `json:"settings"`
}

// updateProviderRequest is the body for PUT /update.
type updateProviderRequest struct {
	Name     string                 `json:"name"`
	Endpoint string                 `json:"endpoint"`
	APIKey   string                 `json:"api_key"`
	Settings map[string]interface{} `json:"settings"`
	Enabled  bool                   `json:"enabled"`
}

// ─── Hardcoded model lists ────────────────────────────────────────────────────

var anthropicModels = []string{
	"claude-opus-4-6",
	"claude-sonnet-4-6",
	"claude-haiku-4-5-20251001",
	"claude-opus-4-0-20250115",
	"claude-sonnet-4-20250514",
	"claude-3-5-sonnet-20241022",
	"claude-3-5-haiku-20241022",
	"claude-3-opus-20240229",
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// listProviders queries all providers and whether each has an API key stored.
func (ph *ProviderHandlers) listProviders(ctx context.Context) ([]providerRow, error) {
	rows, err := ph.db.Query(ctx, `
		SELECT p.id, p.name, p.provider_type, p.endpoint,
		       p.api_key_credential_id,
		       (p.api_key_credential_id IS NOT NULL) AS has_api_key,
		       p.is_default, p.enabled, p.settings, p.created_at
		FROM llm_providers p
		ORDER BY p.created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("query providers: %w", err)
	}
	defer rows.Close()

	var result []providerRow
	for rows.Next() {
		var r providerRow
		if err := rows.Scan(
			&r.ID, &r.Name, &r.ProviderType, &r.Endpoint,
			&r.APIKeyCredentialID, &r.HasAPIKey,
			&r.IsDefault, &r.Enabled, &r.Settings, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan provider: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// storeAPIKey encrypts and inserts an API key into credential_registry,
// scoped to the specific provider so each provider gets its own credential
// row. Returns the new credential's UUID.
func (ph *ProviderHandlers) storeAPIKey(ctx context.Context, providerID uuid.UUID, apiKey string, actorID *uuid.UUID) (uuid.UUID, error) {
	desc := fmt.Sprintf("API key for llm_provider %s", providerID)
	scopeTarget := providerID.String()
	if err := ph.store.SetCredential(ctx, "provider", scopeTarget, "llm", "api_key", apiKey, true, desc, actorID); err != nil {
		return uuid.UUID{}, fmt.Errorf("store api key: %w", err)
	}
	var credID uuid.UUID
	err := ph.db.QueryRow(ctx, `
		SELECT id FROM credential_registry
		 WHERE scope = 'provider' AND scope_target = $1
		   AND category = 'llm' AND key_name = 'api_key'
	`, scopeTarget).Scan(&credID)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("lookup api_key credential id: %w", err)
	}
	return credID, nil
}

// resolveAPIKey decrypts the stored API key for a provider, or returns "" if none.
func (ph *ProviderHandlers) resolveAPIKey(ctx context.Context, credID *uuid.UUID) (string, error) {
	if credID == nil {
		return "", nil
	}
	var encryptedValue, nonce []byte
	err := ph.db.QueryRow(ctx, `
		SELECT encrypted_value, nonce FROM credential_registry WHERE id = $1
	`, *credID).Scan(&encryptedValue, &nonce)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("fetch credential: %w", err)
	}
	plaintext, err := crypto.Decrypt(ph.store.MasterKey(), *credID, encryptedValue, nonce)
	if err != nil {
		return "", fmt.Errorf("decrypt credential: %w", err)
	}
	return plaintext, nil
}

// ─── ListProviders ─────────────────────────────────────────────────────────────

// ListProviders handles GET /api/admin/providers.
func (ph *ProviderHandlers) ListProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providers, err := ph.listProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if providers == nil {
		providers = []providerRow{}
	}
	writeJSON(w, http.StatusOK, providers)
}

// ─── CreateProvider ───────────────────────────────────────────────────────────

// CreateProvider handles POST /api/admin/providers/create.
func (ph *ProviderHandlers) CreateProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req createProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.ProviderType == "" {
		writeError(w, http.StatusBadRequest, "name and provider_type are required")
		return
	}

	settingsJSON := json.RawMessage("{}")
	if req.Settings != nil {
		b, _ := json.Marshal(req.Settings)
		settingsJSON = b
	}

	providerID := uuid.New()
	var credIDVal interface{}
	actorID := actorIDFromCtx(r)

	if req.APIKey != "" {
		credID, err := ph.storeAPIKey(r.Context(), providerID, req.APIKey, actorID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		credIDVal = credID
	}

	_, err := ph.db.Exec(r.Context(), `
		INSERT INTO llm_providers
			(id, name, provider_type, endpoint, api_key_credential_id, settings)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, providerID, req.Name, req.ProviderType, req.Endpoint, credIDVal, settingsJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("insert provider: %v", err))
		return
	}

	// Keep credential_registry (what agents read) in sync with the UI-managed
	// llm_providers (what this handler writes). No-op if no default exists yet.
	if err := syncDefaultLLMProviderToCredentials(r.Context(), ph.db, ph.store); err != nil {
		// Non-fatal: the provider row was inserted. Log, continue.
		fmt.Printf("provider-create: sync to credential_registry failed: %v\n", err)
	}

	providers, err := ph.listProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range providers {
		if p.ID == providerID {
			writeJSON(w, http.StatusCreated, p)
			return
		}
	}
	writeError(w, http.StatusInternalServerError, "provider created but not found")
}

// ─── UpdateProvider ───────────────────────────────────────────────────────────

// UpdateProvider handles PUT /api/admin/providers/update?id=.
func (ph *ProviderHandlers) UpdateProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var req updateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("provider-update: invalid body: %v", err)
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	log.Printf("provider-update: id=%s name=%q apiKey_len=%d enabled=%v", providerID, req.Name, len(req.APIKey), req.Enabled)

	settingsJSON := json.RawMessage("{}")
	if req.Settings != nil {
		b, _ := json.Marshal(req.Settings)
		settingsJSON = b
	}

	// M4-06: the default provider's model/endpoint is the global default
	// model of every definition without its own ("set default model").
	// The guarded transaction writes the row's configuration; the API key
	// (not part of a snapshot) is stored afterwards, so a refused change
	// stores no key.
	if !guardConfigChange(w, r, ph.guardDB(), "providers.update", map[string]any{"provider_id": providerID},
		func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE llm_providers SET name = $1, endpoint = $2, settings = $3, enabled = $4, updated_at = NOW() WHERE id = $5`,
				req.Name, req.Endpoint, settingsJSON, req.Enabled, providerID)
			if err == nil && tag.RowsAffected() == 0 {
				return notFound("provider not found")
			}
			return err
		}) {
		return
	}

	// Fetch existing credential ID
	var existingCredID *uuid.UUID
	err = ph.db.QueryRow(r.Context(), `
		SELECT api_key_credential_id FROM llm_providers WHERE id = $1
	`, providerID).Scan(&existingCredID)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	log.Printf("provider-update: existingCredID=%v", existingCredID)

	var newCredIDVal interface{}
	actorID := actorIDFromCtx(r)
	if req.APIKey != "" {
		if existingCredID != nil {
			// Check if the existing credential is at the shared global/default
			// scope. If so, create a new provider-scoped credential instead of
			// rotating the shared one (which gets overwritten by sync).
			var credScope, credTarget string
			_ = ph.db.QueryRow(r.Context(), `
				SELECT scope, scope_target FROM credential_registry WHERE id = $1
			`, *existingCredID).Scan(&credScope, &credTarget)

			if credScope == "provider" {
				if err := ph.store.RotateCredential(r.Context(), *existingCredID, req.APIKey, actorID); err != nil {
					writeError(w, http.StatusInternalServerError, "rotate credential: "+err.Error())
					return
				}
				newCredIDVal = *existingCredID
			} else {
				credID, credErr := ph.storeAPIKey(r.Context(), providerID, req.APIKey, actorID)
				if credErr != nil {
					writeError(w, http.StatusInternalServerError, credErr.Error())
					return
				}
				newCredIDVal = credID
				log.Printf("provider-update: migrated credential from %s/%s to provider/%s (cred=%s)", credScope, credTarget, providerID, credID)
			}
		} else {
			credID, credErr := ph.storeAPIKey(r.Context(), providerID, req.APIKey, actorID)
			if credErr != nil {
				writeError(w, http.StatusInternalServerError, credErr.Error())
				return
			}
			newCredIDVal = credID
		}
	} else {
		// Keep existing credential as-is
		newCredIDVal = existingCredID
	}

	// The guarded write above committed name/endpoint/settings/enabled; the
	// key reference is not part of any snapshot.
	_, err = ph.db.Exec(r.Context(), `
		UPDATE llm_providers SET api_key_credential_id = $1, updated_at = NOW() WHERE id = $2
	`, newCredIDVal, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("update provider: %v", err))
		return
	}

	// Re-sync to credential_registry whenever a provider row changes; cheap
	// enough to run unconditionally, and catches the case where this was the
	// default provider (or was just disabled, forcing a different default).
	if err := syncDefaultLLMProviderToCredentials(r.Context(), ph.db, ph.store); err != nil {
		fmt.Printf("provider-update: sync to credential_registry failed: %v\n", err)
	}

	providers, err := ph.listProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range providers {
		if p.ID == providerID {
			writeJSON(w, http.StatusOK, p)
			return
		}
	}
	writeError(w, http.StatusNotFound, "provider not found after update")
}

// ─── DeleteProvider ───────────────────────────────────────────────────────────

// DeleteProvider handles DELETE /api/admin/providers/delete?id=.
func (ph *ProviderHandlers) DeleteProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	// Fetch the credential ID so we can delete it too.
	var credID *uuid.UUID
	err = ph.db.QueryRow(r.Context(), `
		SELECT api_key_credential_id FROM llm_providers WHERE id = $1
	`, providerID).Scan(&credID)
	if err != nil && err != pgx.ErrNoRows {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if !guardConfigChange(w, r, ph.guardDB(), "providers.delete", map[string]any{"provider_id": providerID},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM llm_providers WHERE id = $1`, providerID)
			return err
		}) {
		return
	}

	if credID != nil {
		// Delete the associated credential row via the configstore helper so
		// the deletion is audit-logged (E.9) and broadcast via pg_notify to
		// every watching agent.
		actorID := actorIDFromCtx(r)
		if err := ph.store.DeleteCredential(r.Context(), *credID, actorID); err != nil {
			// Non-fatal: the provider row is already deleted. Log and continue.
			fmt.Printf("provider-delete: remove credential %s failed: %v\n", *credID, err)
		}
	}

	// If the deleted provider was default, re-sync to the next one (or no-op).
	if err := syncDefaultLLMProviderToCredentials(r.Context(), ph.db, ph.store); err != nil {
		fmt.Printf("provider-delete: sync to credential_registry failed: %v\n", err)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── SetDefault ───────────────────────────────────────────────────────────────

// SetDefault handles POST /api/admin/providers/set-default?id=.
func (ph *ProviderHandlers) SetDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	if !guardConfigChange(w, r, ph.guardDB(), "providers.set_default", map[string]any{"provider_id": providerID},
		func(ctx context.Context, tx pgx.Tx) error {
			// Unset all, then set the chosen one.
			if _, err := tx.Exec(ctx, `UPDATE llm_providers SET is_default = false`); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE llm_providers SET is_default = true, updated_at = NOW() WHERE id = $1`, providerID)
			if err == nil && tag.RowsAffected() == 0 {
				return notFound("provider not found")
			}
			return err
		}) {
		return
	}
	// Default changed → mirror the new default's provider/endpoint/model_name
	// into credential_registry so agents use the new one.
	if err := syncDefaultLLMProviderToCredentials(r.Context(), ph.db, ph.store); err != nil {
		fmt.Printf("provider-set-default: sync to credential_registry failed: %v\n", err)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── TestConnection ───────────────────────────────────────────────────────────

// TestConnection handles POST /api/admin/providers/test?id=.
func (ph *ProviderHandlers) TestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var provType, endpoint string
	var credID *uuid.UUID
	var settingsRaw []byte
	err = ph.db.QueryRow(r.Context(), `
		SELECT provider_type, endpoint, api_key_credential_id, settings
		FROM llm_providers WHERE id = $1
	`, providerID).Scan(&provType, &endpoint, &credID, &settingsRaw)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	apiKey, err := ph.resolveAPIKey(r.Context(), credID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Extract model from settings JSON — openai/anthropic HealthCheck makes a
	// tiny completion call that requires a model parameter. Without this the
	// test fails with "you must provide a model parameter" even though the
	// credentials are valid.
	model := ""
	if len(settingsRaw) > 0 {
		var s map[string]any
		if json.Unmarshal(settingsRaw, &s) == nil {
			if m, ok := s["model"].(string); ok {
				model = m
			}
		}
	}

	a, err := adapter.NewFromConfig(provType, endpoint, apiKey, model)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"reachable":  false,
			"latency_ms": 0,
			"error":      err.Error(),
		})
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	healthErr := a.HealthCheck(ctx)
	latency := time.Since(start).Milliseconds()

	if healthErr != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"reachable":  false,
			"latency_ms": latency,
			"error":      healthErr.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reachable":  true,
		"latency_ms": latency,
		"error":      nil,
	})
}

// ─── ListModels ───────────────────────────────────────────────────────────────

// ListModels handles GET /api/admin/providers/models?id=.
func (ph *ProviderHandlers) ListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var provType, endpoint string
	var credID *uuid.UUID
	err = ph.db.QueryRow(r.Context(), `
		SELECT provider_type, endpoint, api_key_credential_id
		FROM llm_providers WHERE id = $1
	`, providerID).Scan(&provType, &endpoint, &credID)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	switch provType {
	case "ollama":
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

	case "openai":
		apiKey, _ := ph.resolveAPIKey(r.Context(), credID)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint+"/models", nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to build request")
			return
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		client := &http.Client{Timeout: 10 * time.Second}
		oResp, err := client.Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("cannot reach OpenAI: %v", err))
			return
		}
		defer oResp.Body.Close()
		body, err := io.ReadAll(oResp.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read OpenAI response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(oResp.StatusCode)
		_, _ = w.Write(body)

	case "anthropic":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"models": anthropicModels,
		})

	default:
		// vllm, custom — cannot enumerate models
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"models": []string{},
		})
	}
}

// ─── PullModel ────────────────────────────────────────────────────────────────

// PullModel handles POST /api/admin/providers/pull?id= (Ollama only).
// Streams download progress as SSE.
func (ph *ProviderHandlers) PullModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var provType, endpoint string
	err = ph.db.QueryRow(r.Context(), `
		SELECT provider_type, endpoint FROM llm_providers WHERE id = $1
	`, providerID).Scan(&provType, &endpoint)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if provType != "ollama" {
		writeError(w, http.StatusBadRequest, "Only Ollama supports model pulling")
		return
	}

	var req pullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		writeError(w, http.StatusBadRequest, "body must be {\"model\": \"<name>\"}")
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"name":   req.Model,
		"stream": true,
	})

	ollamaReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint+"/api/pull", bytes.NewReader(payload))
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

// DeleteModel handles DELETE /api/admin/providers/models?id=&model= (Ollama only).
func (ph *ProviderHandlers) DeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	providerID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	modelName := r.URL.Query().Get("model")
	if modelName == "" {
		writeError(w, http.StatusBadRequest, "model query parameter is required")
		return
	}

	var provType, endpoint string
	err = ph.db.QueryRow(r.Context(), `
		SELECT provider_type, endpoint FROM llm_providers WHERE id = $1
	`, providerID).Scan(&provType, &endpoint)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if provType != "ollama" {
		writeError(w, http.StatusBadRequest, "Only Ollama supports model deletion via this endpoint")
		return
	}

	payload, _ := json.Marshal(map[string]string{"name": modelName})
	ollamaReq, err := http.NewRequestWithContext(r.Context(), http.MethodDelete, endpoint+"/api/delete", bytes.NewReader(payload))
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
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "model": modelName})
		return
	}

	body, _ := io.ReadAll(resp.Body)
	writeError(w, resp.StatusCode, string(body))
}
