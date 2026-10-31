package agentregistry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	adapter "github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// AgentDefinition is an admin-defined agent configuration.
type AgentDefinition struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`

	// Model
	Provider      string `json:"provider"`
	ModelID       string `json:"model_id"`
	ModelEndpoint string `json:"model_endpoint"`

	// Pipeline steps
	StepProductAnalysis bool `json:"step_product_analysis"`
	StepResearch        bool `json:"step_research"`
	StepPlanning        bool `json:"step_planning"`
	StepImplementation  bool `json:"step_implementation"`
	StepReview          bool `json:"step_review"`
	StepHumanReview     bool `json:"step_human_review"`
	StepQA              bool `json:"step_qa"`

	// Routing
	RoutingTags       []string `json:"routing_tags"`
	RoutingComplexity []string `json:"routing_complexity"`
	RoutingPriority   bool     `json:"routing_priority"`

	// Behavior
	MaxRetries      int `json:"max_retries"`
	MaxFilesPerTask int `json:"max_files_per_task"`
	TimeoutMinutes  int `json:"timeout_minutes"`

	// Status
	Enabled   bool `json:"enabled"`
	IsDefault bool `json:"is_default"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AgentDefinitionPrompt is a prompt template scoped to an agent definition.
type AgentDefinitionPrompt struct {
	ID                uuid.UUID `json:"id"`
	AgentDefinitionID uuid.UUID `json:"agent_definition_id"`
	IntentType        string    `json:"intent_type"`
	PromptType        string    `json:"prompt_type"`
	Content           string    `json:"content"`
}

// Registry provides CRUD, routing, and prompt management for agent definitions.
type Registry struct {
	db *pgxpool.Pool

	snapOnce sync.Once
	snaps    *configsnap.Store
}

// New returns a new Registry backed by pool.
func New(db *pgxpool.Pool) *Registry {
	return &Registry{db: db}
}

// DB returns the underlying pool for low-level queries.
func (r *Registry) DB() *pgxpool.Pool {
	return r.db
}

// ─── CRUD ─────────────────────────────────────────────────────────────────────

// DBTX is a pool or a transaction. The *Tx functions below take one so a
// caller can run a write inside its own transaction — the M4-06 rollout
// guard dry-runs each configuration change in a transaction it rolls back.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Create inserts a new agent definition and sets its generated ID and timestamps.
func (r *Registry) Create(ctx context.Context, def *AgentDefinition) error {
	return CreateTx(ctx, r.db, def)
}

// CreateTx is Create on q.
func CreateTx(ctx context.Context, q DBTX, def *AgentDefinition) error {
	query := `
		INSERT INTO agent_definitions (
			name, display_name, description,
			provider, model_id, model_endpoint,
			step_product_analysis, step_research, step_planning,
			step_implementation, step_review, step_human_review, step_qa,
			routing_tags, routing_complexity, routing_priority,
			max_retries, max_files_per_task, timeout_minutes,
			enabled, is_default
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21
		) RETURNING id, created_at, updated_at`

	return q.QueryRow(ctx, query,
		def.Name, def.DisplayName, def.Description,
		def.Provider, def.ModelID, def.ModelEndpoint,
		def.StepProductAnalysis, def.StepResearch, def.StepPlanning,
		def.StepImplementation, def.StepReview, def.StepHumanReview, def.StepQA,
		textArray(def.RoutingTags), textArray(def.RoutingComplexity), def.RoutingPriority,
		def.MaxRetries, def.MaxFilesPerTask, def.TimeoutMinutes,
		def.Enabled, def.IsDefault,
	).Scan(&def.ID, &def.CreatedAt, &def.UpdatedAt)
}

// Get returns the agent definition with the given id.
func (r *Registry) Get(ctx context.Context, id uuid.UUID) (*AgentDefinition, error) {
	query := `SELECT ` + defColumns + ` FROM agent_definitions WHERE id = $1`
	row := r.db.QueryRow(ctx, query, id)
	return scanDef(row)
}

