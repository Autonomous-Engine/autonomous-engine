package vector

import (
	"context"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EmbeddingCredentialKeyName returns the per-provider credential key used to
// stash an embedding provider's API key in credential_registry. Each row in
// embedding_providers carries its own UUID, so multiple providers can each
// hold their own API key at global/default scope without colliding.
func EmbeddingCredentialKeyName(providerID uuid.UUID) string {
	return "api_key_" + providerID.String()
}

// BuildEmbeddingService resolves the default embedding provider from the
// database and returns a configured EmbeddingService along with the
// dimension this provider claims to produce.
//
// Returns a descriptive error when:
//   - no row in embedding_providers has is_default = true AND enabled = true;
//   - the provider row references an api_key_credential_id that cannot be
//     resolved (e.g. the underlying credential_registry row was deleted).
//
// It is INTENTIONALLY loud on failure: callers that silently fell back to
// pseudo-embeddings (hash-based vectors, useless for semantic search) broke
// codex_similar_code in production for months. Callers that can tolerate a
// missing provider (agents that fall back to grep) should check the error
// and disable codex_similar_code tool registration rather than patching the
// fallback in here.
func BuildEmbeddingService(
	ctx context.Context,
	db *pgxpool.Pool,
	store *configstore.ConfigStore,
) (*EmbeddingService, int, error) {
	var (
		providerID   uuid.UUID
		providerType string
		endpoint     string
		model        string
		dimension    int
		apiKeyCredID *uuid.UUID
	)
	err := db.QueryRow(ctx, `
		SELECT id, provider_type, endpoint, model, dimension, api_key_credential_id
		FROM embedding_providers
		WHERE is_default = true AND enabled = true
		LIMIT 1
	`).Scan(&providerID, &providerType, &endpoint, &model, &dimension, &apiKeyCredID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, 0, fmt.Errorf(
				"no default embedding provider configured — " +
					"set one in Admin → Providers → Embeddings (or POST /api/admin/embedding-providers)",
			)
		}
		return nil, 0, fmt.Errorf("query default embedding provider: %w", err)
	}

	// Resolve API key (optional). For Ollama it will typically be absent.
	apiKey := ""
	if apiKeyCredID != nil {
		// Use ResolveCredential rather than RevealCredential so the lookup
		// also populates the configstore cache and logs a system-level read
		// in credential_audit_log. The key name is scoped by the provider's
		// UUID so multiple embedding providers do not collide on a single
		// "api_key" row at global/default.
		keyName := EmbeddingCredentialKeyName(providerID)
		resolved, resolveErr := store.ResolveCredential(ctx, "embeddings", keyName, "", "")
		if resolveErr != nil {
			return nil, 0, fmt.Errorf(
				"resolve embedding api_key for provider %s: %w",
				providerID, resolveErr,
			)
		}
		apiKey = resolved
	}

	svc := NewEmbeddingServiceWithType(providerType, endpoint, apiKey, model)
	return svc, dimension, nil
}
