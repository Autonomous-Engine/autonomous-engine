package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EmbeddingProviderHandlers groups HTTP handlers for the
// /api/admin/embedding-providers/* routes. Mirrors ProviderHandlers — the
// split exists because embeddings are a distinct category of credential
// (credential_category='embeddings') and belong in their own table so a
// default embedding provider can coexist with a default LLM provider.
type EmbeddingProviderHandlers struct {
	db    *pgxpool.Pool
	store *configstore.ConfigStore
}

// guardDB is the pool as the M4-06 configuration guard's database.
func (eh *EmbeddingProviderHandlers) guardDB() guardBeginner {
	if eh.db == nil {
		return nil
	}
	return eh.db
}

// NewEmbeddingProviderHandlers returns an EmbeddingProviderHandlers backed
// by the given pool and config store.
func NewEmbeddingProviderHandlers(db *pgxpool.Pool, store *configstore.ConfigStore) *EmbeddingProviderHandlers {
	return &EmbeddingProviderHandlers{db: db, store: store}
}

// ─── Domain types ─────────────────────────────────────────────────────────────

// embeddingProviderRow is the DB row shape from embedding_providers. The
// api_key_credential_id is unexported in JSON because leaking the registry
// UUID gives no value to the client; a boolean HasAPIKey replaces it.
type embeddingProviderRow struct {
	ID                 uuid.UUID       `json:"id"`
	Name               string          `json:"name"`
	ProviderType       string          `json:"provider_type"`
	Endpoint           string          `json:"endpoint"`
	Model              string          `json:"model"`
	Dimension          int             `json:"dimension"`
	APIKeyCredentialID *uuid.UUID      `json:"-"`
	HasAPIKey          bool            `json:"has_api_key"`
	IsDefault          bool            `json:"is_default"`
	Enabled            bool            `json:"enabled"`
	Settings           json.RawMessage `json:"settings"`
	CreatedAt          time.Time       `json:"created_at"`
}

// createEmbeddingProviderRequest is the body for POST /create.
type createEmbeddingProviderRequest struct {
	Name         string                 `json:"name"`
	ProviderType string                 `json:"provider_type"`
	Endpoint     string                 `json:"endpoint"`
	Model        string                 `json:"model"`
	APIKey       string                 `json:"api_key"`
	Settings     map[string]interface{} `json:"settings"`
}

