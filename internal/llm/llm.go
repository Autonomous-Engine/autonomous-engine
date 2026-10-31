package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/profile"
	"github.com/autonomousengine/autonomous-engine/internal/llm/router"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/sharedstate"
)

// Service is the top-level LLM service that agents use to interact with language models.
// It combines a router (primary + fallback adapters), a strategy, and a model profile.
type Service struct {
	router   *router.Router
	strategy strategy.Strategy
	profile  *models.ModelProfile
	db       *pgxpool.Pool
	cfg      *config.ModelConfig
	// tags are written on every inference_logs row this service writes
	// (M4-05 cost tagging); tags on the call's context take precedence.
	tags InferenceTags
}

// sharedLimitKey names a router's rate-limit bucket in Redis; the key is
// ignored when REDIS_URL is unset. A registry service's bucket is shared by
// the replicas of that agent type for that provider and model. A work-item
// bucket is not the same identity as the in-process one: without Redis,
// NewServiceForWorkItem builds a fresh bucket per Kafka message, while with
// Redis every message of one work item, on any replica, draws on one bucket
// keyed by the work item.
func sharedLimitKey(parts ...string) string {
	return strings.Join(parts, ":")
}

// Tags returns the service's own inference tags.
func (svc *Service) Tags() InferenceTags { return svc.tags }

// NewService constructs a Service from the model configuration and a database pool.
// It creates adapters for the primary and fallback providers, wires up the router,
// and defaults to the Standard strategy.
func NewService(cfg *config.ModelConfig, db *pgxpool.Pool) (*Service, error) {
	if cfg.Provider == "" {
		return nil, fmt.Errorf("llm: model provider is not configured")
	}

	primaryAdapter, err := adapter.NewFromConfig(cfg.Provider, cfg.Endpoint, cfg.APIKey, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("llm: create primary adapter: %w", err)
	}

	routerCfg := router.RouterConfig{
		Primary:             primaryAdapter,
		MaxConcurrent:       10,
		RateLimit:           60,
		HealthCheckInterval: 30 * time.Second,
		// With REDIS_URL set, replicas share this bucket (see sharedLimitKey).
		SharedStore:  sharedstate.FromEnv(),
		RateLimitKey: sharedLimitKey("model-config", cfg.Provider, cfg.Name),
	}

	// Optionally configure fallback adapter
	if cfg.FallbackProvider != "" && cfg.Name != "" {
		fallbackAdapter, err := adapter.NewFromConfig(
			cfg.FallbackProvider,
			cfg.FallbackEndpoint,
			cfg.FallbackAPIKey,
			cfg.Name,
		)
		if err == nil {
			routerCfg.Fallback = fallbackAdapter
		}
		// Non-fatal: fallback misconfiguration just means no fallback
	}

	r := router.New(routerCfg)

	svc := &Service{
		router:   r,
		strategy: &strategy.StandardStrategy{},
		db:       db,
		cfg:      cfg,
	}

	return svc, nil
}

// NewServiceFromRegistry creates an LLM Service by resolving credentials from the
// ConfigStore using the scoped override chain for the given agent type and instance.
func NewServiceFromRegistry(cs *configstore.ConfigStore, agentType string, instanceID string, db *pgxpool.Pool) (*Service, error) {
	ctx := context.Background()

	provider, err := cs.ResolveCredential(ctx, "llm", "provider", agentType, instanceID)
	if err != nil || provider == "" {
		return nil, fmt.Errorf("no LLM provider configured for agent type %s: %w", agentType, err)
	}

	endpoint, _ := cs.ResolveCredential(ctx, "llm", "endpoint", agentType, instanceID)
	apiKey, _ := cs.ResolveCredential(ctx, "llm", "api_key", agentType, instanceID)
	modelName, _ := cs.ResolveCredential(ctx, "llm", "model_name", agentType, instanceID)

	primary, err := adapter.NewFromConfig(provider, endpoint, apiKey, modelName)
	if err != nil {
		return nil, fmt.Errorf("create primary adapter for %s: %w", agentType, err)
	}

	var fallback adapter.ModelAdapter
	fbProvider, _ := cs.ResolveCredential(ctx, "llm", "fallback_provider", agentType, instanceID)
	if fbProvider != "" {
		fbEndpoint, _ := cs.ResolveCredential(ctx, "llm", "fallback_endpoint", agentType, instanceID)
		fbKey, _ := cs.ResolveCredential(ctx, "llm", "fallback_api_key", agentType, instanceID)
		fallback, _ = adapter.NewFromConfig(fbProvider, fbEndpoint, fbKey, modelName)
	}

	r := router.New(router.RouterConfig{
		Primary:       primary,
		Fallback:      fallback,
		MaxConcurrent: 10,
		RateLimit:     120,
		SharedStore:   sharedstate.FromEnv(),
		RateLimitKey:  sharedLimitKey(agentType, provider, modelName),
	})

	return &Service{
		router:   r,
		strategy: strategy.Select(models.OperatingModeStandard),
		db:       db,
		tags: InferenceTags{
			Role:      RoleForAgentType(agentType),
			AgentType: agentType,
			Provider:  provider,
			ModelID:   modelName,
		},
	}, nil
}

