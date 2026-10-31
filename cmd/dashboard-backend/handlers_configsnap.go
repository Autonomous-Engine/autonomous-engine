package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Configuration versions, outcomes and model prices (M4-05) ───────────────
//
// GET  /api/config-snapshots?id=              one snapshot document
// GET  /api/config-snapshots/diff?a=&b=       diff of two snapshots; b=current
//                                             captures the current default now
//                                             (not stored)
// GET  /api/admin/config-snapshots            snapshots with outcome rollups
// GET  /api/admin/model-prices                price rows
// POST /api/admin/model-prices                upsert a price row
// DELETE /api/admin/model-prices?id=          delete a price row

// registerConfigVersionRoutes mounts the M4-05 routes behind auth (the
// JWT middleware) and AdminOnly. Every route is admin-only: snapshot
// documents carry prompt templates, definition prompts and provider
// settings. The work-item badge shows the id and links into the admin page.
func registerConfigVersionRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, h *ConfigVersionHandlers) {
	mux.HandleFunc("/api/config-snapshots", auth(AdminOnly(h.GetSnapshot)))
	mux.HandleFunc("/api/config-snapshots/diff", auth(AdminOnly(h.DiffSnapshots)))
	mux.HandleFunc("/api/admin/config-snapshots", auth(AdminOnly(h.ListSnapshots)))
	mux.HandleFunc("/api/admin/model-prices", auth(AdminOnly(h.ModelPrices)))
}

// configDB is the pool surface these handlers use.
type configDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ConfigVersionHandlers serves the configuration-version and price APIs.
type ConfigVersionHandlers struct {
	db       configDB
	snaps    *configsnap.Store
	registry *agentregistry.Registry
	cs       *configstore.ConfigStore
}

// NewConfigVersionHandlers returns the handlers over pool.
func NewConfigVersionHandlers(pool *pgxpool.Pool, cs *configstore.ConfigStore) *ConfigVersionHandlers {
	return &ConfigVersionHandlers{db: pool, snaps: configsnap.NewStore(pool), registry: agentregistry.New(pool), cs: cs}
}

// GetSnapshot handles GET /api/config-snapshots?id=.
func (h *ConfigVersionHandlers) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s, err := h.snaps.Get(r.Context(), r.URL.Query().Get("id"))
	if errors.Is(err, configsnap.ErrNotFound) {
		writeError(w, http.StatusNotFound, "config snapshot not found")
		return
	}
	if err != nil {
		log.Printf("config-snapshots: get: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load config snapshot")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// DiffSnapshots handles GET /api/config-snapshots/diff?a=&b=. b defaults to
// "current": the configuration a run of a's agent type would capture now
// under the current default agent definition.
func (h *ConfigVersionHandlers) DiffSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	a, err := h.snaps.Get(ctx, r.URL.Query().Get("a"))
	if errors.Is(err, configsnap.ErrNotFound) {
		writeError(w, http.StatusNotFound, "config snapshot a not found")
		return
	}
	if err != nil {
		log.Printf("config-snapshots: diff a: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load config snapshot")
		return
	}
	bParam := r.URL.Query().Get("b")
	var bID string
	var bDoc configsnap.Document
	if bParam == "" || bParam == "current" {
		doc, err := h.currentDefault(ctx, a)
		if err != nil {
			log.Printf("config-snapshots: capture current default: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to capture the current default configuration")
			return
		}
		id, _, err := configsnap.Hash(doc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to hash the current configuration")
			return
		}
		bID, bDoc = id, doc
	} else {
		b, err := h.snaps.Get(ctx, bParam)
		if errors.Is(err, configsnap.ErrNotFound) {
			writeError(w, http.StatusNotFound, "config snapshot b not found")
			return
		}
		if err != nil {
			log.Printf("config-snapshots: diff b: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load config snapshot")
			return
		}
		bID, bDoc = b.ID, b.Document
	}
	changes, err := configsnap.Diff(a.Document, bDoc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to diff config snapshots")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"a": a.ID, "b": bID, "b_is_current": bParam == "" || bParam == "current",
		"identical": len(changes) == 0, "changes": changes,
	})
}