// updateEmbeddingProviderRequest is the body for PUT /update.
type updateEmbeddingProviderRequest struct {
	Name     string                 `json:"name"`
	Endpoint string                 `json:"endpoint"`
	Model    string                 `json:"model"`
	APIKey   string                 `json:"api_key"`
	Settings map[string]interface{} `json:"settings"`
	Enabled  bool                   `json:"enabled"`
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// listEmbeddingProviders returns every embedding_providers row, annotated
// with whether an api_key credential is attached.
func (eh *EmbeddingProviderHandlers) listEmbeddingProviders(ctx context.Context) ([]embeddingProviderRow, error) {
	rows, err := eh.db.Query(ctx, `
		SELECT id, name, provider_type, endpoint, model, dimension,
		       api_key_credential_id,
		       (api_key_credential_id IS NOT NULL) AS has_api_key,
		       is_default, enabled, settings, created_at
		FROM embedding_providers
		ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("query embedding_providers: %w", err)
	}
	defer rows.Close()

	var result []embeddingProviderRow
	for rows.Next() {
		var r embeddingProviderRow
		if err := rows.Scan(
			&r.ID, &r.Name, &r.ProviderType, &r.Endpoint, &r.Model, &r.Dimension,
			&r.APIKeyCredentialID, &r.HasAPIKey,
			&r.IsDefault, &r.Enabled, &r.Settings, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan embedding_provider: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// storeEmbeddingAPIKey writes the API key into credential_registry at
// global/default under category='embeddings' with key_name scoped to the
// provider UUID. Per-provider key_name prevents collisions when the
// operator configures multiple embedding providers.
//
// Returns the credential_registry row UUID so the embedding_providers row
// can reference it via api_key_credential_id.
func (eh *EmbeddingProviderHandlers) storeEmbeddingAPIKey(ctx context.Context, providerID uuid.UUID, apiKey string, actorID *uuid.UUID) (uuid.UUID, error) {
	keyName := vector.EmbeddingCredentialKeyName(providerID)
	desc := fmt.Sprintf("API key for embedding_provider %s", providerID)
	if err := eh.store.SetGlobalCredential(ctx, "embeddings", keyName, apiKey, true, desc, actorID); err != nil {
		return uuid.UUID{}, fmt.Errorf("store embedding api key: %w", err)
	}
	var credID uuid.UUID
	err := eh.db.QueryRow(ctx, `
		SELECT id FROM credential_registry
		 WHERE scope = 'global' AND scope_target = 'default'
		   AND category = 'embeddings' AND key_name = $1
	`, keyName).Scan(&credID)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("lookup embedding api_key credential id: %w", err)
	}
	return credID, nil
}

// resolveEmbeddingAPIKey decrypts the stored API key for an embedding
// provider, or returns "" if no credential is linked.
func (eh *EmbeddingProviderHandlers) resolveEmbeddingAPIKey(ctx context.Context, credID *uuid.UUID) (string, error) {
	if credID == nil {
		return "", nil
	}
	var encryptedValue, nonce []byte
	err := eh.db.QueryRow(ctx, `
		SELECT encrypted_value, nonce FROM credential_registry WHERE id = $1
	`, *credID).Scan(&encryptedValue, &nonce)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("fetch embedding credential: %w", err)
	}
	plaintext, err := crypto.Decrypt(eh.store.MasterKey(), *credID, encryptedValue, nonce)
	if err != nil {
		return "", fmt.Errorf("decrypt embedding credential: %w", err)
	}
	return plaintext, nil
}

// ─── ListEmbeddingProviders ───────────────────────────────────────────────────

// ListEmbeddingProviders handles GET /api/admin/embedding-providers.
func (eh *EmbeddingProviderHandlers) ListEmbeddingProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providers, err := eh.listEmbeddingProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if providers == nil {
		providers = []embeddingProviderRow{}
	}
	writeJSON(w, http.StatusOK, providers)
}

// ─── CreateEmbeddingProvider ──────────────────────────────────────────────────

// CreateEmbeddingProvider handles POST /api/admin/embedding-providers.
// The dimension column is left at 0 and will be populated by the Test
// endpoint after the admin clicks "Test" in the UI.
func (eh *EmbeddingProviderHandlers) CreateEmbeddingProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req createEmbeddingProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.ProviderType == "" || req.Endpoint == "" || req.Model == "" {
		writeError(w, http.StatusBadRequest, "name, provider_type, endpoint, and model are required")
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
		credID, err := eh.storeEmbeddingAPIKey(r.Context(), providerID, req.APIKey, actorID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		credIDVal = credID
	}

	_, err := eh.db.Exec(r.Context(), `
		INSERT INTO embedding_providers
			(id, name, provider_type, endpoint, model, dimension, api_key_credential_id, settings)
		VALUES ($1, $2, $3, $4, $5, 0, $6, $7)
	`, providerID, req.Name, req.ProviderType, req.Endpoint, req.Model, credIDVal, settingsJSON)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("insert embedding_provider: %v", err))
		return
	}

	providers, err := eh.listEmbeddingProviders(r.Context())
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

// ─── UpdateEmbeddingProvider ──────────────────────────────────────────────────

// UpdateEmbeddingProvider handles PUT /api/admin/embedding-providers/update?id=.
// The dimension column is NOT modified by this handler — it is only written
// by the Test endpoint (or reset to 0 when model/endpoint change invalidate
// the previously-measured value; see below).
func (eh *EmbeddingProviderHandlers) UpdateEmbeddingProvider(w http.ResponseWriter, r *http.Request) {
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

	var req updateEmbeddingProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	settingsJSON := json.RawMessage("{}")
	if req.Settings != nil {
		b, _ := json.Marshal(req.Settings)
		settingsJSON = b
	}

	// Fetch existing state so we can rotate (vs create) the credential and
	// detect a model/endpoint change (which invalidates the stored dimension).
	var existingCredID *uuid.UUID
	var existingModel, existingEndpoint string
	err = eh.db.QueryRow(r.Context(), `
		SELECT api_key_credential_id, model, endpoint
		  FROM embedding_providers WHERE id = $1
	`, providerID).Scan(&existingCredID, &existingModel, &existingEndpoint)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// M4-06: the default embedding provider is part of every configuration
	// snapshot (retrieval, D7). The configuration columns are written under
	// the rollout guard; the API key (not part of a snapshot) afterwards, so
	// a refused change stores no key.
	// A model or endpoint change invalidates the previously-measured
	// dimension — we can't trust that nomic-768 and text-embedding-3-large-3072
	// produce the same vector length. Reset to 0 so the UI's "dimension
	// unknown" state prompts the admin to re-test.
	modelOrEndpointChanged := (req.Model != existingModel) || (req.Endpoint != existingEndpoint)
	if !guardConfigChange(w, r, eh.guardDB(), "embedding_providers.update", map[string]any{"embedding_provider_id": providerID},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE embedding_providers
				   SET name = $1, endpoint = $2, model = $3, settings = $4, enabled = $5,
				       dimension = CASE WHEN $6 THEN 0 ELSE dimension END, updated_at = NOW()
				 WHERE id = $7
			`, req.Name, req.Endpoint, req.Model, settingsJSON, req.Enabled, modelOrEndpointChanged, providerID)
			return err
		}) {
		return
	}

	var newCredIDVal interface{}
	actorID := actorIDFromCtx(r)
	if req.APIKey != "" {
		if existingCredID != nil {
			if err := eh.store.RotateCredential(r.Context(), *existingCredID, req.APIKey, actorID); err != nil {
				writeError(w, http.StatusInternalServerError, "rotate credential: "+err.Error())
				return
			}
			newCredIDVal = *existingCredID
		} else {
			credID, credErr := eh.storeEmbeddingAPIKey(r.Context(), providerID, req.APIKey, actorID)
			if credErr != nil {
				writeError(w, http.StatusInternalServerError, credErr.Error())
				return
			}
			newCredIDVal = credID
		}
	} else {
		newCredIDVal = existingCredID
	}

	_, err = eh.db.Exec(r.Context(), `
		UPDATE embedding_providers SET api_key_credential_id = $1, updated_at = NOW() WHERE id = $2
	`, newCredIDVal, providerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("update embedding_provider: %v", err))
		return
	}

	providers, err := eh.listEmbeddingProviders(r.Context())
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