// NewServiceForWorkItem creates an LLM Service using the model specified by the work
// item's agent definition, falling back to global credentials when no definition
// model is configured. It is equivalent to calling
// registry.ResolveAdapterForWorkItem and then wrapping the adapter in a Service.
func NewServiceForWorkItem(
	ctx context.Context,
	registry *agentregistry.Registry,
	workItemID uuid.UUID,
	cs *configstore.ConfigStore,
	fallbackAgentType string,
	db *pgxpool.Pool,
) (*Service, error) {
	res, err := registry.Resolve(ctx, workItemID, cs, fallbackAgentType, "instance-1")
	if err != nil {
		return nil, err
	}

	r := router.New(router.RouterConfig{
		Primary:       res.Adapter,
		MaxConcurrent: 10,
		RateLimit:     120,
		SharedStore:   sharedstate.FromEnv(),
		RateLimitKey:  sharedLimitKey(fallbackAgentType, "work-item", workItemID.String()),
	})

	// Every call this service makes is tagged with the work item, the
	// agent's role and the resolution's configuration snapshot, even when
	// the intent does not name the work item (M4-05).
	return &Service{
		router:   r,
		strategy: strategy.Select(models.OperatingModeStandard),
		db:       db,
		tags:     TagsForResolution(res, workItemID),
	}, nil
}

// Execute runs an intent through the current strategy and logs the inference
// to the DB. The log row is INSERTED at the start of the call (latency_ms=0,
// success=false as placeholders) and UPDATED when the call completes. This
// gives the dashboard live visibility into in-flight LLM calls — without it,
// a work item can sit in "prd" state for five minutes with no indication
// anything is happening. Violates Fail Honestly otherwise.
func (svc *Service) Execute(ctx context.Context, intent strategy.Intent) (*strategy.Result, error) {
	start := time.Now()
	primaryAdapter := svc.router.GetPrimary()

	logID := svc.beginInference(context.Background(), intent, InferenceTagsFrom(ctx).merge(svc.tags))

	// Retry transient LLM failures (connection resets, 429/502/503, HTTP
	// timeouts during slow generations). Non-transient errors bubble up
	// immediately. Respects ctx so callers with their own deadlines are
	// honoured. The OnRetry callback logs to stderr for operator
	// visibility; a future enhancement could publish heartbeat events to
	// Kafka so the dashboard shows "retrying 2/3" instead of a silent
	// pause.
	var result *strategy.Result
	err := withRetry(ctx, DefaultRetryPolicy(), func() error {
		r, e := svc.strategy.Execute(ctx, primaryAdapter, intent)
		if e != nil {
			return e
		}
		result = r
		return nil
	})
	latencyMs := time.Since(start).Milliseconds()

	// Complete the log row asynchronously; don't fail the caller on log errors.
	go svc.completeInference(context.Background(), logID, result, err, latencyMs)

	if err != nil {
		return nil, fmt.Errorf("llm execute: %w", err)
	}
	return result, nil
}

// RunEvaluation runs the model profiler, updates the strategy based on the result,
// and saves the profile to the database.
func (svc *Service) RunEvaluation(ctx context.Context) (*models.ModelProfile, error) {
	primaryAdapter := svc.router.GetPrimary()
	p := profile.NewProfiler(primaryAdapter)

	prof, err := p.Evaluate(ctx)
	if err != nil {
		return nil, fmt.Errorf("llm evaluation: %w", err)
	}

	// Update strategy based on discovered operating mode
	svc.strategy = strategy.Select(prof.OperatingMode)
	svc.profile = prof

	// Persist profile to DB
	if svc.db != nil {
		if saveErr := svc.saveProfile(ctx, prof); saveErr != nil {
			// Non-fatal: log but don't fail
			_ = saveErr
		}
	}

	return prof, nil
}

// GetProfile returns the most recently evaluated model profile, or nil if no
// evaluation has been run yet.
func (svc *Service) GetProfile() *models.ModelProfile {
	return svc.profile
}

// HealthCheck delegates to the primary adapter's health check.
func (svc *Service) HealthCheck(ctx context.Context) error {
	return svc.router.GetPrimary().HealthCheck(ctx)
}

// Close shuts down the router and associated health monitors.
func (svc *Service) Close() {
	svc.router.Close()
}

// ---- persistence helpers ----

const insertInferenceLogSQL = `
INSERT INTO inference_logs (
    id, model_provider, model_id, intent_type,
    input_tokens, output_tokens, latency_ms, success,
    error_message, work_item_id, created_at,
    role, agent_type, config_snapshot_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
`