// List returns all agent definitions ordered by name.
func (r *Registry) List(ctx context.Context) ([]*AgentDefinition, error) {
	query := `SELECT ` + defColumns + ` FROM agent_definitions ORDER BY name`
	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDefs(rows)
}

// Update overwrites the mutable fields of an existing agent definition.
func (r *Registry) Update(ctx context.Context, id uuid.UUID, def *AgentDefinition) error {
	return UpdateTx(ctx, r.db, id, def)
}

// UpdateTx is Update on q.
func UpdateTx(ctx context.Context, q DBTX, id uuid.UUID, def *AgentDefinition) error {
	query := `
		UPDATE agent_definitions SET
			name=$1, display_name=$2, description=$3,
			provider=$4, model_id=$5, model_endpoint=$6,
			step_product_analysis=$7, step_research=$8, step_planning=$9,
			step_implementation=$10, step_review=$11, step_human_review=$12, step_qa=$13,
			routing_tags=$14, routing_complexity=$15, routing_priority=$16,
			max_retries=$17, max_files_per_task=$18, timeout_minutes=$19,
			enabled=$20, is_default=$21,
			updated_at=NOW()
		WHERE id=$22
		RETURNING updated_at`
	return q.QueryRow(ctx, query,
		def.Name, def.DisplayName, def.Description,
		def.Provider, def.ModelID, def.ModelEndpoint,
		def.StepProductAnalysis, def.StepResearch, def.StepPlanning,
		def.StepImplementation, def.StepReview, def.StepHumanReview, def.StepQA,
		textArray(def.RoutingTags), textArray(def.RoutingComplexity), def.RoutingPriority,
		def.MaxRetries, def.MaxFilesPerTask, def.TimeoutMinutes,
		def.Enabled, def.IsDefault,
		id,
	).Scan(&def.UpdatedAt)
}

// Delete removes an agent definition by id.
func (r *Registry) Delete(ctx context.Context, id uuid.UUID) error {
	return DeleteTx(ctx, r.db, id)
}

// DeleteTx is Delete on q.
func DeleteTx(ctx context.Context, q DBTX, id uuid.UUID) error {
	_, err := q.Exec(ctx, `DELETE FROM agent_definitions WHERE id=$1`, id)
	return err
}

// GetDefault returns the enabled default agent definition, if one exists.
func (r *Registry) GetDefault(ctx context.Context) (*AgentDefinition, error) {
	query := `SELECT ` + defColumns + ` FROM agent_definitions WHERE is_default=true AND enabled=true LIMIT 1`
	row := r.db.QueryRow(ctx, query)
	def, err := scanDef(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil
	}
	return def, err
}

