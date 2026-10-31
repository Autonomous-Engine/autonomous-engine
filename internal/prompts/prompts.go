package prompts

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PromptTemplate is a single prompt stored in the database, optionally scoped
// to a specific agent type, provider, model, and/or operating mode.
type PromptTemplate struct {
	ID            uuid.UUID  `json:"id"`
	IntentType    string     `json:"intent_type"`
	AgentType     string     `json:"agent_type"`
	Provider      string     `json:"provider"`
	ModelID       string     `json:"model_id"`
	OperatingMode string     `json:"operating_mode"`
	PromptType    string     `json:"prompt_type"`
	Content       string     `json:"content"`
	Description   string     `json:"description"`
	IsDefault     bool       `json:"is_default"`
	CreatedBy     *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// PromptStore provides CRUD and resolution for prompt templates.
type PromptStore struct {
	db *pgxpool.Pool
}

// NewStore returns a PromptStore backed by the given pool.
func NewStore(db *pgxpool.Pool) *PromptStore {
	return &PromptStore{db: db}
}

// ─── Resolution ───────────────────────────────────────────────────────────────

// Resolve returns the content of the most specific matching prompt template.
// It walks the resolution chain from most-specific to least-specific and
// returns the first non-empty match. If no template is found, an error is
// returned so callers can fall back to hardcoded defaults.
func (ps *PromptStore) Resolve(
	ctx context.Context,
	intentType, agentType, provider, modelID, operatingMode, promptType string,
) (string, error) {
	// Candidates in descending specificity.
	queries := []struct {
		intent, agent, prov, model, mode string
	}{
		{intentType, agentType, provider, modelID, operatingMode}, // exact
		{intentType, agentType, provider, modelID, ""},            // any mode
		{intentType, agentType, provider, "", ""},                 // any model in provider
		{intentType, agentType, "", "", operatingMode},            // any provider, specific mode
		{intentType, agentType, "", "", ""},                       // agent-specific default
		{intentType, "", "", "", operatingMode},                   // intent + mode
		{intentType, "", "", "", ""},                              // intent default
	}

	for _, q := range queries {
		var content string
		err := ps.db.QueryRow(ctx,
			`SELECT content FROM prompt_templates
			 WHERE intent_type = $1 AND agent_type = $2 AND provider = $3
			 AND model_id = $4 AND operating_mode = $5 AND prompt_type = $6`,
			q.intent, q.agent, q.prov, q.model, q.mode, promptType,
		).Scan(&content)
		if err == nil && content != "" {
			return content, nil
		}
	}

	return "", fmt.Errorf(
		"no prompt template found for intent=%s agent=%s provider=%s model=%s mode=%s",
		intentType, agentType, provider, modelID, operatingMode,
	)
}

// GetResolutionChain returns all templates that would participate in
// resolution, in order from most-specific to least-specific. The admin UI
// uses this to show which template wins and why.
func (ps *PromptStore) GetResolutionChain(
	ctx context.Context,
	intentType, agentType, provider, modelID, operatingMode, promptType string,
) ([]*PromptTemplate, error) {
	queries := []struct {
		intent, agent, prov, model, mode string
	}{
		{intentType, agentType, provider, modelID, operatingMode},
		{intentType, agentType, provider, modelID, ""},
		{intentType, agentType, provider, "", ""},
		{intentType, agentType, "", "", operatingMode},
		{intentType, agentType, "", "", ""},
		{intentType, "", "", "", operatingMode},
		{intentType, "", "", "", ""},
	}

	var chain []*PromptTemplate
	for _, q := range queries {
		rows, err := ps.db.Query(ctx,
			`SELECT id, intent_type, agent_type, provider, model_id, operating_mode,
			        prompt_type, content, description, is_default, created_by,
			        created_at, updated_at
			 FROM prompt_templates
			 WHERE intent_type = $1 AND agent_type = $2 AND provider = $3
			 AND model_id = $4 AND operating_mode = $5 AND prompt_type = $6`,
			q.intent, q.agent, q.prov, q.model, q.mode, promptType,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			t := &PromptTemplate{}
			if scanErr := scanTemplate(rows, t); scanErr == nil {
				chain = append(chain, t)
			}
		}
		rows.Close()
	}
	return chain, nil
}

// ─── CRUD ─────────────────────────────────────────────────────────────────────

// List returns all prompt templates, with optional filters on intent_type and
// agent_type. Pass empty strings to skip filtering.
func (ps *PromptStore) List(ctx context.Context, intentType, agentType string) ([]*PromptTemplate, error) {
	query := `SELECT id, intent_type, agent_type, provider, model_id, operating_mode,
	                 prompt_type, content, description, is_default, created_by,
	                 created_at, updated_at
	          FROM prompt_templates`
	args := []interface{}{}
	conditions := []string{}

	if intentType != "" {
		args = append(args, intentType)
		conditions = append(conditions, fmt.Sprintf("intent_type = $%d", len(args)))
	}
	if agentType != "" {
		args = append(args, agentType)
		conditions = append(conditions, fmt.Sprintf("agent_type = $%d", len(args)))
	}
	if len(conditions) > 0 {
		query += " WHERE " + joinConditions(conditions)
	}
	query += " ORDER BY intent_type, agent_type, provider, model_id, operating_mode"

	rows, err := ps.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list prompt templates: %w", err)
	}
	defer rows.Close()

	var out []*PromptTemplate
	for rows.Next() {
		t := &PromptTemplate{}
		if err := scanTemplate(rows, t); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		out = append(out, t)
	}
	return out, nil
}