// currentDefault captures, without storing, what a run of a's agent type
// would record now under the default agent definition.
func (h *ConfigVersionHandlers) currentDefault(ctx context.Context, a *configsnap.Snapshot) (configsnap.Document, error) {
	def, err := h.registry.GetDefault(ctx)
	if err != nil {
		return nil, err
	}
	in := configsnap.Input{AgentType: a.AgentType, Model: configsnap.Model{Source: configsnap.SourceGlobalDefault}}
	switch {
	case def != nil && def.Provider != "" && def.ModelID != "":
		in.Model = configsnap.Model{Provider: def.Provider, ModelID: def.ModelID, Endpoint: def.ModelEndpoint, Source: configsnap.SourceAgentDefinition}
	case a.AgentType != configsnap.PipelineAgentType && h.cs != nil:
		// The global model as this agent type resolves it today.
		prov, _ := h.cs.ResolveCredential(ctx, "llm", "provider", a.AgentType, "instance-1")
		model, _ := h.cs.ResolveCredential(ctx, "llm", "model_name", a.AgentType, "instance-1")
		endpoint, _ := h.cs.ResolveCredential(ctx, "llm", "endpoint", a.AgentType, "instance-1")
		in.Model = configsnap.Model{Provider: prov, ModelID: model, Endpoint: endpoint, Source: configsnap.SourceGlobalDefault}
	}
	if def != nil {
		id := def.ID
		in.AgentDefinitionID = &id
	}
	return configsnap.Capture(ctx, h.db, in)
}

// snapshotRollup is one row of the Configuration versions page.
type snapshotRollup struct {
	ID                  string    `json:"id"`
	AgentType           string    `json:"agent_type"`
	Provider            string    `json:"provider"`
	ModelID             string    `json:"model_id"`
	AgentDefinitionID   *string   `json:"agent_definition_id"`
	AgentDefinitionName *string   `json:"agent_definition_name"`
	CreatedAt           time.Time `json:"created_at"`

	Items             int64    `json:"items"`
	Verified          int64    `json:"verified"`
	VerifiedRate      *float64 `json:"verified_rate"`
	DefectDataItems   int64    `json:"defect_data_items"`
	DefectsCritical   float64  `json:"defects_critical"`
	DefectsMajor      float64  `json:"defects_major"`
	DefectsMinor      float64  `json:"defects_minor"`
	HumanEditKnown    int64    `json:"human_edit_known"`
	HumanEdited       int64    `json:"human_edited"`
	HumanEditRate     *float64 `json:"human_edit_rate"`
	FalseSuccess      int64    `json:"false_success"`
	Reverted          int64    `json:"reverted"`
	InferenceCalls    int64    `json:"inference_calls"`
	TokensIn          int64    `json:"tokens_in"`
	TokensOut         int64    `json:"tokens_out"`
	CostUSD           *float64 `json:"cost_usd"`
	CostUnknown       bool     `json:"cost_unknown"`
	UnpricedCalls     int64    `json:"unpriced_calls"`
	IsCurrentDefault  bool     `json:"is_current_default"`
	ReferencedByItems int64    `json:"referenced_by_items"`

	// CostUSD above is the spend of the calls tagged with this snapshot,
	// i.e. this agent type's calls only. ItemCostUSD is the full per-item
	// cost (work_item_outcomes.cost_usd: every agent's calls on the items
	// whose outcome rolls up to this snapshot); nil when any item's cost is
	// unknown.
	ItemCostUSD          *float64 `json:"item_cost_usd"`
	ItemCostUnknownItems int64    `json:"item_cost_unknown_items"`
	ItemCostUnknown      bool     `json:"item_cost_unknown"`
}

