package configsnap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
)

// SchemaVersion is the version of the snapshot document layout. Bump it
// when a field is added, removed or reinterpreted; it is part of the hashed
// document, so old and new layouts never share an id.
const SchemaVersion = 1

// ChecksVersion stands in for the constant of internal/checks, which does
// not exist yet: "absent" is recorded (and hashed) so the document says
// explicitly that no such component shaped the run. VerifierVersion is
// internal/verifier.Version (M3-07); it is copied rather than imported
// because internal/verifier depends on this package through
// internal/agentregistry, and a verifier test keeps the two equal. A new
// value gives every new snapshot a new id, as it should.
const (
	ChecksVersion   = "absent"
	VerifierVersion = "7" // R18: per-criterion citations (6); only seven security categories escalate (7) (see verifier.Version)
)

// PipelineAgentType is the agent type of a snapshot taken for the whole
// pipeline rather than one agent (the gateway at intake): its
// prompt_templates are the templates of every agent type.
const PipelineAgentType = "*"

// Model sources.
const (
	SourceAgentDefinition = "agent_definition"
	SourceGlobalDefault   = "global_default"
)

// Model is the model an agent resolved (or, at intake, will resolve).
// Endpoint is redacted (user info and secret query parameters) before it is
// stored.
type Model struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
	Endpoint string `json:"endpoint"`
	// Source is SourceAgentDefinition or SourceGlobalDefault. At intake a
	// definition without a model records SourceGlobalDefault with an empty
	// provider and model: the concrete model is resolved per agent type
	// later and captured in that agent's snapshot.
	Source string `json:"source"`
}

// Input names what to capture.
type Input struct {
	// WorkItemID is the work item the capture is for (uuid.Nil: none). The
	// document no longer reads anything from it: codex_version is fixed at 0
	// (legacyCodexVersion, migration 060).
	WorkItemID uuid.UUID
	// AgentDefinitionID is the work item's definition; nil when it has none.
	AgentDefinitionID *uuid.UUID
	// AgentType selects the prompt templates in effect ("" = only the
	// agent-independent ones, PipelineAgentType = all of them).
	AgentType string
	Model     Model
}

// Document is a decoded snapshot document.
type Document map[string]any

// Querier is the read surface Capture needs (*pgxpool.Pool and pgx.Tx
// satisfy it).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Execer is the write surface the store needs.
type Execer interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Builder assembles a document from already-read parts; Capture fills it
// from the database. It is exported so the document's shape is testable
// without a database.
type Builder struct {
	AgentType              string
	AgentDefinition        map[string]any
	AgentDefinitionPrompts []map[string]any
	PromptTemplates        []map[string]any
	Model                  Model
	LLMProvider            map[string]any
	EmbeddingProvider      map[string]any
}

// legacyCodexVersion is the document's codex_version. It was the work
// item's codex_version, always 0 for parent items before migration 060;
// since 060 that is the item's pinned, per-product codex version, which is
// a property of the work item (work_item_codex_pins), not of the
// configuration, and would split one configuration's snapshot id by codex
// version. The key stays, fixed at 0, so the ids of earlier captures
// without a codex version stay valid.
const legacyCodexVersion = 0

// Build returns the snapshot document. Lists are sorted by their identity
// columns so the order rows come back from the database never matters, and
// every provider/settings value passes through Redact.
func (b Builder) Build() Document {
	prompts := sortedRows(b.AgentDefinitionPrompts, "intent_type", "prompt_type")
	templates := sortedRows(b.PromptTemplates, "intent_type", "agent_type", "provider", "model_id", "operating_mode", "prompt_type")
	m := b.Model
	m.Endpoint = RedactURL(m.Endpoint)
	doc := Document{
		"schema_version": SchemaVersion,
		"agent_type":     b.AgentType,
		// The definition row carries model_endpoint, which may embed
		// credentials (user:pw@host, ?api-key=…): it is redacted like the
		// provider rows so they reach neither the document nor the hash.
		"agent_definition":         redactMap(b.AgentDefinition),
		"agent_definition_prompts": prompts,
		"prompt_templates":         templates,
		"model": map[string]any{
			"provider": m.Provider, "model_id": m.ModelID, "endpoint": m.Endpoint, "source": m.Source,
		},
		"llm_provider":       redactMap(b.LLMProvider),
		"embedding_provider": redactMap(b.EmbeddingProvider),
		"codex_version":      legacyCodexVersion,
		"versions": map[string]any{
			"controller": controller.Version,
			"checks":     ChecksVersion,
			"verifier":   VerifierVersion,
		},
	}
	return doc
}

func redactMap(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return Redact(m)
}

