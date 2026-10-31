package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// syncDefaultLLMProviderToCredentials mirrors the currently-default row from
// llm_providers into the credential_registry rows that agents read via
// configstore.ResolveCredential("llm", ...).
//
// The two-table split is historical: the Providers UI manages llm_providers
// (named, enabled, default, settings), while agents have always read from
// credential_registry (simple key→value with audit + encryption). Without a
// sync, a UI change to the default provider's endpoint does NOT reach agents.
//
// Call this helper from every provider-write path (create, update, delete,
// set-default) to keep the two stores consistent. It also fires pg_notify via
// SetCredential so running agents flush their in-memory caches.
//
// Behaviour:
//   - Reads llm_providers.is_default = true
//   - Writes provider_type  → credential_registry (llm/provider)
//   - Writes endpoint       → credential_registry (llm/endpoint)
//   - Writes settings.model → credential_registry (llm/model_name), clearing
//     the old value when the new provider has no model configured
//   - Mirrors api_key_credential_id's plaintext → credential_registry
//     (llm/api_key) at global/default so the resolver chain reaches it
//     regardless of where the original row was scoped.
//   - If NO provider is marked default (first-run, or after deleting the
//     default), credential_registry llm rows are left untouched. Agents will
//     continue to use whatever was last synced until a new default is chosen.
func syncDefaultLLMProviderToCredentials(
	ctx context.Context,
	db *pgxpool.Pool,
	store *configstore.ConfigStore,
) error {
	var (
		providerType  string
		endpoint      string
		settingsRaw   []byte
		apiKeyCredID  *uuid.UUID
	)
	err := db.QueryRow(ctx, `
		SELECT provider_type, endpoint, settings, api_key_credential_id
		FROM llm_providers
		WHERE is_default = true AND enabled = true
		LIMIT 1
	`).Scan(&providerType, &endpoint, &settingsRaw, &apiKeyCredID)
	if err != nil {
		// No default provider → nothing to sync. Not an error.
		return nil
	}

	// Always write provider + endpoint at the "default" scope_target because
	// that's what configstore.ResolveCredential walks to for global scope.
	if err := store.SetGlobalCredential(ctx, "llm", "provider", providerType, false, "synced from llm_providers default", nil); err != nil {
		return fmt.Errorf("sync provider: %w", err)
	}
	if err := store.SetGlobalCredential(ctx, "llm", "endpoint", endpoint, false, "synced from llm_providers default", nil); err != nil {
		return fmt.Errorf("sync endpoint: %w", err)
	}

	// Sync model_name from settings.model. When the new default provider has
	// no model configured (e.g. Ollama without a pre-selected model), we must
	// clear the old value — otherwise agents keep using the previous provider's
	// model name (e.g. "claude-sonnet-4-20250514") against the new provider.
	modelName := ""
	if len(settingsRaw) > 0 {
		var settings map[string]any
		if err := json.Unmarshal(settingsRaw, &settings); err == nil {
			if m, ok := settings["model"].(string); ok {
				modelName = m
			}
		}
	}
	if err := store.SetGlobalCredential(ctx, "llm", "model_name", modelName, false, "synced from llm_providers default", nil); err != nil {
		return fmt.Errorf("sync model_name: %w", err)
	}

	// Mirror the API key plaintext to global/default so agents calling
	// ResolveCredential("llm","api_key",...) find it. Clear it when the new
	// provider has no api_key (e.g. local Ollama) so the old provider's key
	// doesn't linger.
	apiKey := ""
	if apiKeyCredID != nil {
		plaintext, err := store.RevealCredential(ctx, *apiKeyCredID, nil, "")
		if err != nil {
			return fmt.Errorf("reveal api_key for sync: %w", err)
		}
		apiKey = plaintext
	}
	if err := store.SetGlobalCredential(ctx, "llm", "api_key", apiKey, true, "synced from llm_providers default", nil); err != nil {
		return fmt.Errorf("sync api_key: %w", err)
	}
	return nil
}