const snapshotRollupSQL = `
WITH oc AS (
	SELECT config_snapshot_id AS sid,
	       COUNT(*) AS items,
	       -- "verified" means released on verified evidence: a reviewed head
	       -- or a human_review / legacy decision is not verification.
	       COUNT(*) FILTER (WHERE release_decision = 'release') AS verified,
	       SUM(cost_usd) AS item_cost,
	       COUNT(*) FILTER (WHERE cost_usd IS NULL) AS item_cost_unknown,
	       COUNT(*) FILTER (WHERE review_defects IS NOT NULL) AS defect_items,
	       COALESCE(SUM(CASE WHEN jsonb_typeof(review_defects->'critical') = 'number' THEN (review_defects->>'critical')::numeric END), 0) AS crit,
	       COALESCE(SUM(CASE WHEN jsonb_typeof(review_defects->'major') = 'number' THEN (review_defects->>'major')::numeric END), 0) AS maj,
	       COALESCE(SUM(CASE WHEN jsonb_typeof(review_defects->'minor') = 'number' THEN (review_defects->>'minor')::numeric END), 0) AS mnr,
	       COUNT(*) FILTER (WHERE human_edits_after_review IS NOT NULL) AS edit_known,
	       COUNT(*) FILTER (WHERE human_edits_after_review) AS edited,
	       COUNT(*) FILTER (WHERE false_success) AS false_success,
	       COUNT(*) FILTER (WHERE reverted_at IS NOT NULL) AS reverted
	FROM work_item_outcomes WHERE config_snapshot_id IS NOT NULL GROUP BY config_snapshot_id
), ic AS (
	SELECT config_snapshot_id AS sid,
	       COUNT(*) AS calls,
	       COALESCE(SUM(input_tokens), 0) AS tin,
	       COALESCE(SUM(output_tokens), 0) AS tout,
	       COUNT(*) FILTER (WHERE input_tokens + output_tokens > 0) AS token_calls,
	       COUNT(*) FILTER (WHERE input_tokens + output_tokens > 0 AND cost_usd IS NULL) AS unpriced,
	       SUM(cost_usd) AS cost
	FROM inference_log_costs WHERE config_snapshot_id IS NOT NULL GROUP BY config_snapshot_id
), wi AS (
	SELECT config_snapshot_id AS sid, COUNT(*) AS n FROM work_items WHERE config_snapshot_id IS NOT NULL GROUP BY config_snapshot_id
)
SELECT s.id, s.agent_type, s.provider, s.model_id, s.agent_definition_id::text, ad.name, s.created_at,
       COALESCE(oc.items, 0), COALESCE(oc.verified, 0), COALESCE(oc.defect_items, 0),
       COALESCE(oc.crit, 0)::float8, COALESCE(oc.maj, 0)::float8, COALESCE(oc.mnr, 0)::float8,
       COALESCE(oc.edit_known, 0), COALESCE(oc.edited, 0), COALESCE(oc.false_success, 0), COALESCE(oc.reverted, 0),
       COALESCE(ic.calls, 0), COALESCE(ic.tin, 0), COALESCE(ic.tout, 0), COALESCE(ic.token_calls, 0), COALESCE(ic.unpriced, 0),
       ic.cost::float8, COALESCE(wi.n, 0),
       oc.item_cost::float8, COALESCE(oc.item_cost_unknown, 0)
FROM config_snapshots s
LEFT JOIN agent_definitions ad ON ad.id = s.agent_definition_id
LEFT JOIN oc ON oc.sid = s.id
LEFT JOIN ic ON ic.sid = s.id
LEFT JOIN wi ON wi.sid = s.id
ORDER BY s.created_at DESC, s.id
LIMIT 500`