// Get returns a single prompt template by ID.
func (ps *PromptStore) Get(ctx context.Context, id uuid.UUID) (*PromptTemplate, error) {
	row := ps.db.QueryRow(ctx,
		`SELECT id, intent_type, agent_type, provider, model_id, operating_mode,
		        prompt_type, content, description, is_default, created_by,
		        created_at, updated_at
		 FROM prompt_templates WHERE id = $1`, id)

	t := &PromptTemplate{}
	if err := scanTemplate(row, t); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("prompt template %s not found", id)
		}
		return nil, fmt.Errorf("get prompt template: %w", err)
	}
	return t, nil
}

// Create inserts a new prompt template. On a unique-constraint conflict the
// existing row is left unchanged (idempotent for seed operations).
func (ps *PromptStore) Create(ctx context.Context, t *PromptTemplate) error {
	return CreateTx(ctx, ps.db, t)
}

// DBTX is a pool or a transaction: the *Tx functions let a caller write a
// template inside its own transaction (the M4-06 configuration guard).
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CreateTx is Create on q.
func CreateTx(ctx context.Context, q DBTX, t *PromptTemplate) error {
	err := q.QueryRow(ctx,
		`INSERT INTO prompt_templates
		 (intent_type, agent_type, provider, model_id, operating_mode,
		  prompt_type, content, description, is_default, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (intent_type, agent_type, provider, model_id, operating_mode, prompt_type)
		 DO NOTHING
		 RETURNING id, created_at, updated_at`,
		t.IntentType, t.AgentType, t.Provider, t.ModelID, t.OperatingMode,
		t.PromptType, t.Content, t.Description, t.IsDefault, t.CreatedBy,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		// ErrNoRows means the ON CONFLICT DO NOTHING branch fired — row already
		// existed, which is fine for seeding.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("create prompt template: %w", err)
	}
	return nil
}

// Update replaces the content and description of an existing template.
func (ps *PromptStore) Update(ctx context.Context, id uuid.UUID, content, description string) error {
	return UpdateTx(ctx, ps.db, id, content, description)
}

// ErrNotFound is returned by UpdateTx and DeleteTx for an unknown id.
var ErrNotFound = errors.New("prompt template not found")