// SetDefault marks a single definition as the default, clearing all others.
func (r *Registry) SetDefault(ctx context.Context, id uuid.UUID) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := SetDefaultTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetDefaultTx is SetDefault's two statements on q (run it in a transaction).
func SetDefaultTx(ctx context.Context, q DBTX, id uuid.UUID) error {
	if _, err := q.Exec(ctx, `UPDATE agent_definitions SET is_default=false, updated_at=NOW()`); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE agent_definitions SET is_default=true, updated_at=NOW() WHERE id=$1`, id); err != nil {
		return err
	}
	return nil
}

// ─── Routing ──────────────────────────────────────────────────────────────────

// ResolveForWorkItem selects the best-matching enabled agent definition for a
// work item described by tags and complexity. Resolution rules:
//  1. A definition with routing_tags must have at least one tag in common with the work item.
//  2. A definition with routing_complexity must include the work item complexity.
//  3. Among matches: routing_priority=true wins; ties broken by number of matched tags.
//  4. If no definition matches, the default is returned.
//  5. If no default exists, an error is returned.
func (r *Registry) ResolveForWorkItem(ctx context.Context, tags []string, complexity string) (*AgentDefinition, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentregistry: list definitions: %w", err)
	}

	tagSet := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		tagSet[t] = struct{}{}
	}

	type candidate struct {
		def      *AgentDefinition
		tagScore int
	}
	var candidates []candidate

	for _, def := range all {
		if !def.Enabled {
			continue
		}

		// Tag filter: if the definition specifies tags, at least one must match.
		if len(def.RoutingTags) > 0 {
			matched := 0
			for _, t := range def.RoutingTags {
				if _, ok := tagSet[t]; ok {
					matched++
				}
			}
			if matched == 0 {
				continue
			}
			candidates = append(candidates, candidate{def, matched})
		} else {
			// No tag constraint — still check complexity below.
			candidates = append(candidates, candidate{def, 0})
		}
	}

	// Complexity filter.
	if complexity != "" {
		var filtered []candidate
		for _, c := range candidates {
			if len(c.def.RoutingComplexity) == 0 {
				filtered = append(filtered, c)
				continue
			}
			for _, cx := range c.def.RoutingComplexity {
				if cx == complexity {
					filtered = append(filtered, c)
					break
				}
			}
		}
		candidates = filtered
	}

	if len(candidates) == 0 {
		def, err := r.GetDefault(ctx)
		if err != nil {
			return nil, fmt.Errorf("agentregistry: get default: %w", err)
		}
		if def == nil {
			return nil, fmt.Errorf("agentregistry: no matching agent definition and no default configured")
		}
		return def, nil
	}

	// Sort: priority first, then tag score descending.
	sort.Slice(candidates, func(i, j int) bool {
		ci, cj := candidates[i], candidates[j]
		if ci.def.RoutingPriority != cj.def.RoutingPriority {
			return ci.def.RoutingPriority
		}
		return ci.tagScore > cj.tagScore
	})

	return candidates[0].def, nil
}

// ─── Prompts ──────────────────────────────────────────────────────────────────

// SetPrompt upserts a prompt for the given agent definition, intent, and type.
func (r *Registry) SetPrompt(ctx context.Context, agentDefID uuid.UUID, intentType, promptType, content string) error {
	return SetPromptTx(ctx, r.db, agentDefID, intentType, promptType, content)
}

// SetPromptTx is SetPrompt on q.
func SetPromptTx(ctx context.Context, q DBTX, agentDefID uuid.UUID, intentType, promptType, content string) error {
	query := `
		INSERT INTO agent_definition_prompts (agent_definition_id, intent_type, prompt_type, content)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (agent_definition_id, intent_type, prompt_type)
		DO UPDATE SET content=EXCLUDED.content, updated_at=NOW()`
	_, err := q.Exec(ctx, query, agentDefID, intentType, promptType, content)
	return err
}

// GetPrompts returns all prompts for the given agent definition.
func (r *Registry) GetPrompts(ctx context.Context, agentDefID uuid.UUID) ([]*AgentDefinitionPrompt, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, agent_definition_id, intent_type, prompt_type, content
		 FROM agent_definition_prompts WHERE agent_definition_id=$1 ORDER BY intent_type, prompt_type`,
		agentDefID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AgentDefinitionPrompt
	for rows.Next() {
		p := &AgentDefinitionPrompt{}
		if err := rows.Scan(&p.ID, &p.AgentDefinitionID, &p.IntentType, &p.PromptType, &p.Content); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPrompt returns the content of a single prompt for an agent definition.
func (r *Registry) GetPrompt(ctx context.Context, agentDefID uuid.UUID, intentType, promptType string) (string, error) {
	var content string
	err := r.db.QueryRow(ctx,
		`SELECT content FROM agent_definition_prompts
		 WHERE agent_definition_id=$1 AND intent_type=$2 AND prompt_type=$3`,
		agentDefID, intentType, promptType,
	).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return content, err
}

// DeletePrompt removes all prompts for the given agent definition and intent type.
func (r *Registry) DeletePrompt(ctx context.Context, agentDefID uuid.UUID, intentType string) error {
	return DeletePromptTx(ctx, r.db, agentDefID, intentType)
}

// DeletePromptTx is DeletePrompt on q.
func DeletePromptTx(ctx context.Context, q DBTX, agentDefID uuid.UUID, intentType string) error {
	_, err := q.Exec(ctx,
		`DELETE FROM agent_definition_prompts WHERE agent_definition_id=$1 AND intent_type=$2`,
		agentDefID, intentType,
	)
	return err
}

// ─── Pipeline helpers ─────────────────────────────────────────────────────────

// pipelineOrder is the canonical order of pipeline steps.
var pipelineOrder = []struct {
	name    string
	enabled func(*AgentDefinition) bool
}{
	{"product_analysis", func(d *AgentDefinition) bool { return d.StepProductAnalysis }},
	{"research", func(d *AgentDefinition) bool { return d.StepResearch }},
	{"planning", func(d *AgentDefinition) bool { return d.StepPlanning }},
	{"implementation", func(d *AgentDefinition) bool { return d.StepImplementation }},
	{"review", func(d *AgentDefinition) bool { return d.StepReview }},
	{"human_review", func(d *AgentDefinition) bool { return d.StepHumanReview }},
	{"qa", func(d *AgentDefinition) bool { return d.StepQA }},
}

// GetPipelineSteps returns the ordered list of active pipeline step names for def.
func GetPipelineSteps(def *AgentDefinition) []string {
	var steps []string
	for _, s := range pipelineOrder {
		if s.enabled(def) {
			steps = append(steps, s.name)
		}
	}
	return steps
}

// NextStep returns the next active pipeline step after currentStep.
// Returns "" when there are no more active steps.
func NextStep(def *AgentDefinition, currentStep string) string {
	found := false
	for _, s := range pipelineOrder {
		if s.name == currentStep {
			found = true
			continue
		}
		if found && s.enabled(def) {
			return s.name
		}
	}
	return ""
}

// ─── Seeding ──────────────────────────────────────────────────────────────────

// SeedDefaults seeds two built-in agent definitions when the table is empty.
// The emptiness check and both inserts run in one transaction, so a seed
// that fails part-way leaves nothing behind and the next startup retries it;
// a table that already has rows (seeded, or curated by an admin) is left
// alone, which keeps the seed idempotent across restarts.
func (r *Registry) SeedDefaults(ctx context.Context) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("agentregistry: seed begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := SeedDefaultsTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SeedDefaultsTx is SeedDefaults on q (run it in a transaction).
func SeedDefaultsTx(ctx context.Context, q DBTX) error {
	var count int
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM agent_definitions`).Scan(&count); err != nil {
		return fmt.Errorf("agentregistry: seed check: %w", err)
	}
	if count > 0 {
		return nil
	}
	for _, def := range defaultDefinitions() {
		if err := CreateTx(ctx, q, def); err != nil {
			return fmt.Errorf("agentregistry: seed %s: %w", def.Name, err)
		}
	}
	return nil
}

// defaultDefinitions returns the built-in definitions SeedDefaults inserts.
// full-stack-engineer has no routing constraints: its routing slices are
// empty (never nil — routing_tags and routing_complexity are TEXT[] NOT NULL
// and pgx encodes a nil slice as NULL; CreateTx also guards against that).
func defaultDefinitions() []*AgentDefinition {
	return []*AgentDefinition{
		{
			Name:                "full-stack-engineer",
			DisplayName:         "Full Stack Engineer",
			Description:         "Runs the complete pipeline: product analysis, research, planning, implementation, review, human review, and QA.",
			StepProductAnalysis: true,
			StepResearch:        true,
			StepPlanning:        true,
			StepImplementation:  true,
			StepReview:          true,
			StepHumanReview:     true,
			StepQA:              true,
			RoutingTags:         []string{},
			RoutingComplexity:   []string{},
			MaxRetries:          3,
			MaxFilesPerTask:     10,
			TimeoutMinutes:      120,
			Enabled:             true,
			IsDefault:           true,
		},
		{
			Name:                "quick-text-editor",
			DisplayName:         "Quick Text Editor",
			Description:         "Lightweight agent for trivial text/copy changes — skips research, planning, and QA.",
			StepProductAnalysis: true,
			StepResearch:        false,
			StepPlanning:        false,
			StepImplementation:  true,
			StepReview:          true,
			StepHumanReview:     true,
			StepQA:              false,
			RoutingTags:         []string{"text-change", "copy-change", "trivial"},
			RoutingComplexity:   []string{"trivial", "simple"},
			MaxRetries:          3,
			MaxFilesPerTask:     10,
			TimeoutMinutes:      120,
			Enabled:             true,
			IsDefault:           false,
		},
	}
}

// textArray returns s, or an empty slice when s is nil. pgx encodes a nil
// []string as SQL NULL, which the TEXT[] NOT NULL routing columns reject
// ("null value in column routing_tags"); an empty slice encodes as '{}'.
func textArray(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ─── Per-work-item model resolution ──────────────────────────────────────────

// ResolveAdapterForWorkItem returns the LLM adapter to use for a given work item.
// It checks the work item's agent_definition_id first. If the definition specifies
// a model (provider + model_id), that is used directly. Otherwise it falls back
// to the global credential registry via cs.
//
// The second return value is the AgentDefinition that was resolved (may be nil
// when the work item has no agent_definition_id or the definition has no model).
// The source string in the log line will be "agent_definition" or "global_default".
//
// It is Resolve without the resolution details; the configuration snapshot
// is still captured (M4-05).
func (r *Registry) ResolveAdapterForWorkItem(
	ctx context.Context,
	workItemID uuid.UUID,
	cs *configstore.ConfigStore,
	fallbackAgentType, fallbackInstanceID string,
) (adapter.ModelAdapter, *AgentDefinition, error) {
	res, err := r.Resolve(ctx, workItemID, cs, fallbackAgentType, fallbackInstanceID)
	if res == nil {
		return nil, nil, err
	}
	return res.Adapter, res.Definition, err
}

// Resolution is what Resolve resolved for one agent and work item.
type Resolution struct {
	Adapter    adapter.ModelAdapter
	Definition *AgentDefinition
	// Provider, ModelID and Endpoint are the resolved model; Source is
	// configsnap.SourceAgentDefinition or configsnap.SourceGlobalDefault.
	Provider string
	ModelID  string
	Endpoint string
	Source   string
	// AgentType is the agent type the resolution was made for.
	AgentType string
	// SnapshotID is the configuration snapshot (config_snapshots.id) of
	// this resolution, "" when it could not be captured. Capture is
	// best-effort: a failure is logged and never fails the resolution.
	SnapshotID string
}

// Resolve resolves the work item's model exactly as ResolveAdapterForWorkItem
// always has and captures the configuration snapshot of the resolution
// (internal/configsnap): snapshots are created lazily here and cached by
// content hash. On error the returned Resolution still carries the
// definition (as ResolveAdapterForWorkItem did) but no adapter.
func (r *Registry) Resolve(
	ctx context.Context,
	workItemID uuid.UUID,
	cs *configstore.ConfigStore,
	fallbackAgentType, fallbackInstanceID string,
) (*Resolution, error) {

	// 1. Look up the work item's agent_definition_id.
	var defIDStr *string
	_ = r.db.QueryRow(ctx,
		"SELECT agent_definition_id::text FROM work_items WHERE id = $1",
		workItemID,
	).Scan(&defIDStr)

	var def *AgentDefinition
	if defIDStr != nil && *defIDStr != "" {
		defID, err := uuid.Parse(*defIDStr)
		if err == nil {
			def, _ = r.Get(ctx, defID)
		}
	}
	res := &Resolution{Definition: def, AgentType: fallbackAgentType}

	// 2. If the definition has a model configured, use it.
	if def != nil && def.Provider != "" && def.ModelID != "" {
		endpoint := def.ModelEndpoint

		// Resolve the API key from the credential registry for this provider.
		apiKey := ""
		if cs != nil {
			apiKey, _ = cs.ResolveCredential(ctx, "llm", "api_key", def.Provider, "")
		}

		a, err := adapter.NewFromConfig(def.Provider, endpoint, apiKey, def.ModelID)
		if err != nil {
			return res, fmt.Errorf("create adapter from agent definition %s: %w", def.Name, err)
		}
		log.Printf("agentregistry: using model %s/%s for work_id=%s (source: agent_definition)",
			a.Name(), def.ModelID, workItemID)
		res.Adapter, res.Provider, res.ModelID, res.Endpoint, res.Source = a, def.Provider, def.ModelID, endpoint, configsnap.SourceAgentDefinition
		r.captureSnapshot(ctx, workItemID, res)
		return res, nil
	}

	// 3. Fall back to global credential registry.
	if cs == nil {
		return res, fmt.Errorf("no ConfigStore available and no agent definition model configured")
	}

	provider, err := cs.ResolveCredential(ctx, "llm", "provider", fallbackAgentType, fallbackInstanceID)
	if err != nil || provider == "" {
		return res, fmt.Errorf("no LLM provider configured")
	}
	endpoint, _ := cs.ResolveCredential(ctx, "llm", "endpoint", fallbackAgentType, fallbackInstanceID)
	apiKey, _ := cs.ResolveCredential(ctx, "llm", "api_key", fallbackAgentType, fallbackInstanceID)
	modelName, _ := cs.ResolveCredential(ctx, "llm", "model_name", fallbackAgentType, fallbackInstanceID)

	a, err := adapter.NewFromConfig(provider, endpoint, apiKey, modelName)
	if err != nil {
		return res, fmt.Errorf("create adapter from global config: %w", err)
	}
	log.Printf("agentregistry: using model %s/%s for work_id=%s (source: global_default)",
		a.Name(), modelName, workItemID)
	res.Adapter, res.Provider, res.ModelID, res.Endpoint, res.Source = a, provider, modelName, endpoint, configsnap.SourceGlobalDefault
	r.captureSnapshot(ctx, workItemID, res)
	return res, nil
}

// snapshots returns the registry's snapshot store (created on first use).
func (r *Registry) snapshots() *configsnap.Store {
	r.snapOnce.Do(func() {
		if r.db != nil {
			r.snaps = configsnap.NewStore(r.db)
		}
	})
	return r.snaps
}

// captureSnapshot captures res's configuration into res.SnapshotID and,
// when the work item has none yet (an item created outside the gateway:
// portal, chat, Jira), records it on the work item. Best-effort: telemetry
// must not block the agent.
func (r *Registry) captureSnapshot(ctx context.Context, workItemID uuid.UUID, res *Resolution) {
	store := r.snapshots()
	if store == nil {
		return
	}
	var defID *uuid.UUID
	if res.Definition != nil {
		id := res.Definition.ID
		defID = &id
	}
	id, err := store.CaptureAndStore(ctx, configsnap.Input{
		WorkItemID:        workItemID,
		AgentDefinitionID: defID,
		AgentType:         res.AgentType,
		Model:             configsnap.Model{Provider: res.Provider, ModelID: res.ModelID, Endpoint: res.Endpoint, Source: res.Source},
	})
	if err != nil {
		log.Printf("agentregistry: capture config snapshot for work_id=%s agent_type=%s (continuing without one): %v", workItemID, res.AgentType, err)
		return
	}
	res.SnapshotID = id
	if workItemID != uuid.Nil {
		if _, err := r.db.Exec(ctx, `UPDATE work_items SET config_snapshot_id = $2 WHERE id = $1 AND config_snapshot_id IS NULL`, workItemID, id); err != nil {
			log.Printf("agentregistry: record config snapshot on work_id=%s: %v", workItemID, err)
		}
	}
}

// CaptureIntakeSnapshot captures the pipeline snapshot of a new work item
// routed to def (nil = no definition) before the item exists: the model is
// the definition's when it names one, otherwise "global_default" with no
// concrete model (each agent resolves its model per agent type and
// snapshots it then). The id is "" with the error when capture fails.
func (r *Registry) CaptureIntakeSnapshot(ctx context.Context, def *AgentDefinition) (string, error) {
	store := r.snapshots()
	if store == nil {
		return "", fmt.Errorf("agentregistry: no database")
	}
	in := configsnap.Input{AgentType: configsnap.PipelineAgentType, Model: configsnap.Model{Source: configsnap.SourceGlobalDefault}}
	if def != nil {
		id := def.ID
		in.AgentDefinitionID = &id
		if def.Provider != "" && def.ModelID != "" {
			in.Model = configsnap.Model{Provider: def.Provider, ModelID: def.ModelID, Endpoint: def.ModelEndpoint, Source: configsnap.SourceAgentDefinition}
		}
	}
	return store.CaptureAndStore(ctx, in)
}

// RecordAttemptSnapshot records snapshotID as the work item's current
// configuration (work_items.config_snapshot_id): an implementation attempt
// runs under it, so outcomes roll up by the configuration that wrote the
// code rather than the one routed at intake. No-op for "".
func (r *Registry) RecordAttemptSnapshot(ctx context.Context, workItemID uuid.UUID, snapshotID string) error {
	if snapshotID == "" || workItemID == uuid.Nil {
		return nil
	}
	_, err := r.db.Exec(ctx, `UPDATE work_items SET config_snapshot_id = $2 WHERE id = $1`, workItemID, snapshotID)
	return err
}

// GetOperatingMode returns the OperatingMode recorded in model_profiles for
// the given (provider, model_id) pair. When no row exists it returns
// OperatingModeStandard and nil — callers use the mode to select a prompt
// scaffold, so "I've never profiled this model" should default to the
// middle tier rather than fail the request.
func (r *Registry) GetOperatingMode(ctx context.Context, provider, modelID string) (models.OperatingMode, error) {
	if provider == "" || modelID == "" {
		return models.OperatingModeStandard, nil
	}
	var mode string
	err := r.db.QueryRow(ctx,
		`SELECT operating_mode FROM model_profiles
		 WHERE provider = $1 AND model_id = $2
		 ORDER BY evaluated_at DESC NULLS LAST
		 LIMIT 1`,
		provider, modelID,
	).Scan(&mode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return models.OperatingModeStandard, nil
		}
		return models.OperatingModeStandard, err
	}
	switch mode {
	case "constrained":
		return models.OperatingModeConstrained, nil
	case "advanced":
		return models.OperatingModeAdvanced, nil
	case "standard":
		return models.OperatingModeStandard, nil
	default:
		return models.OperatingModeStandard, nil
	}
}

// ─── scan helpers ─────────────────────────────────────────────────────────────

const defColumns = `id, name, display_name, description,
	provider, model_id, model_endpoint,
	step_product_analysis, step_research, step_planning,
	step_implementation, step_review, step_human_review, step_qa,
	routing_tags, routing_complexity, routing_priority,
	max_retries, max_files_per_task, timeout_minutes,
	enabled, is_default, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDef(row rowScanner) (*AgentDefinition, error) {
	d := &AgentDefinition{}
	err := row.Scan(
		&d.ID, &d.Name, &d.DisplayName, &d.Description,
		&d.Provider, &d.ModelID, &d.ModelEndpoint,
		&d.StepProductAnalysis, &d.StepResearch, &d.StepPlanning,
		&d.StepImplementation, &d.StepReview, &d.StepHumanReview, &d.StepQA,
		&d.RoutingTags, &d.RoutingComplexity, &d.RoutingPriority,
		&d.MaxRetries, &d.MaxFilesPerTask, &d.TimeoutMinutes,
		&d.Enabled, &d.IsDefault, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if d.RoutingTags == nil {
		d.RoutingTags = []string{}
	}
	if d.RoutingComplexity == nil {
		d.RoutingComplexity = []string{}
	}
	return d, nil
}

func collectDefs(rows pgx.Rows) ([]*AgentDefinition, error) {
	var out []*AgentDefinition
	for rows.Next() {
		d, err := scanDef(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