func sortedRows(rows []map[string]any, keys ...string) []any {
	cp := make([]map[string]any, len(rows))
	copy(cp, rows)
	sort.SliceStable(cp, func(i, j int) bool {
		for _, k := range keys {
			a, b := fmt.Sprint(cp[i][k]), fmt.Sprint(cp[j][k])
			if a != b {
				return a < b
			}
		}
		// Identical identity columns cannot happen for the unique keys
		// these tables have; fall back to the canonical encoding so the
		// order is still total.
		ca, _ := Canonicalize(cp[i])
		cb, _ := Canonicalize(cp[j])
		return bytes.Compare(ca, cb) < 0
	})
	out := make([]any, len(cp))
	for i, r := range cp {
		out[i] = r
	}
	return out
}

// Columns excluded from captured rows: timestamps and status flags change
// without changing what a run does (SetDefault touches updated_at and
// is_default on every definition), and *_credential_id / id point at rows,
// not behaviour. Keeping them would split identical configurations into
// different snapshots.
const (
	agentDefinitionSQL = `SELECT to_jsonb(d) - 'created_at' - 'updated_at' - 'is_default' - 'enabled'
		FROM agent_definitions d WHERE d.id = $1`
	agentDefinitionPromptsSQL = `SELECT COALESCE(jsonb_agg(jsonb_build_object(
			'intent_type', intent_type, 'prompt_type', prompt_type, 'content', content)), '[]'::jsonb)
		FROM agent_definition_prompts WHERE agent_definition_id = $1`
	promptTemplatesSQL = `SELECT COALESCE(jsonb_agg(jsonb_build_object(
			'intent_type', intent_type, 'agent_type', agent_type, 'provider', provider,
			'model_id', model_id, 'operating_mode', operating_mode, 'prompt_type', prompt_type,
			'content', content)), '[]'::jsonb)
		FROM prompt_templates
		WHERE ($1 = '*' OR agent_type = '' OR agent_type = $1)
		  AND (provider = '' OR provider = $2)
		  AND (model_id = '' OR model_id = $3)`
	llmProviderSQL = `SELECT to_jsonb(p) - 'id' - 'created_at' - 'updated_at' - 'api_key_credential_id' - 'is_default' - 'enabled'
		FROM llm_providers p
		WHERE p.enabled AND (($1 = '' AND p.is_default) OR p.provider_type = $1)
		ORDER BY p.is_default DESC, p.name, p.id LIMIT 1`
	embeddingProviderSQL = `SELECT to_jsonb(e) - 'id' - 'created_at' - 'updated_at' - 'api_key_credential_id' - 'is_default' - 'enabled'
		FROM embedding_providers e
		WHERE e.enabled AND e.is_default
		ORDER BY e.name, e.id LIMIT 1`
)

// Capture reads everything the snapshot holds and returns the document.
// A missing row (no definition, no default provider) is recorded as null;
// any other read error is returned.
func Capture(ctx context.Context, q Querier, in Input) (Document, error) {
	b := Builder{AgentType: in.AgentType, Model: in.Model}
	if in.AgentDefinitionID != nil && *in.AgentDefinitionID != uuid.Nil {
		def, err := queryObject(ctx, q, agentDefinitionSQL, *in.AgentDefinitionID)
		if err != nil {
			return nil, fmt.Errorf("configsnap: agent definition: %w", err)
		}
		b.AgentDefinition = def
		if def != nil {
			prompts, err := queryArray(ctx, q, agentDefinitionPromptsSQL, *in.AgentDefinitionID)
			if err != nil {
				return nil, fmt.Errorf("configsnap: agent definition prompts: %w", err)
			}
			b.AgentDefinitionPrompts = prompts
		}
	}
	templates, err := queryArray(ctx, q, promptTemplatesSQL, in.AgentType, in.Model.Provider, in.Model.ModelID)
	if err != nil {
		return nil, fmt.Errorf("configsnap: prompt templates: %w", err)
	}
	b.PromptTemplates = templates
	if b.LLMProvider, err = queryObject(ctx, q, llmProviderSQL, in.Model.Provider); err != nil {
		return nil, fmt.Errorf("configsnap: llm provider: %w", err)
	}
	if b.EmbeddingProvider, err = queryObject(ctx, q, embeddingProviderSQL); err != nil {
		return nil, fmt.Errorf("configsnap: embedding provider: %w", err)
	}
	return b.Build(), nil
}

func queryObject(ctx context.Context, q Querier, sql string, args ...any) (map[string]any, error) {
	var raw []byte
	err := q.QueryRow(ctx, sql, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := decodeJSON(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func queryArray(ctx context.Context, q Querier, sql string, args ...any) ([]map[string]any, error) {
	var raw []byte
	if err := q.QueryRow(ctx, sql, args...).Scan(&raw); err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := decodeJSON(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func decodeJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}