// ListSnapshots handles GET /api/admin/config-snapshots.
func (h *ConfigVersionHandlers) ListSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	rows, err := h.db.Query(ctx, snapshotRollupSQL)
	if err != nil {
		log.Printf("config-snapshots: list: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list config snapshots")
		return
	}
	defer rows.Close()
	out := []snapshotRollup{}
	for rows.Next() {
		var s snapshotRollup
		var tokenCalls int64
		if err := rows.Scan(&s.ID, &s.AgentType, &s.Provider, &s.ModelID, &s.AgentDefinitionID, &s.AgentDefinitionName, &s.CreatedAt,
			&s.Items, &s.Verified, &s.DefectDataItems, &s.DefectsCritical, &s.DefectsMajor, &s.DefectsMinor,
			&s.HumanEditKnown, &s.HumanEdited, &s.FalseSuccess, &s.Reverted,
			&s.InferenceCalls, &s.TokensIn, &s.TokensOut, &tokenCalls, &s.UnpricedCalls, &s.CostUSD, &s.ReferencedByItems,
			&s.ItemCostUSD, &s.ItemCostUnknownItems); err != nil {
			log.Printf("config-snapshots: scan: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to read config snapshots")
			return
		}
		finishRollup(&s, tokenCalls)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read config snapshots")
		return
	}
	// Mark the snapshots that equal what the default would capture now,
	// per agent type present in the list.
	current := map[string]string{}
	for i := range out {
		at := out[i].AgentType
		id, seen := current[at]
		if !seen {
			snap := &configsnap.Snapshot{AgentType: at}
			if doc, err := h.currentDefault(ctx, snap); err == nil {
				id, _, _ = configsnap.Hash(doc)
			}
			current[at] = id
		}
		out[i].IsCurrentDefault = id != "" && id == out[i].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// finishRollup derives the rates and the cost-unknown flag. Rates are nil
// (not 0) when their denominator is 0; cost is nil unless every call with
// tokens was priced.
func finishRollup(s *snapshotRollup, tokenCalls int64) {
	if s.Items > 0 {
		v := float64(s.Verified) / float64(s.Items)
		s.VerifiedRate = &v
	}
	if s.HumanEditKnown > 0 {
		v := float64(s.HumanEdited) / float64(s.HumanEditKnown)
		s.HumanEditRate = &v
	}
	if tokenCalls == 0 || s.UnpricedCalls > 0 {
		s.CostUSD = nil
	}
	s.CostUnknown = s.CostUSD == nil
	if s.Items == 0 || s.ItemCostUnknownItems > 0 {
		s.ItemCostUSD = nil
	}
	s.ItemCostUnknown = s.ItemCostUSD == nil
}

// costSummary is the /api/work-items/cost body. cost_usd is set only when
// every call with tokens has an effective price (model_prices); otherwise
// it is null and cost_unknown is true. Zero is never reported for unknown.
func costSummary(id uuid.UUID, in, out, calls, tokenCalls, unpriced int64, sum *float64) map[string]any {
	var cost any
	if tokenCalls > 0 && unpriced == 0 && sum != nil {
		cost = *sum
	}
	return map[string]any{
		"work_item_id":        id,
		"total_input_tokens":  in,
		"total_output_tokens": out,
		"total_tokens":        in + out,
		"total_llm_calls":     calls,
		"cost_usd":            cost,
		"cost_unknown":        cost == nil,
		"unpriced_calls":      unpriced,
	}
}

// ─── work item detail additions ──────────────────────────────────────────────

// configSnapshotID returns work_items.config_snapshot_id (nil when unset or
// unreadable: the detail page renders without it).
func (h *Handlers) configSnapshotID(ctx context.Context, id uuid.UUID) *string {
	var s *string
	if err := h.wg.DB().QueryRow(ctx, `SELECT config_snapshot_id FROM work_items WHERE id = $1`, id).Scan(&s); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("handlers: config snapshot for %s: %v", id, err)
		}
		return nil
	}
	return s
}