// ─── DeleteEmbeddingProvider ──────────────────────────────────────────────────

// DeleteEmbeddingProvider handles DELETE /api/admin/embedding-providers/delete?id=.
func (eh *EmbeddingProviderHandlers) DeleteEmbeddingProvider(w http.ResponseWriter, r *http.Request) {
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

	var credID *uuid.UUID
	err = eh.db.QueryRow(r.Context(), `
		SELECT api_key_credential_id FROM embedding_providers WHERE id = $1
	`, providerID).Scan(&credID)
	if err != nil && err != pgx.ErrNoRows {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if !guardConfigChange(w, r, eh.guardDB(), "embedding_providers.delete", map[string]any{"embedding_provider_id": providerID},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM embedding_providers WHERE id = $1`, providerID)
			return err
		}) {
		return
	}

	if credID != nil {
		actorID := actorIDFromCtx(r)
		if err := eh.store.DeleteCredential(r.Context(), *credID, actorID); err != nil {
			// Non-fatal: the provider row is already deleted.
			fmt.Printf("embedding-provider-delete: remove credential %s failed: %v\n", *credID, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ─── SetDefaultEmbeddingProvider ──────────────────────────────────────────────

// SetDefaultEmbeddingProvider handles POST /api/admin/embedding-providers/default?id=.
// Clears any existing default before setting the new one, so the partial
// unique index on is_default never trips.
func (eh *EmbeddingProviderHandlers) SetDefaultEmbeddingProvider(w http.ResponseWriter, r *http.Request) {
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

	if !guardConfigChange(w, r, eh.guardDB(), "embedding_providers.set_default", map[string]any{"embedding_provider_id": providerID},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE embedding_providers SET is_default = false WHERE is_default = true`); err != nil {
				return fmt.Errorf("unset defaults: %w", err)
			}
			tag, err := tx.Exec(ctx, `UPDATE embedding_providers SET is_default = true, updated_at = NOW() WHERE id = $1`, providerID)
			if err == nil && tag.RowsAffected() == 0 {
				return notFound("provider not found")
			}
			return err
		}) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── TestEmbeddingProvider ────────────────────────────────────────────────────

// TestEmbeddingProvider handles POST /api/admin/embedding-providers/test?id=.
// Executes a single-token embedding call to verify reachability + auth,
// measures end-to-end latency, computes the vector L2 norm for display, and
// persists the detected dimension back onto the row so BuildEmbeddingService
// doesn't have to probe on every boot.
func (eh *EmbeddingProviderHandlers) TestEmbeddingProvider(w http.ResponseWriter, r *http.Request) {
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

	var provType, endpoint, model string
	var credID *uuid.UUID
	err = eh.db.QueryRow(r.Context(), `
		SELECT provider_type, endpoint, model, api_key_credential_id
		  FROM embedding_providers WHERE id = $1
	`, providerID).Scan(&provType, &endpoint, &model, &credID)
	if err != nil {
		if err == pgx.ErrNoRows {
			writeError(w, http.StatusNotFound, "provider not found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	apiKey, err := eh.resolveEmbeddingAPIKey(r.Context(), credID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	svc := vector.NewEmbeddingServiceWithType(provType, endpoint, apiKey, model)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	start := time.Now()
	vec, err := svc.Embed(ctx, "test")
	latency := time.Since(start).Milliseconds()

	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"reachable":  false,
			"latency_ms": latency,
			"error":      err.Error(),
		})
		return
	}

	// Compute L2 norm so the UI can show "was this normalized?". Most
	// embeddings are unit-norm (~1.0); anything wildly off suggests the
	// user picked a model that isn't actually an embedding model.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	// sqrt done in-line to avoid an import just for math.
	if norm > 0 {
		// crude sqrt via Newton's method, one iteration is plenty for display.
		x := norm
		for i := 0; i < 16; i++ {
			x = 0.5 * (x + norm/x)
		}
		norm = x
	}

	dim := len(vec)
	if _, updErr := eh.db.Exec(r.Context(), `
		UPDATE embedding_providers SET dimension = $1, updated_at = NOW() WHERE id = $2
	`, dim, providerID); updErr != nil {
		// Non-fatal: the test succeeded, persistence failure just means
		// the next Test click will re-measure. Log and continue.
		fmt.Printf("embedding-provider-test: persist dimension %d for %s failed: %v\n", dim, providerID, updErr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reachable":   true,
		"latency_ms":  latency,
		"dimension":   dim,
		"vector_norm": norm,
		"error":       nil,
	})
}
