package verifier

import (
	"context"
	"errors"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/jackc/pgx/v5"
)

// Shared is the review agent's resolved model, the fallback.
type Shared struct {
	Adapter  adapter.ModelAdapter
	Provider string
	ModelID  string
}

// ResolveModel picks the verifier's model, with fallbackAgentType
// "verifier":
//
//  1. an enabled agent definition named "verifier" with a provider and
//     model (its API key from the credential registry, as for any
//     definition);
//  2. LLM credentials scoped to agent_type "verifier"
//     (credential_registry scope='agent_type', scope_target='verifier',
//     category='llm', key_name='provider'), resolved through the normal
//     scope chain for the remaining keys;
//  3. otherwise the review agent's model.
//
// verifier_model_shared_with_author is then set by comparing the chosen
// (provider, model) with the author's (WithAuthor), not by which branch
// resolved it: a verifier definition naming the author's model is shared.
//
// A configured verifier model that cannot be built is logged and falls back
// to (3) rather than failing the review.
func resolveModel(ctx context.Context, q Querier, cs *configstore.ConfigStore, shared Shared) (adapter.ModelAdapter, ModelInfo) {
	agentType := prompts.VerifierAgentType
	if q != nil {
		var provider, model, endpoint string
		err := q.QueryRow(ctx, `SELECT provider, model_id, COALESCE(model_endpoint, '') FROM agent_definitions
			WHERE name = $1 AND enabled AND COALESCE(provider, '') <> '' AND COALESCE(model_id, '') <> ''
			ORDER BY updated_at DESC LIMIT 1`, agentType).Scan(&provider, &model, &endpoint)
		switch {
		case err == nil:
			apiKey := ""
			if cs != nil {
				apiKey, _ = cs.ResolveCredential(ctx, "llm", "api_key", provider, "")
			}
			if a, aerr := adapter.NewFromConfig(provider, endpoint, apiKey, model); aerr == nil {
				return a, ModelInfo{Provider: provider, ModelID: model, Source: SourceVerifierDefinition}
			} else {
				log.Printf("verifier: agent definition %q model %s/%s unusable: %v — using the review agent's model", agentType, provider, model, aerr)
			}
		case !errors.Is(err, pgx.ErrNoRows):
			log.Printf("verifier: read verifier agent definition: %v — trying credentials", err)
		}

		var one int
		err = q.QueryRow(ctx, `SELECT 1 FROM credential_registry
			WHERE scope = 'agent_type' AND scope_target = $1 AND category = 'llm' AND key_name = 'provider'`, agentType).Scan(&one)
		if err == nil && cs != nil {
			provider, perr := cs.ResolveCredential(ctx, "llm", "provider", agentType, "")
			endpoint, _ := cs.ResolveCredential(ctx, "llm", "endpoint", agentType, "")
			apiKey, _ := cs.ResolveCredential(ctx, "llm", "api_key", agentType, "")
			model, _ := cs.ResolveCredential(ctx, "llm", "model_name", agentType, "")
			if perr == nil && provider != "" {
				if a, aerr := adapter.NewFromConfig(provider, endpoint, apiKey, model); aerr == nil {
					return a, ModelInfo{Provider: provider, ModelID: model, Source: SourceVerifierCredential}
				} else {
					log.Printf("verifier: verifier-scoped credentials unusable: %v — using the review agent's model", aerr)
				}
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("verifier: read verifier credentials: %v", err)
		}
	}
	return shared.Adapter, ModelInfo{Provider: shared.Provider, ModelID: shared.ModelID, Source: SourceSharedWithReviewer}
}