// workItemOutcome returns the item's work_item_outcomes row as a map, nil
// when none exists yet.
func (h *Handlers) workItemOutcome(ctx context.Context, id uuid.UUID) map[string]any {
	var raw []byte
	err := h.wg.DB().QueryRow(ctx, `SELECT to_jsonb(o) - 'release_decisions' FROM work_item_outcomes o WHERE work_item_id = $1`, id).Scan(&raw)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("handlers: outcome for %s: %v", id, err)
		}
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// ─── model_prices ─────────────────────────────────────────────────────────────

type modelPrice struct {
	ID            string    `json:"id"`
	Provider      string    `json:"provider"`
	ModelID       string    `json:"model_id"`
	InputPer1M    *float64  `json:"input_per_1m"`
	OutputPer1M   *float64  `json:"output_per_1m"`
	EffectiveFrom time.Time `json:"effective_from"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ModelPrices handles GET/POST/DELETE /api/admin/model-prices.
func (h *ConfigVersionHandlers) ModelPrices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listPrices(w, r)
	case http.MethodPost, http.MethodPut:
		h.upsertPrice(w, r)
	case http.MethodDelete:
		id, err := uuid.Parse(r.URL.Query().Get("id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid id")
			return
		}
		if _, err := h.db.Exec(r.Context(), `DELETE FROM model_prices WHERE id = $1`, id); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to delete price")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *ConfigVersionHandlers) listPrices(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `SELECT id::text, provider, model_id, input_per_1m::float8, output_per_1m::float8, effective_from, updated_at
		FROM model_prices ORDER BY provider, model_id, effective_from DESC`)
	if err != nil {
		log.Printf("model-prices: list: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list prices")
		return
	}
	defer rows.Close()
	out := []modelPrice{}
	for rows.Next() {
		var p modelPrice
		if err := rows.Scan(&p.ID, &p.Provider, &p.ModelID, &p.InputPer1M, &p.OutputPer1M, &p.EffectiveFrom, &p.UpdatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read prices")
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read prices")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": out})
}

type priceRequest struct {
	Provider      string   `json:"provider"`
	ModelID       string   `json:"model_id"`
	InputPer1M    *float64 `json:"input_per_1m"`
	OutputPer1M   *float64 `json:"output_per_1m"`
	EffectiveFrom string   `json:"effective_from"`
}

// validate checks a price request and returns its effective time. An empty
// effective_from is the epoch (the price applies to all past calls), which
// is also the placeholder rows' key, so editing a placeholder updates it.
func (p *priceRequest) validate() (time.Time, string) {
	p.Provider, p.ModelID = strings.TrimSpace(p.Provider), strings.TrimSpace(p.ModelID)
	if p.Provider == "" || p.ModelID == "" {
		return time.Time{}, "provider and model_id are required"
	}
	for _, v := range []*float64{p.InputPer1M, p.OutputPer1M} {
		if v != nil && *v < 0 {
			return time.Time{}, "prices must be non-negative"
		}
	}
	if p.EffectiveFrom == "" {
		return time.Unix(0, 0).UTC(), ""
	}
	t, err := time.Parse(time.RFC3339, p.EffectiveFrom)
	if err != nil {
		if d, derr := time.Parse("2006-01-02", p.EffectiveFrom); derr == nil {
			return d.UTC(), ""
		}
		return time.Time{}, "effective_from must be RFC3339 or YYYY-MM-DD"
	}
	return t.UTC(), ""
}

func (h *ConfigVersionHandlers) upsertPrice(w http.ResponseWriter, r *http.Request) {
	var req priceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	eff, msg := req.validate()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	var id string
	err := h.db.QueryRow(r.Context(), `INSERT INTO model_prices (provider, model_id, input_per_1m, output_per_1m, effective_from)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider, model_id, effective_from)
		DO UPDATE SET input_per_1m = EXCLUDED.input_per_1m, output_per_1m = EXCLUDED.output_per_1m, updated_at = NOW()
		RETURNING id::text`, req.Provider, req.ModelID, req.InputPer1M, req.OutputPer1M, eff).Scan(&id)
	if err != nil {
		log.Printf("model-prices: upsert: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to save price")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "saved"})
}