// inferenceRowIdentity returns the model_provider, model_id and
// work_item_id of a Service call's inference_logs row. The tags' resolved
// model wins (bare provider and model, so the price join matches); then
// the static config; then the adapter name split at its slash (before
// M4-05 the whole adapter name was written to both columns). The work item
// is the intent's, else the tags'.
func inferenceRowIdentity(cfg *config.ModelConfig, adapterName string, intent strategy.Intent, tags InferenceTags) (provider, modelID string, workItemID *uuid.UUID) {
	switch {
	case tags.Provider != "" && tags.ModelID != "":
		provider, modelID = tags.Provider, tags.ModelID
	case cfg != nil:
		provider, modelID = cfg.Provider, cfg.Name
	default:
		provider, modelID = SplitAdapterName(adapterName)
		if modelID == "" {
			modelID = provider
		}
	}
	if widStr, ok := intent.Context["work_item_id"]; ok && widStr != "" {
		if wid, err := uuid.Parse(widStr); err == nil {
			workItemID = &wid
		}
	}
	if workItemID == nil && tags.WorkItemID != nil && *tags.WorkItemID != uuid.Nil {
		wid := *tags.WorkItemID
		workItemID = &wid
	}
	return provider, modelID, workItemID
}

// beginInference inserts an in-progress row (latency_ms=0, success=false,
// empty error) and returns its ID. The dashboard uses latency_ms=0 as the
// "still running" marker, rendering a spinner + elapsed-time clock against
// `created_at`. On error it returns uuid.Nil and the call still proceeds —
// logging is best-effort.
func (svc *Service) beginInference(ctx context.Context, intent strategy.Intent, tags InferenceTags) uuid.UUID {
	if svc.db == nil {
		return uuid.Nil
	}

	provider, modelID, workItemID := inferenceRowIdentity(svc.cfg, svc.router.GetPrimary().Name(), intent, tags)

	id := uuid.New()
	_, _ = svc.db.Exec(ctx, insertInferenceLogSQL,
		id, provider, modelID, string(intent.Type),
		0, 0, int64(0), false, "",
		workItemID, time.Now(),
		nullIfEmpty(tags.Role), nullIfEmpty(tags.AgentType), nullIfEmpty(tags.ConfigSnapshotID),
	)
	// Notify listeners that a new in-progress row exists. Agents and the
	// dashboard listen on `inference_started`; the dashboard WS forwards to
	// work-item-scoped channels.
	_, _ = svc.db.Exec(ctx, "SELECT pg_notify('inference_started', $1)", id.String())
	return id
}

// completeInference updates the in-progress row written by beginInference with
// the final latency, tokens, success/error, AND the full conversation
// (prompt_messages + response_content) for operator transparency.
func (svc *Service) completeInference(ctx context.Context, logID uuid.UUID, result *strategy.Result, execErr error, latencyMs int64) {
	if svc.db == nil || logID == uuid.Nil {
		return
	}

	success := execErr == nil
	errMsg := ""
	if execErr != nil {
		errMsg = execErr.Error()
	}
	inTok, outTok := 0, 0
	response := ""
	messagesJSON := []byte("[]")
	if result != nil {
		inTok = result.InputTokens
		outTok = result.OutputTokens
		response = result.Content
		if len(result.Messages) > 0 {
			if b, err := json.Marshal(result.Messages); err == nil {
				messagesJSON = b
			}
		}
	}

	_, _ = svc.db.Exec(ctx, `
		UPDATE inference_logs
		SET input_tokens = $1, output_tokens = $2, latency_ms = $3,
		    success = $4, error_message = $5,
		    prompt_messages = $6::jsonb, response_content = $7
		WHERE id = $8
	`, inTok, outTok, latencyMs, success, errMsg, string(messagesJSON), response, logID)
	_, _ = svc.db.Exec(ctx, "SELECT pg_notify('inference_completed', $1)", logID.String())
}

const upsertProfileSQL = `
INSERT INTO model_profiles (
    id, provider, model_id, effective_context_window,
    instruction_following, code_generation, code_understanding,
    reasoning, synthesis, avg_inference_speed,
    concurrent_capacity, operating_mode, evaluated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (model_id) DO UPDATE SET
    instruction_following  = EXCLUDED.instruction_following,
    code_generation        = EXCLUDED.code_generation,
    code_understanding     = EXCLUDED.code_understanding,
    reasoning              = EXCLUDED.reasoning,
    synthesis              = EXCLUDED.synthesis,
    avg_inference_speed    = EXCLUDED.avg_inference_speed,
    concurrent_capacity    = EXCLUDED.concurrent_capacity,
    operating_mode         = EXCLUDED.operating_mode,
    evaluated_at           = EXCLUDED.evaluated_at
`

func (svc *Service) saveProfile(ctx context.Context, p *models.ModelProfile) error {
	_, err := svc.db.Exec(ctx, upsertProfileSQL,
		p.ID,
		p.Provider,
		p.ModelID,
		p.EffectiveContextWindow,
		p.InstructionFollowing,
		p.CodeGeneration,
		p.CodeUnderstanding,
		p.Reasoning,
		p.Synthesis,
		p.AvgInferenceSpeed,
		p.ConcurrentCapacity,
		string(p.OperatingMode),
		p.EvaluatedAt,
	)
	if err != nil {
		return fmt.Errorf("save model profile: %w", err)
	}
	return nil
}