// UpdateTx is Update on q.
func UpdateTx(ctx context.Context, q DBTX, id uuid.UUID, content, description string) error {
	tag, err := q.Exec(ctx,
		`UPDATE prompt_templates
		 SET content = $1, description = $2, updated_at = NOW()
		 WHERE id = $3`,
		content, description, id,
	)
	if err != nil {
		return fmt.Errorf("update prompt template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// Delete removes a prompt template by ID.
func (ps *PromptStore) Delete(ctx context.Context, id uuid.UUID) error {
	return DeleteTx(ctx, ps.db, id)
}

// DeleteTx is Delete on q.
func DeleteTx(ctx context.Context, q DBTX, id uuid.UUID) error {
	tag, err := q.Exec(ctx,
		`DELETE FROM prompt_templates WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete prompt template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// ListIntentTypes returns all distinct intent_type values present in the table.
func (ps *PromptStore) ListIntentTypes(ctx context.Context) ([]string, error) {
	rows, err := ps.db.Query(ctx,
		`SELECT DISTINCT intent_type FROM prompt_templates ORDER BY intent_type`)
	if err != nil {
		return nil, fmt.Errorf("list intent types: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ListForIntent returns all templates for a given intent type.
func (ps *PromptStore) ListForIntent(ctx context.Context, intentType string) ([]*PromptTemplate, error) {
	rows, err := ps.db.Query(ctx,
		`SELECT id, intent_type, agent_type, provider, model_id, operating_mode,
		        prompt_type, content, description, is_default, created_by,
		        created_at, updated_at
		 FROM prompt_templates
		 WHERE intent_type = $1
		 ORDER BY agent_type, provider, model_id, operating_mode`, intentType)
	if err != nil {
		return nil, fmt.Errorf("list for intent: %w", err)
	}
	defer rows.Close()

	var out []*PromptTemplate
	for rows.Next() {
		t := &PromptTemplate{}
		if err := scanTemplate(rows, t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// ─── Seeding ─────────────────────────────────────────────────────────────────

// SeedDefaults inserts the hardcoded system prompts as is_default=true entries.
// It is idempotent: if any prompt template rows other than the late seeds
// (EnsureLateSeeds) already exist, the original set is not re-inserted. The
// late seeds are ensured on every call, so a template added after a
// database was first seeded (verify_implementation, M3-07) still lands.
func (ps *PromptStore) SeedDefaults(ctx context.Context) error {
	var count int
	if err := ps.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM prompt_templates WHERE NOT (intent_type = ANY($1))`, lateSeedIntents()).Scan(&count); err != nil {
		return fmt.Errorf("seed check: %w", err)
	}
	if count > 0 {
		return ps.EnsureLateSeeds(ctx) // original set already seeded
	}
	if err := ps.EnsureLateSeeds(ctx); err != nil {
		log.Printf("prompts: ensure late seeds: %v", err)
	}

	log.Println("prompts: seeding default prompt templates")

	type seed struct {
		intentType string
		agentType  string
		content    string
		desc       string
	}

	// Intent system prompts — global defaults (agent_type = "").
	intentSeeds := []seed{
		{
			intentType: "analyze_pain_point",
			content:    `You are a senior product manager skilled at identifying user pain points and their business impact. Analyze problems with clarity, quantify impact where possible, and distinguish root causes from symptoms. Be concise and structured.`,
			desc:       "Default system prompt for analyze_pain_point intent",
		},
		{
			intentType: "generate_prd",
			content:    `You are an experienced product manager writing Product Requirements Documents. Structure your output with clear sections: Overview, Problem Statement, Goals, Non-Goals, User Stories, Acceptance Criteria, and Open Questions. Be precise about requirements without over-specifying implementation.`,
			desc:       "Default system prompt for generate_prd intent",
		},
		{
			intentType: "evaluate_findings",
			content:    `You are a senior technical analyst evaluating research findings. Assess evidence quality, identify patterns, flag contradictions, and rank findings by impact and confidence. Distinguish facts from assumptions.`,
			desc:       "Default system prompt for evaluate_findings intent",
		},
		{
			intentType: "synthesize_brief",
			content:    `You are a technical writer who synthesizes complex information into actionable briefs. Produce structured summaries with clear recommendations, prioritized by impact. Avoid jargon without explanation.`,
			desc:       "Default system prompt for synthesize_brief intent",
		},
		{
			intentType: "decompose_tasks",
			content:    `You are a technical project lead who breaks down complex requirements into concrete, independently executable engineering tasks. Each task should have a clear scope, dependencies noted, and estimated complexity. Format as a structured list.`,
			desc:       "Default system prompt for decompose_tasks intent",
		},
		{
			intentType: "verify_assumptions",
			content:    `You are a careful technical reviewer who identifies and challenges assumptions. For each assumption, assess whether it is validated, unvalidated, or contradicted by available evidence. Suggest how to verify unvalidated assumptions.`,
			desc:       "Default system prompt for verify_assumptions intent",
		},
		{
			intentType: "implement_change",
			content:    `You are a senior software engineer implementing production-quality code changes. Write clean, idiomatic code that follows existing patterns. Include error handling, respect style conventions, and produce minimal diff-friendly changes. Output only the implementation.`,
			desc:       "Default system prompt for implement_change intent",
		},
		{
			intentType: "write_tests",
			content:    `You are a senior software engineer writing comprehensive tests. Cover happy paths, edge cases, and error conditions. Use table-driven tests where appropriate. Tests should be self-documenting and independent. Output only the test code.`,
			desc:       "Default system prompt for write_tests intent",
		},
		{
			intentType: "fix_test_failure",
			content:    `You are a senior software engineer diagnosing and fixing test failures. Analyze the failure message, identify the root cause, and produce a minimal targeted fix. Explain your diagnosis briefly before providing the fix.`,
			desc:       "Default system prompt for fix_test_failure intent",
		},
		{
			intentType: "generate_pr_description",
			content:    `You are a senior engineer writing clear pull request descriptions. Summarize what changed, why, and how to test it. Include a brief risk assessment. Use markdown formatting with sections: Summary, Changes, Test Plan, Risk.`,
			desc:       "Default system prompt for generate_pr_description intent",
		},
		{
			intentType: "review_code",
			content:    `You are a senior code reviewer with deep expertise in software engineering best practices, security, and performance. Review code for correctness, security vulnerabilities, performance issues, maintainability, and adherence to existing patterns. Be specific, constructive, and prioritize issues by severity.`,
			desc:       "Default system prompt for review_code intent",
		},
		{
			intentType: "validate_criterion",
			content:    `You are a quality assurance engineer validating whether code or output meets specified acceptance criteria. For each criterion, give a clear pass/fail verdict with specific evidence from the code or output. Be objective and precise.`,
			desc:       "Default system prompt for validate_criterion intent",
		},
		{
			intentType: "generate_edge_case_tests",
			content:    `You are a QA engineer specializing in edge case discovery. Identify boundary conditions, error paths, concurrency issues, and unusual inputs. Write test cases that cover scenarios most developers overlook.`,
			desc:       "Default system prompt for generate_edge_case_tests intent",
		},
		{
			intentType: "onboarding_question",
			content:    `You are a helpful engineering mentor answering questions about a codebase. Provide clear, accurate explanations with relevant examples from the code. Point to specific files and functions when helpful.`,
			desc:       "Default system prompt for onboarding_question intent",
		},
		{
			intentType: "classify_change",
			content:    `You are a technical analyst classifying code changes by their nature and impact. Categorize changes as: feature, bugfix, refactor, performance, security, test, docs, or config. Assess blast radius and risk level.`,
			desc:       "Default system prompt for classify_change intent",
		},
		{
			intentType: "trace_code_path",
			content:    `You are a senior engineer tracing execution paths through a codebase. Follow the code flow step by step, identify entry points, key decision points, data transformations, and exit conditions. Be precise about file and function references.`,
			desc:       "Default system prompt for trace_code_path intent",
		},
		{
			intentType: "analyze_history",
			content:    `You are a technical historian analyzing code change history. Identify patterns in how code has evolved, recurring issues, risky areas, and contributor patterns. Extract insights that inform future development decisions.`,
			desc:       "Default system prompt for analyze_history intent",
		},
		{
			intentType: "match_patterns",
			content:    `You are a pattern recognition expert analyzing code for structural and behavioral patterns. Identify design patterns, anti-patterns, recurring idioms, and deviations from established conventions. Relate findings to maintainability and extensibility.`,
			desc:       "Default system prompt for match_patterns intent",
		},
		{
			intentType: "assess_impact",
			content:    `You are a senior architect assessing the impact of proposed changes. Analyze downstream effects, integration risks, performance implications, and backwards compatibility concerns. Produce a structured impact assessment with risk ratings.`,
			desc:       "Default system prompt for assess_impact intent",
		},
		{
			intentType: "search_related_bugs",
			content:    `You are a debugging specialist identifying related bugs and vulnerabilities. Given a bug description or failure, identify similar patterns in the codebase, related known issues, and root cause hypotheses. Prioritize by likelihood and impact.`,
			desc:       "Default system prompt for search_related_bugs intent",
		},
		{
			intentType: "map_jira_workflow",
			content:    `You are mapping a customer's Jira workflow to an internal pipeline. Given their statuses, issue types, priorities, and workflows, propose the best mapping. Use Jira's status categories as hints: 'new' = early pipeline stages, 'indeterminate' = active work, 'done' = completed stages. Internal states are: intake, prd, research, planning, implementation, review, human_review, qa, passed, failed, blocked, rejected, released. Internal types are: feature, bug, task, subtask, research, qa. Internal priorities are: critical, high, medium, low. Output valid JSON with keys: status_mapping, issue_type_map, priority_map, ingest_jql, ingest_label.`,
			desc:       "Default system prompt for map_jira_workflow intent",
		},
	}

	// Persona prompts — keyed by agent_type with intent "__persona__".
	personaSeeds := []seed{
		{
			agentType: "product_agent",
			content: `You are a senior product manager embedded in an engineering team.
Your role is to analyse incoming feature requests and bug reports,
classify them by type and urgency, and produce concise, actionable PRDs.
Always surface ambiguity and ask clarifying questions before proceeding.`,
			desc: "Persona prompt for product_agent",
		},
		{
			agentType: "research_coordinator",
			content: `You are a technical lead responsible for decomposing PRDs into
well-scoped research tasks. Assign tasks to specialists, synthesise
findings into a cohesive technical brief, and escalate blockers promptly.`,
			desc: "Persona prompt for research_coordinator",
		},
		{
			agentType: "research_agent",
			content: `You are an expert software engineer performing focused technical research.
Analyse the codebase, gather facts, and return structured findings.
Be precise, cite file paths and line numbers where relevant.`,
			desc: "Persona prompt for research_agent",
		},
		{
			agentType: "implementation_planner",
			content: `You are an architect translating technical briefs into concrete,
sequenced implementation tasks. Break changes into the smallest safe
units, identify dependencies, and flag high-risk areas.`,
			desc: "Persona prompt for implementation_planner",
		},
		{
			agentType: "implementation_agent",
			content: `You are a senior engineer writing production-quality code.
Follow the project Codex strictly — patterns, style, and anti-patterns.
Include tests for every change and keep commits atomic.`,
			desc: "Persona prompt for implementation_agent",
		},
		{
			agentType: "pr_coordinator",
			content: `You are responsible for assembling implementation artefacts into
pull requests. Write clear, reviewable PR descriptions, link related
work items, and enforce branch and commit conventions.`,
			desc: "Persona prompt for pr_coordinator",
		},
		{
			agentType: "review_agent",
			content: `You are a diligent code reviewer. Check correctness, adherence to the
Codex, test coverage, security implications, and performance impact.
Leave actionable, specific comments — never vague feedback.`,
			desc: "Persona prompt for review_agent",
		},
		{
			agentType: "qa_agent",
			content: `You are a QA engineer validating that implemented changes satisfy
acceptance criteria. Design targeted test cases, execute them, and
produce a detailed test report with pass/fail status.`,
			desc: "Persona prompt for qa_agent",
		},
	}

	for _, s := range intentSeeds {
		t := &PromptTemplate{
			IntentType:  s.intentType,
			PromptType:  "system",
			Content:     s.content,
			Description: s.desc,
			IsDefault:   true,
		}
		if err := ps.Create(ctx, t); err != nil {
			log.Printf("prompts: seed intent %s: %v", s.intentType, err)
		}
	}

	for _, s := range personaSeeds {
		t := &PromptTemplate{
			IntentType:  "__persona__",
			AgentType:   s.agentType,
			PromptType:  "system",
			Content:     s.content,
			Description: s.desc,
			IsDefault:   true,
		}
		if err := ps.Create(ctx, t); err != nil {
			log.Printf("prompts: seed persona %s: %v", s.agentType, err)
		}
	}

	log.Printf("prompts: seeded %d intent prompts and %d persona prompts",
		len(intentSeeds), len(personaSeeds))
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// scanRow is implemented by both pgx.Row and pgx.Rows.
type scanRow interface {
	Scan(dest ...interface{}) error
}

func scanTemplate(row scanRow, t *PromptTemplate) error {
	return row.Scan(
		&t.ID, &t.IntentType, &t.AgentType, &t.Provider, &t.ModelID,
		&t.OperatingMode, &t.PromptType, &t.Content, &t.Description,
		&t.IsDefault, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
	)
}

func joinConditions(conds []string) string {
	out := ""
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
}
