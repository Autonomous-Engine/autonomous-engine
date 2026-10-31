package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// knownAgentTypes lists all agent types the system recognises.
var knownAgentTypes = []string{
	"product",
	"research",
	"research_coordinator",
	"implementation",
	"implementation_planner",
	"pr_coordinator",
	"review",
	"qa",
}

// Handlers holds the REST handler dependencies.
type Handlers struct {
	wg                    *workgraph.WorkGraph
	codex                 *codex.Codex
	msgBus                bus.MessageBus
	interactionsStore     *interactions.Store
	interactionsPublisher *interactions.Publisher
	// evidence reads implementation_verifications (migration 044). Nil in
	// tests that do not exercise evidence endpoints; handlers treat nil as
	// "no evidence available".
	evidence EvidenceStore
	// runs reads implementation_runs (migration 052) for the detail
	// payload. Nil in tests that do not exercise it.
	runs RunReader

	// approvals and txdb are what the manual approval paths use to commit a
	// transition and its event in one transaction (approvals_outbox.go).
	// NewHandlers wires both to wg; tests supply fakes.
	approvals approvalGraph
	txdb      workgraph.TxBeginner
	// codexDB reads and writes codex refusals (codex_refusal.go). Nil in
	// tests that do not exercise them: the blocked answers then take the
	// generic paths.
	codexDB codexRefusalDB
}

// NewHandlers creates a Handlers instance from the given WorkGraph and Codex.
func NewHandlers(wg *workgraph.WorkGraph, cx *codex.Codex, msgBus bus.MessageBus,
	iStore *interactions.Store, iPub *interactions.Publisher, evStore EvidenceStore) *Handlers {
	h := &Handlers{
		wg: wg, codex: cx, msgBus: msgBus,
		interactionsStore:     iStore,
		interactionsPublisher: iPub,
		evidence:              evStore,
	}
	if wg != nil {
		h.approvals = workGraphApprovals{wg}
		h.txdb = wg.DB()
		if pool := wg.DB(); pool != nil {
			h.runs = controller.NewStore(pool)
			h.codexDB = pool
		}
	}
	return h
}

// writeJSON writes v as JSON to w with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("handlers: encode response: %v", err)
	}
}

// writeError writes an {"error": msg} JSON body with the given status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ─── Work Items ───────────────────────────────────────────────────────────────

// ListWorkItems handles GET /api/work-items.
// An optional ?state= query parameter filters the result to items in that state.
// Without the parameter all items are returned grouped by state.
func (h *Handlers) ListWorkItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	stateParam := r.URL.Query().Get("state")

	var (
		items []*models.WorkItem
		err   error
	)

	if stateParam != "" {
		items, err = h.wg.GetByState(r.Context(), models.WorkItemState(stateParam))
	} else {
		items, err = h.listAllWorkItems(r.Context())
	}

	if err != nil {
		log.Printf("handlers: list work items: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if items == nil {
		items = []*models.WorkItem{}
	}
	// Codex coherence Part 1: each card shows the codex version and modules
	// the item was planned against, and whether the codex changed since.
	var pinDB rowQuerier
	if h.wg != nil {
		if p := h.wg.DB(); p != nil {
			pinDB = p
		}
	}
	writeJSON(w, http.StatusOK, withCodexPins(r.Context(), pinDB, items))
}

// listAllWorkItems fetches work items for every known state and aggregates them.
func (h *Handlers) listAllWorkItems(ctx context.Context) ([]*models.WorkItem, error) {
	states := []models.WorkItemState{
		models.WorkItemStateIntake,
		models.WorkItemStatePRD,
		models.WorkItemStatePRDApproval,
		models.WorkItemStateResearch,
		models.WorkItemStatePlanning,
		models.WorkItemStateImplementation,
		models.WorkItemStateVerification,
		models.WorkItemStateReview,
		models.WorkItemStateHumanReview,
		models.WorkItemStateQA,
		models.WorkItemStatePassed,
		models.WorkItemStateFailed,
		models.WorkItemStateAwaitingUser,
		models.WorkItemStateBlocked,
		models.WorkItemStateRejected,
		models.WorkItemStateReleased,
		models.WorkItemStateCancelled,
	}

	var all []*models.WorkItem
	for _, state := range states {
		items, err := h.wg.GetByState(ctx, state)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	return all, nil
}

// GetWorkItem handles GET /api/work-items/detail?id=<uuid>.
// Returns the work item plus its full lifecycle data: transitions, children,
// artifacts, agent assignments, inference logs, repository, and product info.
func (h *Handlers) GetWorkItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}

	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	// Core work item
	item, err := h.wg.GetWorkItem(r.Context(), id)
	if err != nil {
		log.Printf("handlers: get work item %s: %v", id, err)
		writeError(w, http.StatusNotFound, "work item not found")
		return
	}

	// State transitions (timeline)
	transitions, err := h.wg.GetTransitions(r.Context(), id)
	if err != nil {
		log.Printf("handlers: get transitions for %s: %v", id, err)
		transitions = []*models.WorkItemTransition{}
	}
	if transitions == nil {
		transitions = []*models.WorkItemTransition{}
	}

	// Children (sub-tasks)
	children, err := h.wg.GetChildren(r.Context(), id)
	if err != nil {
		log.Printf("handlers: get children for %s: %v", id, err)
		children = []*models.WorkItem{}
	}
	if children == nil {
		children = []*models.WorkItem{}
	}

	// Artifacts (PRD, technical brief, review comments, QA report, etc.)
	var artifacts []map[string]interface{}
	aRows, aErr := h.wg.DB().Query(r.Context(),
		`SELECT id, artifact_type, content, agent_id, created_at
		 FROM work_item_artifacts WHERE work_item_id = $1 ORDER BY created_at`, id)
	if aErr == nil {
		defer aRows.Close()
		for aRows.Next() {
			var aid, agentID string
			var artifactType string
			var content json.RawMessage
			var createdAt time.Time
			if scanErr := aRows.Scan(&aid, &artifactType, &content, &agentID, &createdAt); scanErr == nil {
				artifacts = append(artifacts, map[string]interface{}{
					"id":            aid,
					"artifact_type": artifactType,
					"content":       content,
					"agent_id":      agentID,
					"created_at":    createdAt,
				})
			}
		}
	} else {
		log.Printf("handlers: get artifacts for %s: %v", id, aErr)
	}
	if artifacts == nil {
		artifacts = []map[string]interface{}{}
	}

	// Agent definition info — agent_definition_id was added in migration 012
	// and is not on the WorkItem struct, so query it directly.
	var agentDef map[string]interface{}
	var defName, defProvider, defModelID string
	defErr := h.wg.DB().QueryRow(r.Context(),
		`SELECT ad.name, ad.provider, ad.model_id
		 FROM agent_definitions ad
		 JOIN work_items wi ON wi.agent_definition_id = ad.id
		 WHERE wi.id = $1`, id,
	).Scan(&defName, &defProvider, &defModelID)
	if defErr == nil && defName != "" {
		agentDef = map[string]interface{}{
			"name":     defName,
			"provider": defProvider,
			"model_id": defModelID,
		}
	}

	// Agent assignments for this work item
	var assignments []map[string]interface{}
	assRows, assErr := h.wg.DB().Query(r.Context(),
		`SELECT id, agent_type, agent_instance_id, status, assigned_at, completed_at
		 FROM agent_assignments WHERE work_item_id = $1 ORDER BY assigned_at`, id)
	if assErr == nil {
		defer assRows.Close()
		for assRows.Next() {
			var aid, atype, instanceID, status string
			var assignedAt time.Time
			var completedAt *time.Time
			if scanErr := assRows.Scan(&aid, &atype, &instanceID, &status, &assignedAt, &completedAt); scanErr == nil {
				assignments = append(assignments, map[string]interface{}{
					"id":           aid,
					"agent_type":   atype,
					"instance_id":  instanceID,
					"status":       status,
					"assigned_at":  assignedAt,
					"completed_at": completedAt,
				})
			}
		}
	} else {
		log.Printf("handlers: get assignments for %s: %v", id, assErr)
	}
	if assignments == nil {
		assignments = []map[string]interface{}{}
	}

	// Inference logs for this work item (token usage, model, latency)
	// agent_id in inference_logs is a UUID (nullable), cast to text for simplicity.
	var inferenceLogs []map[string]interface{}
	iRows, iErr := h.wg.DB().Query(r.Context(),
		`SELECT id, model_provider, model_id, intent_type, input_tokens, output_tokens,
		        latency_ms, success, error_message, COALESCE(agent_id::text, ''), created_at,
		        prompt_messages, response_content,
		        COALESCE(role, ''), COALESCE(agent_type, ''), COALESCE(config_snapshot_id, '')
		 FROM inference_logs WHERE work_item_id = $1 ORDER BY created_at`, id)
	if iErr == nil {
		defer iRows.Close()
		for iRows.Next() {
			var lid, provider, modelID, intentType, agentID, errMsg, respContent string
			var role, agentType, snapshotID string
			var inputTok, outputTok int
			var latency int64
			var success bool
			var createdAt time.Time
			var promptMessages []byte
			if scanErr := iRows.Scan(&lid, &provider, &modelID, &intentType, &inputTok, &outputTok,
				&latency, &success, &errMsg, &agentID, &createdAt, &promptMessages, &respContent,
				&role, &agentType, &snapshotID); scanErr == nil {
				// prompt_messages is jsonb; ship as-is so the UI can render
				// the [{role, content}, ...] array without double-encoding.
				var promptPayload interface{} = json.RawMessage(promptMessages)
				if len(promptMessages) == 0 {
					promptPayload = []interface{}{}
				}
				inferenceLogs = append(inferenceLogs, map[string]interface{}{
					"id":               lid,
					"model":            provider + "/" + modelID,
					"intent":           intentType,
					"input_tokens":     inputTok,
					"output_tokens":    outputTok,
					"latency_ms":       latency,
					"success":          success,
					"error":            errMsg,
					"agent_id":         agentID,
					"created_at":       createdAt,
					"prompt_messages":  promptPayload,
					"response_content": respContent,
					// M4-05 cost tagging; "" for rows written before 053.
					"role":               role,
					"agent_type":         agentType,
					"config_snapshot_id": snapshotID,
				})
			}
		}
	} else {
		log.Printf("handlers: get inference logs for %s: %v", id, iErr)
	}
	if inferenceLogs == nil {
		inferenceLogs = []map[string]interface{}{}
	}

	// Repository info
	var repoInfo map[string]interface{}
	if item.RepositoryID != nil {
		var repoName, githubURL string
		if scanErr := h.wg.DB().QueryRow(r.Context(),
			"SELECT name, github_url FROM repositories WHERE id = $1", *item.RepositoryID,
		).Scan(&repoName, &githubURL); scanErr == nil && repoName != "" {
			repoInfo = map[string]interface{}{"name": repoName, "github_url": githubURL}
		}
	}

	// Product info
	var productInfo map[string]interface{}
	if item.ProductID != nil {
		var prodName string
		if scanErr := h.wg.DB().QueryRow(r.Context(),
			"SELECT name FROM products WHERE id = $1", *item.ProductID,
		).Scan(&prodName); scanErr == nil && prodName != "" {
			productInfo = map[string]interface{}{"name": prodName}
		}
	}

	// Latest verification evidence (M0-08). Nil when none has been recorded;
	// the full history is at /api/work-items/evidence.
	latestVerification := h.latestEvidence(r.Context(), id)

	// Repository snapshot pins and their audit trail (M2-02).
	var snapDB rowQuerier
	if p := h.wg.DB(); p != nil {
		snapDB = p
	}
	basePins, baseEvents := loadBaseSnapshot(r.Context(), snapDB, id)
	// Codex coherence Part 1: the codex pin and what each codex change did.
	codexPin, codexEvents := loadCodexPin(r.Context(), snapDB, id)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"item":             item,
		"transitions":      transitions,
		"children":         children,
		"artifacts":        artifacts,
		"agent_definition": agentDef,
		"assignments":      assignments,
		"inference_logs":   inferenceLogs,
		"repository":       repoInfo,
		"product":          productInfo,
		"evidence":         latestVerification,
		// Latest controller run per subtask with its red/green evidence
		// (M3-02), read from implementation_runs as recorded.
		"implementation_runs": h.implementationRuns(r.Context(), id),
		"base_shas":           basePins,
		"base_sha_events":     baseEvents,
		// Cross-repository contract edges between its subtasks and their
		// last PR-time check (M2-06).
		"contract_edges": loadContractEdges(r.Context(), snapDB, id),
		// Codex coherence Part 1: the codex the item was planned against
		// (null when unpinned) and its re-plans, notes and escalations.
		"codex_pin":    codexPin,
		"codex_events": codexEvents,
		// M4-05: the configuration snapshot the item runs under and its
		// production outcome (nil until status-orchestrator records one).
		"config_snapshot_id": h.configSnapshotID(r.Context(), id),
		"outcome":            h.workItemOutcome(r.Context(), id),
	})
}

// GetQueueCounts handles GET /api/queue/counts.
// Returns a map of state → count for all work items.
func (h *Handlers) GetQueueCounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	counts, err := h.wg.CountByState(r.Context())
	if err != nil {
		log.Printf("handlers: count by state: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	// Convert map key type to string for JSON serialisation.
	result := make(map[string]int, len(counts))
	for state, count := range counts {
		result[string(state)] = count
	}
	writeJSON(w, http.StatusOK, result)
}

// ─── Codex ───────────────────────────────────────────────────────────────────

// ListCodexPatterns handles GET /api/codex/patterns.
// An optional ?category= query parameter filters by category.
func (h *Handlers) ListCodexPatterns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	category := r.URL.Query().Get("category")
	productID := r.URL.Query().Get("product_id")

	var (
		patterns []*models.CodexPattern
		err      error
	)

	if category != "" {
		patterns, err = h.codex.GetPatternsByCategory(r.Context(), category)
	} else if productID != "" {
		patterns, err = h.getPatternsByProduct(r.Context(), productID)
	} else {
		patterns, err = h.getAllPatterns(r.Context())
	}

	if err != nil {
		log.Printf("handlers: list codex patterns: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	if patterns == nil {
		patterns = []*models.CodexPattern{}
	}
	writeJSON(w, http.StatusOK, patterns)
}

// getAllPatterns fetches all patterns by querying codex_patterns directly via
// the pool. Since the Codex struct does not expose a list-all method we use the
// section type listing as an indirect approach — but the cleanest approach is a
// direct pool query. We expose this via GetSectionsByType on the pattern table.
// For now we query each section and collect the categories seen, then fetch by
// each category to build the full list.
//
// As a pragmatic alternative, we fetch all patterns for the empty-string
// category which (for codex_patterns) would return nothing. Instead we use the
// sections API to build the union.
//
// The simplest correct implementation: query sections of type "pattern" to get
// category names, then query patterns per category.
func (h *Handlers) getAllPatterns(ctx context.Context) ([]*models.CodexPattern, error) {
	// Query all patterns directly
	rows, err := h.wg.DB().Query(ctx,
		"SELECT id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes FROM codex_patterns ORDER BY category, name")
	if err != nil {
		return []*models.CodexPattern{}, nil
	}
	defer rows.Close()

	var all []*models.CodexPattern
	for rows.Next() {
		p := &models.CodexPattern{}
		if err := rows.Scan(&p.ID, &p.Name, &p.Category, &p.WhenToUse, &p.ReferenceFiles, &p.Rationale, &p.KeyRules, &p.CommonMistakes); err != nil {
			continue
		}
		all = append(all, p)
	}
	if all == nil {
		all = []*models.CodexPattern{}
	}
	return all, nil
}

func (h *Handlers) getPatternsByProduct(ctx context.Context, productID string) ([]*models.CodexPattern, error) {
	rows, err := h.wg.DB().Query(ctx,
		"SELECT id, name, category, when_to_use, reference_files, rationale, key_rules, common_mistakes FROM codex_patterns WHERE product_id = $1 ORDER BY category, name", productID)
	if err != nil {
		return []*models.CodexPattern{}, nil
	}
	defer rows.Close()

	var all []*models.CodexPattern
	for rows.Next() {
		p := &models.CodexPattern{}
		if err := rows.Scan(&p.ID, &p.Name, &p.Category, &p.WhenToUse, &p.ReferenceFiles, &p.Rationale, &p.KeyRules, &p.CommonMistakes); err != nil {
			continue
		}
		all = append(all, p)
	}
	if all == nil {
		all = []*models.CodexPattern{}
	}
	return all, nil
}

// GetCodexVersion handles GET /api/codex/version?product_id=<uuid>[&branch=<name>].
//
// Codex versions are numbered per product and branch (migration 060), so
// product_id is required; branch defaults to the product's default branch.
// The response is the latest version of that product and branch: version,
// version_number and product_version all carry its product_version (0 when
// the branch has none yet; version_number is the field the dashboard reads),
// with branch, product_id, change_summary, sections_changed and created_at.
func (h *Handlers) GetCodexVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	productID, err := uuid.Parse(r.URL.Query().Get("product_id"))
	if err != nil || productID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "product_id is required: codex versions are per product and branch")
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch, err = codex.ResolveDefaultBranch(r.Context(), h.codex.Pool(), productID)
		switch {
		case errors.Is(err, codex.ErrNoDefaultBranch):
			// No enabled repository yet: no codex version either. 200 with
			// version 0, so the Codex view (which loads this with the
			// modules) still renders.
			branch = ""
		case err != nil:
			log.Printf("handlers: get codex version: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
	}

	var latest *models.CodexVersion
	if branch != "" {
		latest, err = h.codex.GetLatestVersion(r.Context(), productID, branch)
		if err != nil {
			log.Printf("handlers: get codex version: %v", err)
			writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
	}
	resp := map[string]any{
		"product_id":       productID.String(),
		"branch":           branch,
		"version":          0,
		"version_number":   0,
		"product_version":  0,
		"change_summary":   "",
		"sections_changed": []string{},
	}
	if latest != nil {
		resp["version"] = latest.ProductVersion
		resp["version_number"] = latest.ProductVersion
		resp["product_version"] = latest.ProductVersion
		resp["change_summary"] = latest.ChangeSummary
		resp["sections_changed"] = latest.SectionsChanged
		resp["created_at"] = latest.CreatedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// SearchCodex handles GET /api/codex/search?q=<text>&type=<section>.
// Case-insensitive SQL search across all four codex section tables.
// Returns a flat list of {kind, id, name, snippet, ...} records sorted by
// simple relevance (name-match wins over body-match, capped at 50 results).
// Semantic search via qdrant is a separate endpoint (future).
func (h *Handlers) SearchCodex(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	typeFilter := r.URL.Query().Get("type") // "" | "modules" | "patterns" | "anti_patterns" | "sections"
	like := "%" + q + "%"
	ctx := r.Context()
	db := h.wg.DB()

	type hit struct {
		Kind            string `json:"kind"`
		ID              string `json:"id"`
		Name            string `json:"name"`
		Snippet         string `json:"snippet"`
		Source          string `json:"source"`
		State           string `json:"state"`
		NameHit         bool   `json:"name_hit"`
		SourceCommitSHA string `json:"source_commit_sha"`
	}
	out := []hit{}

	// codex_modules: name + purpose + internal_patterns
	if typeFilter == "" || typeFilter == "modules" {
		rows, err := db.Query(ctx, `
			SELECT id, name, COALESCE(purpose, ''), source, state,
			       (name ILIKE $1) AS name_hit, COALESCE(source_commit_sha, '')
			FROM codex_modules
			WHERE state='active' AND (name ILIKE $1 OR purpose ILIKE $1 OR internal_patterns ILIKE $1)
			LIMIT 50
		`, like)
		if err == nil {
			for rows.Next() {
				h := hit{Kind: "module"}
				_ = rows.Scan(&h.ID, &h.Name, &h.Snippet, &h.Source, &h.State, &h.NameHit, &h.SourceCommitSHA)
				out = append(out, h)
			}
			rows.Close()
		}
	}
	// codex_patterns: name + rationale + when_to_use
	if typeFilter == "" || typeFilter == "patterns" {
		rows, err := db.Query(ctx, `
			SELECT id, name, COALESCE(rationale, ''), source, state,
			       (name ILIKE $1) AS name_hit, COALESCE(source_commit_sha, '')
			FROM codex_patterns
			WHERE state='active' AND (name ILIKE $1 OR rationale ILIKE $1 OR when_to_use ILIKE $1)
			LIMIT 50
		`, like)
		if err == nil {
			for rows.Next() {
				h := hit{Kind: "pattern"}
				_ = rows.Scan(&h.ID, &h.Name, &h.Snippet, &h.Source, &h.State, &h.NameHit, &h.SourceCommitSHA)
				out = append(out, h)
			}
			rows.Close()
		}
	}
	// codex_anti_patterns: name + why_wrong + what_instead
	if typeFilter == "" || typeFilter == "anti_patterns" {
		rows, err := db.Query(ctx, `
			SELECT id, name, COALESCE(why_wrong, ''), source, state,
			       (name ILIKE $1) AS name_hit, COALESCE(source_commit_sha, '')
			FROM codex_anti_patterns
			WHERE state='active' AND (name ILIKE $1 OR why_wrong ILIKE $1 OR what_instead ILIKE $1)
			LIMIT 50
		`, like)
		if err == nil {
			for rows.Next() {
				h := hit{Kind: "anti_pattern"}
				_ = rows.Scan(&h.ID, &h.Name, &h.Snippet, &h.Source, &h.State, &h.NameHit, &h.SourceCommitSHA)
				out = append(out, h)
			}
			rows.Close()
		}
	}
	// codex_sections: name + content (jsonb cast to text)
	if typeFilter == "" || typeFilter == "sections" {
		rows, err := db.Query(ctx, `
			SELECT id, name, COALESCE(content::text, ''), source, state,
			       (name ILIKE $1) AS name_hit, COALESCE(source_commit_sha, '')
			FROM codex_sections
			WHERE state='active' AND (name ILIKE $1 OR content::text ILIKE $1)
			LIMIT 50
		`, like)
		if err == nil {
			for rows.Next() {
				h := hit{Kind: "section"}
				_ = rows.Scan(&h.ID, &h.Name, &h.Snippet, &h.Source, &h.State, &h.NameHit, &h.SourceCommitSHA)
				out = append(out, h)
			}
			rows.Close()
		}
	}

	// Rank: name hits first, then body hits; cap result size so the UI stays fast.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NameHit != out[j].NameHit {
			return out[i].NameHit
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 100 {
		out = out[:100]
	}
	// Trim snippets so the payload stays small.
	for i := range out {
		if len(out[i].Snippet) > 240 {
			out[i].Snippet = out[i].Snippet[:240] + "…"
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ListCodexModules handles GET /api/codex/modules
func (h *Handlers) ListCodexModules(w http.ResponseWriter, r *http.Request) {
	productID := r.URL.Query().Get("product_id")
	var rows pgx.Rows
	var err error
	if productID != "" {
		rows, err = h.wg.DB().Query(r.Context(),
			"SELECT id, name, purpose, dependencies, dependents, risk_level, test_coverage_notes FROM codex_modules WHERE product_id = $1 ORDER BY name", productID)
	} else {
		rows, err = h.wg.DB().Query(r.Context(),
			"SELECT id, name, purpose, dependencies, dependents, risk_level, test_coverage_notes FROM codex_modules ORDER BY name")
	}
	if err != nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	defer rows.Close()

	var modules []map[string]interface{}
	for rows.Next() {
		var id, name, purpose, riskLevel, testNotes string
		var deps, dependents []string
		if err := rows.Scan(&id, &name, &purpose, &deps, &dependents, &riskLevel, &testNotes); err != nil {
			continue
		}
		modules = append(modules, map[string]interface{}{
			"id": id, "name": name, "purpose": purpose,
			"dependencies": deps, "dependents": dependents,
			"risk_level": riskLevel, "test_coverage_notes": testNotes,
		})
	}
	if modules == nil {
		modules = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, modules)
}

// ListCodexSections handles GET /api/codex/sections
func (h *Handlers) ListCodexSections(w http.ResponseWriter, r *http.Request) {
	productID := r.URL.Query().Get("product_id")
	var rows pgx.Rows
	var err error
	if productID != "" {
		rows, err = h.wg.DB().Query(r.Context(),
			"SELECT id, section_type, name, content, version, updated_at FROM codex_sections WHERE product_id = $1 ORDER BY section_type, name", productID)
	} else {
		rows, err = h.wg.DB().Query(r.Context(),
			"SELECT id, section_type, name, content, version, updated_at FROM codex_sections ORDER BY section_type, name")
	}
	if err != nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	defer rows.Close()

	var sections []map[string]interface{}
	for rows.Next() {
		var id, sectionType, name string
		var content json.RawMessage
		var version int
		var updatedAt time.Time
		if err := rows.Scan(&id, &sectionType, &name, &content, &version, &updatedAt); err != nil {
			continue
		}
		sections = append(sections, map[string]interface{}{
			"id": id, "section_type": sectionType, "name": name,
			"content": content, "version": version, "updated_at": updatedAt.Format(time.RFC3339),
		})
	}
	if sections == nil {
		sections = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, sections)
}

// GraphForProduct handles GET /api/codex/graph?product_id=<uuid>.
//
// It returns nodes + edges for the codex graph of a single product. External
// dependencies that don't resolve to an indexed module within the same
// product are emitted as synthetic `external:<name>` nodes so the viewer can
// still show them (honest about what's known vs not known).
//
// Fail-honestly contract:
//   - missing/invalid product_id → 400 with the real reason
//   - product_id not found in products → 404
//   - DB error → 500 with err text
//   - empty result (product exists but has 0 indexed modules) → 200 with
//     empty nodes/edges and meta.empty_reason="no_modules_indexed"
func (h *Handlers) GraphForProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id: "+err.Error())
		return
	}

	// Confirm product exists (404 vs 500 distinction).
	var productName string
	if err := h.wg.DB().QueryRow(r.Context(),
		`SELECT name FROM products WHERE id = $1`, productID,
	).Scan(&productName); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "product not found: "+productID.String())
			return
		}
		log.Printf("handlers: graph product lookup: %v", err)
		writeError(w, http.StatusInternalServerError, "product lookup: "+err.Error())
		return
	}

	rows, err := h.wg.DB().Query(r.Context(), `
		SELECT id, name, purpose, public_interface, dependencies, dependents,
		       risk_level, owner_domain, test_coverage_notes, repository_id
		FROM codex_modules
		WHERE product_id = $1
		ORDER BY name`,
		productID,
	)
	if err != nil {
		log.Printf("handlers: graph query: %v", err)
		writeError(w, http.StatusInternalServerError, "query modules: "+err.Error())
		return
	}
	defer rows.Close()

	var mods []*models.CodexModule
	for rows.Next() {
		m := &models.CodexModule{}
		var repoID *uuid.UUID
		if err := rows.Scan(
			&m.ID, &m.Name, &m.Purpose, &m.PublicInterface,
			&m.Dependencies, &m.Dependents,
			&m.RiskLevel, &m.OwnerDomain, &m.TestCoverageNotes,
			&repoID,
		); err != nil {
			log.Printf("handlers: graph scan: %v", err)
			writeError(w, http.StatusInternalServerError, "scan module: "+err.Error())
			return
		}
		m.RepositoryID = repoID
		mods = append(mods, m)
	}
	if err := rows.Err(); err != nil {
		log.Printf("handlers: graph rows: %v", err)
		writeError(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}

	resp := BuildGraph(mods)
	resp.ProductID = productID.String()
	resp.ProductName = productName
	resp.GeneratedAt = time.Now().UTC().Format(time.RFC3339)

	writeJSON(w, http.StatusOK, resp)
}

// ─── Cancellation ────────────────────────────────────────────────────────────

// CancelWorkItem handles POST /api/work-items/cancel?id=<uuid>.
// Cancels the work item and cascades cancellation to all non-terminal children.
func (h *Handlers) CancelWorkItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}

	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "admin"
	}

	if err := h.wg.TransitionState(r.Context(), id, models.WorkItemStateCancelled, userID, "Cancelled by admin"); err != nil {
		writeError(w, http.StatusBadRequest, "Cannot cancel: "+err.Error())
		return
	}
	h.dropRefusalLogged(r.Context(), id)

	// Cascade cancellation to all children that are not already in a terminal state.
	children, _ := h.wg.GetChildren(r.Context(), id)
	for _, child := range children {
		if child.State != models.WorkItemStatePassed &&
			child.State != models.WorkItemStateReleased &&
			child.State != models.WorkItemStateCancelled {
			if tErr := h.wg.TransitionState(r.Context(), child.ID, models.WorkItemStateCancelled, userID, "Parent cancelled"); tErr != nil {
				log.Printf("handlers: cancel child %s: %v", child.ID, tErr)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// ─── Manual Retry ─────────────────────────────────────────────────────────────

// RetryWorkItem handles POST /api/work-items/retry?id=<uuid>.
// Resets a blocked or failed work item to the prd state and re-publishes it.
func (h *Handlers) RetryWorkItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}

	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	item, err := h.approvals.GetWorkItem(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "work item not found")
		return
	}

	if item.State != models.WorkItemStateBlocked && item.State != models.WorkItemStateFailed {
		writeError(w, http.StatusBadRequest, "can only retry blocked or failed work items")
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "admin"
	}

	productID := ""
	if item.ProductID != nil {
		productID = item.ProductID.String()
	}

	// ── Subtask retry: skip PRD logic, re-queue directly for implementation ──
	if item.Type == models.WorkItemTypeSubtask {
		// The transition and implementation.tasks commit together.
		if err := h.retrySubtask(r.Context(), item, userID, "Manual retry by admin — subtask re-queued for implementation"); err != nil {
			log.Printf("handlers: retry subtask %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "retry failed: "+err.Error())
			return
		}
		log.Printf("handlers: retry for subtask %s routed to implementation.tasks", id)
		h.dropRefusalLogged(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "retrying"})
		return
	}

	// Check whether a PRD artifact exists. If one was already generated,
	// we can skip straight to research by publishing to prd.ready. If
	// NOT (e.g. the clarity gate blocked before PRD generation), we must
	// re-run the product-agent by publishing to intake — otherwise
	// research runs with an empty PRD and produces garbage.
	hasPRD := false
	prdContent := ""
	workType := ""
	if artifacts, aErr := h.approvals.GetArtifacts(r.Context(), id, models.ArtifactTypePRD); aErr == nil && len(artifacts) > 0 {
		var prdArt struct {
			PRD      string `json:"prd"`
			WorkType string `json:"work_type"`
		}
		if jErr := json.Unmarshal(artifacts[len(artifacts)-1].Content, &prdArt); jErr == nil && prdArt.PRD != "" {
			prdContent = prdArt.PRD
			workType = prdArt.WorkType
			hasPRD = true
		}
	}

	if hasPRD {
		// PRD exists — transition to prd and enqueue prd.ready (one
		// transaction) so research-coordinator picks it up with full PRD
		// context.
		payload := map[string]interface{}{
			"work_id":     id.String(),
			"product_id":  productID,
			"work_type":   workType,
			"prd_content": prdContent,
			"retry":       true,
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}
		if err := h.transitionAndEnqueue(r.Context(), id, item, models.WorkItemStatePRD, userID, "Manual retry by admin", bus.TopicPRDReady, id.String(), payload); err != nil {
			log.Printf("handlers: retry %s to prd: %v", id, err)
			writeError(w, http.StatusInternalServerError, "retry failed: "+err.Error())
			return
		}
	} else {
		// No PRD — transition to intake and re-run the full pipeline
		// starting from product-agent so a proper PRD is generated first.
		payload := map[string]interface{}{
			"work_id":     id.String(),
			"product_id":  productID,
			"description": item.Description,
			"source":      "retry",
			"retry":       true,
		}
		if err := h.transitionAndEnqueue(r.Context(), id, item, models.WorkItemStateIntake, userID, "Manual retry by admin — re-running from intake (no PRD exists)", bus.TopicIntake, id.String(), payload); err != nil {
			log.Printf("handlers: retry %s to intake: %v", id, err)
			writeError(w, http.StatusInternalServerError, "retry failed: "+err.Error())
			return
		}
		log.Printf("handlers: retry for %s routed to intake (no PRD artifact found)", id)
	}

	h.dropRefusalLogged(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "retrying"})
}

// UpdateAndRetry handles POST /api/work-items/update-and-retry?id=<uuid>.
// Body: {"description":"..."}. Updates the work item's description AND
// transitions it back to `intake`, republishing to the intake topic so
// product-agent reruns the full PRD flow (clarity check + generation).
//
// This is the escape hatch for blocked items: when product-agent's clarity
// check blocks a work item with questions, the operator edits the
// description to answer them and hits this endpoint. Unlike RetryWorkItem
// (which jumps to `prd` and republishes prd.ready), this reruns from the
// top so the clarity check + re-generation both get a fresh pass.
func (h *Handlers) UpdateAndRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Description) == "" {
		writeError(w, http.StatusBadRequest, "description is required")
		return
	}

	item, err := h.wg.GetWorkItem(r.Context(), id)
	if err != nil || item == nil {
		writeError(w, http.StatusNotFound, "work item not found")
		return
	}
	if item.State != models.WorkItemStateBlocked && item.State != models.WorkItemStateFailed {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("can only update+retry blocked or failed items (this one is %q)", item.State))
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "admin"
	}

	// Cancel any existing subtask children from prior attempts. Without
	// this, a zombie 'blocked' or 'passed' subtask from the previous run
	// lingers under the parent, and pr-coordinator's "all children
	// complete" gate either blocks forever (zombie blocked) or opens a
	// PR from a prior attempt's work (zombie passed) instead of the
	// fresh attempt's work. Each blocked→intake retry is a fresh
	// attempt; branches pushed by prior attempts remain on GitHub as
	// historical artifacts but are no longer owned by this work item.
	// We record a cancellation transition per child for audit clarity.
	rows, cancelQueryErr := h.wg.DB().Query(r.Context(),
		`SELECT id::text, state::text FROM work_items WHERE parent_id = $1 AND state::text != 'cancelled'`,
		id)
	var cancelledChildren []string
	if cancelQueryErr == nil {
		for rows.Next() {
			var childID, childState string
			if scanErr := rows.Scan(&childID, &childState); scanErr == nil {
				cancelledChildren = append(cancelledChildren, childID+"|"+childState)
			}
		}
		rows.Close()
	}
	for _, entry := range cancelledChildren {
		parts := strings.SplitN(entry, "|", 2)
		if len(parts) != 2 {
			continue
		}
		childID, childState := parts[0], parts[1]
		if _, uErr := h.wg.DB().Exec(r.Context(),
			`UPDATE work_items SET state = 'cancelled', updated_at = NOW() WHERE id = $1`,
			childID); uErr != nil {
			log.Printf("handlers: cancel prior child %s: %v", childID, uErr)
			continue
		}
		_, _ = h.wg.DB().Exec(r.Context(),
			`INSERT INTO work_item_transitions (work_item_id, from_state, to_state, agent_id, reason)
			 VALUES ($1, $2::work_item_state, 'cancelled', $3, 'parent re-triggered via update-and-retry — this attempt superseded')`,
			childID, childState, userID)
	}
	if len(cancelledChildren) > 0 {
		log.Printf("handlers: update-and-retry cancelled %d prior subtask(s) for work_id=%s", len(cancelledChildren), id)
	}

	// Persist the new description and move the parent back to intake so
	// product-agent reprocesses from scratch, with the intake event, in one
	// transaction (commitUpdateAndRetry). blocked → intake isn't in the
	// state machine directly; the state is forced and the transition
	// recorded by hand. product-agent consumes from the intake topic, so the
	// event is what actually restarts the flow.
	if err := commitUpdateAndRetry(r.Context(), h.txdb, item, body.Description, userID); err != nil {
		log.Printf("handlers: update-and-retry %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "update and retry: "+err.Error())
		return
	}
	h.dropRefusalLogged(r.Context(), id)

	writeJSON(w, http.StatusOK, map[string]string{"status": "rerunning", "work_item_id": id.String()})
}

// ─── Approvals ───────────────────────────────────────────────────────────────

// ApproveWorkItem handles POST /api/work-items/approve?id=<uuid>.
// Transitions the work item from human_review to qa and publishes qa.ready.
func (h *Handlers) ApproveWorkItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}

	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	// Determine who is approving using JWT context values.
	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "dashboard"
	}

	// The transition and qa.ready (full context, so QA tests the right
	// branch at the verified head SHA the review examined) commit together.
	if err := h.approveToQA(r.Context(), id, userID, "approved via dashboard"); err != nil {
		log.Printf("handlers: approve work item %s: %v", id, err)
		writeTransitionError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "work_item_id": id.String()})
}

// qaReadyMessage is the qa.ready payload the dashboard publishes when a
// person approves a work item out of human review. It mirrors
// cmd/status-orchestrator's qaReadyMessage field for field: the qa-agent
// reads the additive head_sha and, under EVIDENCE_COMPAT_WINDOW=strict,
// refuses a qa.ready without it (D1.5).
type qaReadyMessage struct {
	WorkID             string `json:"work_id"`
	Title              string `json:"title"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Branch             string `json:"branch,omitempty"`
	// HeadSHA is the verified head SHA the review examined, taken from
	// the review artifact alongside Branch. Empty when the review ran on a
	// legacy pr.ready without evidence.
	HeadSHA string `json:"head_sha,omitempty"`
}

// reviewedHead picks the branch and verified head SHA QA should test from
// the review artifacts (oldest first, as GetArtifacts returns them): the
// newest approved or verifier-escalated review wins, else the newest review
// that names a branch.
// The SHA always comes from the same artifact as the branch — never from a
// sibling review of a different commit — and is empty for a legacy review
// that recorded none. Malformed and branchless artifacts are skipped.
func reviewedHead(artifacts []*models.WorkItemArtifact) (branch, headSHA string) {
	for i := len(artifacts) - 1; i >= 0; i-- {
		if artifacts[i] == nil {
			continue
		}
		var parsed struct {
			Branch   string `json:"branch"`
			Decision string `json:"decision"`
			HeadSHA  string `json:"head_sha"`
		}
		if json.Unmarshal(artifacts[i].Content, &parsed) != nil || parsed.Branch == "" {
			continue
		}
		sha := strings.TrimSpace(parsed.HeadSHA)
		// "escalate" is the independent verifier's hand-off to a human
		// (M3-07): the human approves THAT head, so the newest escalation
		// is the reference even when an older cycle approved another head.
		if parsed.Decision == "approve" || parsed.Decision == "escalate" {
			return parsed.Branch, sha
		}
		if branch == "" {
			branch, headSHA = parsed.Branch, sha
		}
	}
	return branch, headSHA
}

// ─── PRD Approval (human-in-the-loop gate before research) ──────────────────
//
// product-agent transitions new work items to `prd_approval` after generating
// their PRD, instead of publishing `prd.ready` directly. These two endpoints
// let an admin gate the expensive research phase with a real read of the PRD.

// ApprovePRD handles POST /api/work-items/approve-prd?id=<uuid>.
// Transitions prd_approval → research and publishes prd.ready so
// research-coordinator dispatches. No-ops if the item isn't in prd_approval.
func (h *Handlers) ApprovePRD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		Guidance string `json:"guidance"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	item, err := h.approvals.GetWorkItem(r.Context(), id)
	if err != nil || item == nil {
		writeError(w, http.StatusNotFound, "work item not found")
		return
	}
	if item.State != models.WorkItemStatePRDApproval {
		writeError(w, http.StatusConflict, fmt.Sprintf("work item is in state %q, not prd_approval", item.State))
		return
	}

	// Pull the latest PRD artifact to rebuild the prd.ready payload.
	artifacts, _ := h.approvals.GetArtifacts(r.Context(), id, models.ArtifactTypePRD)
	if len(artifacts) == 0 {
		writeError(w, http.StatusInternalServerError, "no PRD artifact found for this work item")
		return
	}
	var prdArtifact struct {
		PRD      string `json:"prd"`
		WorkType string `json:"work_type"`
	}
	if jErr := json.Unmarshal(artifacts[len(artifacts)-1].Content, &prdArtifact); jErr != nil {
		writeError(w, http.StatusInternalServerError, "corrupted PRD artifact: "+jErr.Error())
		return
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "dashboard"
	}
	productID := ""
	if item.ProductID != nil {
		productID = item.ProductID.String()
	}
	prdMsg := map[string]string{
		"work_id":       id.String(),
		"product_id":    productID,
		"work_type":     prdArtifact.WorkType,
		"prd_content":   prdArtifact.PRD,
		"user_guidance": strings.TrimSpace(body.Guidance),
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	}
	// The transition and prd.ready commit together.
	if err := h.transitionAndEnqueue(r.Context(), id, item, models.WorkItemStateResearch, userID, "PRD approved via dashboard", bus.TopicPRDReady, id.String(), prdMsg); err != nil {
		log.Printf("handlers: approve-prd %s: %v", id, err)
		writeTransitionError(w, err)
		return
	}

	// Task 24: mark the paired interaction prompt answered so the bell /
	// sheet / approvals list stays in sync. Best-effort — the user still
	// sees the legacy approval page reflect the approval via the state
	// transition regardless of this call.
	if h.interactionsStore != nil && h.interactionsPublisher != nil {
		if promptID, err := h.interactionsStore.FindPendingApprovalPromptForWorkItem(r.Context(), id); err == nil {
			_ = h.interactionsStore.MarkAnswered(r.Context(), promptID, interactions.AnswerInput{
				AnsweredBy:   userID,
				AnsweredVia:  "dashboard",
				ActionChosen: "approve",
			})
			pID := uuid.Nil
			if item.ProductID != nil {
				pID = *item.ProductID
			}
			_ = h.interactionsPublisher.PublishAnswered(r.Context(), &interactions.AnsweredPayload{
				PromptID:     promptID,
				WorkItemID:   id,
				ProductID:    pID,
				AskingAgent:  "product-agent",
				AnsweredBy:   userID,
				AnsweredVia:  "dashboard",
				ActionChosen: "approve",
				AnsweredAt:   time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "work_item_id": id.String()})
}

// HandlePRDPromptAnswer is called by the generic AnswerPrompt handler when
// the answered prompt is a PRD approval. It performs the same state transition
// and Kafka publish that ApprovePRD/RejectPRD do, so both the legacy approval
// page and the new PromptSheet flow converge on the same pipeline side effects.
func (h *Handlers) HandlePRDPromptAnswer(ctx context.Context, workItemID uuid.UUID, action, answerText, userID string) error {
	item, err := h.approvals.GetWorkItem(ctx, workItemID)
	if err != nil || item == nil {
		return fmt.Errorf("work item not found")
	}
	if item.State != models.WorkItemStatePRDApproval && item.State != models.WorkItemStateAwaitingUser {
		return nil
	}

	if action == "approve" {
		artifacts, _ := h.approvals.GetArtifacts(ctx, workItemID, models.ArtifactTypePRD)
		if len(artifacts) == 0 {
			return fmt.Errorf("no PRD artifact found")
		}
		var prdArtifact struct {
			PRD      string `json:"prd"`
			WorkType string `json:"work_type"`
		}
		if jErr := json.Unmarshal(artifacts[len(artifacts)-1].Content, &prdArtifact); jErr != nil {
			return fmt.Errorf("corrupted PRD artifact: %w", jErr)
		}
		productID := ""
		if item.ProductID != nil {
			productID = item.ProductID.String()
		}
		prdMsg := map[string]string{
			"work_id":       workItemID.String(),
			"product_id":    productID,
			"work_type":     prdArtifact.WorkType,
			"prd_content":   prdArtifact.PRD,
			"user_guidance": strings.TrimSpace(answerText),
			"timestamp":     time.Now().UTC().Format(time.RFC3339),
		}
		// The transition out of awaiting_user / prd_approval and prd.ready
		// commit together.
		return workgraph.WithTx(ctx, h.txdb, func(tx pgx.Tx) error {
			if err := h.approvals.TransitionFromAwaitingUserToStateTx(ctx, tx, workItemID, models.WorkItemStateResearch, userID, "PRD approved via prompt sheet"); err != nil {
				return fmt.Errorf("transition failed: %w", err)
			}
			return outbox.EnqueueJSON(ctx, tx, aggregateFor(workItemID, item), bus.TopicPRDReady, workItemID.String(), prdMsg)
		})
	}

	if action == "reject" {
		reason := "PRD rejected via prompt sheet"
		if strings.TrimSpace(answerText) != "" {
			reason = "PRD rejected: " + answerText
		}
		if err := h.wg.TransitionState(ctx, workItemID, models.WorkItemStateRejected, userID, reason); err != nil {
			return fmt.Errorf("transition failed: %w", err)
		}
		return nil
	}

	return nil
}

// HandleHumanReviewPromptAnswer is called by the generic AnswerPrompt handler
// when the answered prompt is a human_review approval. Performs the same
// state transition and qa.ready publish as ApproveWorkItem.
func (h *Handlers) HandleHumanReviewPromptAnswer(ctx context.Context, workItemID uuid.UUID, action, answerText, userID string) error {
	if action == "approve" {
		// Same transition and qa.ready as ApproveWorkItem (the reviewed
		// branch and the verified head SHA the review examined), committed
		// together.
		if err := h.approveToQA(ctx, workItemID, userID, "approved via prompt sheet"); err != nil {
			var tf *transitionFailed
			if errors.As(err, &tf) {
				return fmt.Errorf("transition failed: %w", tf.err)
			}
			return fmt.Errorf("record qa.ready: %w", err)
		}
		return nil
	}

	if action == "reject" {
		reason := "rejected via prompt sheet"
		if strings.TrimSpace(answerText) != "" {
			reason = "rejected: " + answerText
		}
		if err := h.wg.TransitionState(ctx, workItemID, models.WorkItemStateRejected, userID, reason); err != nil {
			return fmt.Errorf("transition failed: %w", err)
		}
		return nil
	}

	return nil
}

// HandlePRGatePromptAnswer is called by the generic AnswerPrompt handler when
// the answered prompt is a PR gate approval. Finds the pending_pr_changes
// record and triggers the same flow as ApprovePendingChange.
func (h *Handlers) HandlePRGatePromptAnswer(ctx context.Context, workItemID uuid.UUID, action, answerText, userID string) error {
	if action != "approve" {
		if action == "reject" {
			_, err := h.wg.DB().Exec(ctx,
				`UPDATE pending_pr_changes SET status = 'rejected' WHERE work_item_id = $1 AND status = 'pending'`,
				workItemID)
			if err != nil {
				log.Printf("handlers: pr-gate prompt-answer reject update: %v", err)
			}
			reason := "PR rejected via prompt sheet"
			if strings.TrimSpace(answerText) != "" {
				reason = "PR rejected: " + answerText
			}
			_ = h.wg.TransitionState(ctx, workItemID, models.WorkItemStateHumanReview, userID, reason)
		}
		return nil
	}

	// The newest pending change for the work item, including the verified
	// head SHA pr-coordinator stored with it (handlers_pr_gate.go).
	pc, err := loadPendingPRChange(ctx, h.wg.DB(), pendingChangeByWorkItem, workItemID)
	if err != nil {
		return fmt.Errorf("no pending PR change found: %w", err)
	}
	// G1: no PR for a repository without a GitHub remote; the row stays
	// pending (see handlers_pr_gate.go).
	if err := pendingChangeRemoteCheck(ctx, h.wg.DB(), pc); err != nil {
		return err
	}

	_, err = h.wg.DB().Exec(ctx,
		`UPDATE pending_pr_changes SET status = 'approved', approved_by = $1, approved_at = NOW() WHERE id = $2`,
		userID, pc.ID)
	if err != nil {
		return fmt.Errorf("update pending change: %w", err)
	}

	githubServiceURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubServiceURL == "" {
		githubServiceURL = "http://github-service:8085"
	}
	createPRReq := struct {
		RepositoryID string `json:"repository_id"`
		Title        string `json:"title"`
		Body         string `json:"body"`
		Head         string `json:"head"`
		Base         string `json:"base"`
	}{
		RepositoryID: pc.RepositoryID,
		Title:        pc.PRTitle,
		Body:         pc.PRDescription,
		Head:         pc.Branch,
		Base:         pc.BaseBranch,
	}
	reqBody, _ := json.Marshal(createPRReq)
	resp, err := http.Post(githubServiceURL+"/pr", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("github-service call failed: %w", err)
	}
	defer resp.Body.Close()

	var prResp struct {
		Success  bool   `json:"success"`
		PRURL    string `json:"pr_url,omitempty"`
		PRNumber int    `json:"pr_number,omitempty"`
		Message  string `json:"message,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prResp); err != nil {
		return fmt.Errorf("decode github-service response: %w", err)
	}
	if !prResp.Success {
		if resp.StatusCode == http.StatusConflict {
			revertApproval(ctx, h.wg.DB(), pc.ID)
		}
		return fmt.Errorf("github-service: %s", prResp.Message)
	}

	var productID string
	_ = h.wg.DB().QueryRow(ctx,
		`SELECT COALESCE(product_id::text, '') FROM work_items WHERE id = $1`, workItemID,
	).Scan(&productID)

	// Publish pr.ready with the stored verified head SHA (D1.5) and the
	// partial-verification marker, so review-agent examines the verified
	// commit and does not block the approval under strict mode.
	// The PR exists now; pr.ready is enqueued in its own transaction (the
	// GitHub call cannot be in one). If that fails the PR has no pr.ready:
	// say so loudly with its URL and fail the answer.
	if err := commitGatedPRReady(ctx, h.txdb, pc, productID, prResp.PRNumber, prResp.PRURL, time.Now()); err != nil {
		log.Printf("handlers: ERROR pr-gate prompt-answer: PR %s (#%d) was created for work_item=%s but pr.ready was NOT recorded: %v", prResp.PRURL, prResp.PRNumber, pc.WorkItemID, err)
		return fmt.Errorf("PR %s created but pr.ready not recorded: %w", prResp.PRURL, err)
	}

	return nil
}

// subtaskRetryMessage is the implementation.tasks payload the dashboard
// publishes when a person retries a blocked or failed subtask. It carries the
// subset of the implementation-planner's implementationTaskMessage that
// cmd/implementation-agent reads, under the same JSON keys, so the agent
// decodes a retry exactly as it decodes a planner task: work_id is the parent
// work item (the agent's work graph and evidence are keyed on it), task_id is
// the subtask, and product_id / repository_id select the clone and
// credentials. files_to_change is always present (the agent ranges over it)
// but empty: the planner's list is not persisted on the work item, so a retry
// falls back to the agent's own codex discovery. retry marks the message for
// log readers; the agent does not read it.
type subtaskRetryMessage struct {
	TaskID        string   `json:"task_id"`
	WorkID        string   `json:"work_id"`
	ProductID     string   `json:"product_id"`
	RepositoryID  string   `json:"repository_id,omitempty"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	FilesToChange []string `json:"files_to_change"`
	Retry         bool     `json:"retry"`
}

// buildSubtaskRetryMessage builds the retry payload for a subtask work item.
// work_id is the parent when the subtask has one and the subtask's own id
// otherwise, matching the planner's work_id for that subtask.
func buildSubtaskRetryMessage(item *models.WorkItem) subtaskRetryMessage {
	msg := subtaskRetryMessage{
		TaskID:        item.ID.String(),
		WorkID:        item.ID.String(),
		Title:         item.Title,
		Description:   item.Description,
		FilesToChange: []string{},
		Retry:         true,
	}
	if item.ParentID != nil {
		msg.WorkID = item.ParentID.String()
	}
	if item.ProductID != nil {
		msg.ProductID = item.ProductID.String()
	}
	if item.RepositoryID != nil {
		msg.RepositoryID = item.RepositoryID.String()
	}
	return msg
}

// HandleBlockedPromptAnswer is called by the generic AnswerPrompt handler when
// the answered prompt is for a blocked item. "retry" restarts the pipeline;
// "cancel" transitions to cancelled. For an item a stage refused on an
// empty codex (codex coherence Part 3, codex_refusal.go), "reindex"
// requests a full index and leaves the re-run to status-orchestrator,
// "retry" re-runs the refused stage from its recorded message, and
// "cancel" also drops the refusal.
func (h *Handlers) HandleBlockedPromptAnswer(ctx context.Context, workItemID uuid.UUID, action, answerText, userID string) error {
	item, err := h.approvals.GetWorkItem(ctx, workItemID)
	if err != nil || item == nil {
		return fmt.Errorf("work item not found")
	}

	if action == guards.ActionReindex {
		return h.reindexForRefusal(ctx, item, userID)
	}

	if action == "retry" {
		if replayed, err := h.retryRefusal(ctx, item, userID); err != nil || replayed {
			return err
		}
		if item.Type == models.WorkItemTypeSubtask {
			// The transition and implementation.tasks commit together.
			if err := h.retrySubtask(ctx, item, userID, "retry via prompt sheet"); err != nil {
				var tf *transitionFailed
				if errors.As(err, &tf) {
					return fmt.Errorf("transition failed: %w", tf.err)
				}
				return fmt.Errorf("record implementation.tasks retry: %w", err)
			}
			return nil
		}
		// Research only starts on prd.ready (research-coordinator), so the
		// move to research and the prd.ready rebuilt from the newest PRD
		// artifact commit together. Without a PRD research-coordinator would
		// block the item again, so nothing changes and the answer fails.
		payload, err := blockedRetryPRDReady(ctx, h.approvals, item, answerText)
		if err != nil {
			return err
		}
		if err := h.transitionAndEnqueue(ctx, workItemID, item, models.WorkItemStateResearch, userID, "retry via prompt sheet", bus.TopicPRDReady, workItemID.String(), payload); err != nil {
			var tf *transitionFailed
			if errors.As(err, &tf) {
				return fmt.Errorf("transition failed: %w", tf.err)
			}
			return fmt.Errorf("record prd.ready retry: %w", err)
		}
		return nil
	}

	if action == "cancel" {
		reason := "cancelled via prompt sheet"
		if strings.TrimSpace(answerText) != "" {
			reason = "cancelled: " + answerText
		}
		if err := h.wg.TransitionState(ctx, workItemID, models.WorkItemStateCancelled, userID, reason); err != nil {
			return fmt.Errorf("transition failed: %w", err)
		}
		return h.dropRefusal(ctx, item)
	}

	return nil
}

// RejectPRD handles POST /api/work-items/reject-prd?id=<uuid>.
// Body (optional): {"reason":"..."}. Transitions prd_approval → rejected.
func (h *Handlers) RejectPRD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // reason is optional

	reason := "PRD rejected via dashboard"
	if strings.TrimSpace(body.Reason) != "" {
		reason = "PRD rejected: " + body.Reason
	}

	userID, _ := r.Context().Value(contextKeyUserID).(string)
	if userID == "" {
		userID = "dashboard"
	}

	// Fetch the item for ProductID in the prompt-answered event.
	item, err := h.wg.GetWorkItem(r.Context(), id)
	if err != nil || item == nil {
		// Not fatal — we'll still reject, but the interaction prompt won't sync.
		log.Printf("handlers: reject-prd get work item: %v", err)
		item = nil
	}

	if err := h.wg.TransitionState(r.Context(), id, models.WorkItemStateRejected, userID, reason); err != nil {
		log.Printf("handlers: reject-prd transition %s: %v", id, err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Task 24: mark the paired interaction prompt answered (with action=reject).
	if h.interactionsStore != nil && h.interactionsPublisher != nil {
		if promptID, err := h.interactionsStore.FindPendingApprovalPromptForWorkItem(r.Context(), id); err == nil {
			_ = h.interactionsStore.MarkAnswered(r.Context(), promptID, interactions.AnswerInput{
				AnsweredBy:   userID,
				AnsweredVia:  "dashboard",
				ActionChosen: "reject",
			})
			pID := uuid.Nil
			if item != nil && item.ProductID != nil {
				pID = *item.ProductID
			}
			_ = h.interactionsPublisher.PublishAnswered(r.Context(), &interactions.AnsweredPayload{
				PromptID:     promptID,
				WorkItemID:   id,
				ProductID:    pID,
				AskingAgent:  "product-agent",
				AnsweredBy:   userID,
				AnsweredVia:  "dashboard",
				ActionChosen: "reject",
				AnsweredAt:   time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected", "work_item_id": id.String()})
}

// ─── Metrics ─────────────────────────────────────────────────────────────────

// metricsSummaryResponse is the response body for MetricsSummary.
type metricsSummaryResponse struct {
	TotalWorkItems      int            `json:"total_work_items"`
	ByState             map[string]int `json:"by_state"`
	CompletedLast7Days  int            `json:"completed_last_7_days"`
	AvgCycleTimeHours   float64        `json:"avg_cycle_time_hours"`
	QAPassRate          float64        `json:"qa_pass_rate"`
	TotalInferenceCalls int            `json:"total_inference_calls"`
	TotalTokensUsed     int            `json:"total_tokens_used"`
	ActiveAgents        int            `json:"active_agents"`
	ByProvider          []modelUsage   `json:"by_provider"`
	ByModel             []modelUsage   `json:"by_model"`
}

type modelUsage struct {
	Name         string `json:"name"`
	Calls        int    `json:"calls"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
}

// by_provider / by_model group on the provider and model normalised by
// inference_log_costs (migration 053), so rows written before M4-05
// ("anthropic/claude-x" in model_provider, the same or "" in model_id) and
// after it (bare "anthropic" / "claude-x") count as one provider and model.
const (
	metricsByProviderSQL = `
		SELECT COALESCE(NULLIF(provider, ''), 'unknown') AS name,
		       COUNT(*),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0)
		FROM inference_log_costs
		GROUP BY 1
		ORDER BY COUNT(*) DESC, 1`
	metricsByModelSQL = `
		SELECT COALESCE(NULLIF(model_id, ''), 'unknown') AS name,
		       COUNT(*),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0)
		FROM inference_log_costs
		GROUP BY 1
		ORDER BY COUNT(*) DESC, 1`
)

// MetricsSummary handles GET /api/metrics/summary.
// Returns aggregated operational metrics derived from the database.
func (h *Handlers) MetricsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ctx := r.Context()
	db := h.wg.DB()

	resp := metricsSummaryResponse{
		ByState: make(map[string]int),
	}

	// Total work items
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&resp.TotalWorkItems); err != nil {
		log.Printf("handlers: metrics total_work_items: %v", err)
	}

	// By state
	rows, err := db.Query(ctx, `SELECT state, COUNT(*) FROM work_items GROUP BY state`)
	if err != nil {
		log.Printf("handlers: metrics by_state: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var state string
			var count int
			if err := rows.Scan(&state, &count); err == nil {
				resp.ByState[state] = count
			}
		}
	}

	// Completed last 7 days
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM work_items WHERE state = 'passed' AND updated_at > NOW() - INTERVAL '7 days'`,
	).Scan(&resp.CompletedLast7Days); err != nil {
		log.Printf("handlers: metrics completed_last_7_days: %v", err)
	}

	// Average cycle time (intake → passed), in hours
	var avgCycle *float64
	if err := db.QueryRow(ctx,
		`SELECT AVG(EXTRACT(EPOCH FROM (t2.timestamp - t1.timestamp))/3600)
		 FROM work_item_transitions t1
		 JOIN work_item_transitions t2 ON t1.work_item_id = t2.work_item_id
		 WHERE t1.to_state = 'intake' AND t2.to_state = 'passed'`,
	).Scan(&avgCycle); err != nil {
		log.Printf("handlers: metrics avg_cycle_time: %v", err)
	}
	if avgCycle != nil {
		resp.AvgCycleTimeHours = *avgCycle
	}

	// QA pass rate
	passed := resp.ByState["passed"]
	failed := resp.ByState["failed"]
	if passed+failed > 0 {
		resp.QAPassRate = float64(passed) / float64(passed+failed)
	}

	// Inference calls
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM inference_logs`).Scan(&resp.TotalInferenceCalls); err != nil {
		log.Printf("handlers: metrics inference_calls: %v", err)
	}

	// Total tokens
	if err := db.QueryRow(ctx, `SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM inference_logs`).Scan(&resp.TotalTokensUsed); err != nil {
		log.Printf("handlers: metrics total_tokens: %v", err)
	}

	// Active agents
	if err := db.QueryRow(ctx,
		`SELECT COUNT(DISTINCT agent_instance_id) FROM agent_assignments WHERE status = 'active'`,
	).Scan(&resp.ActiveAgents); err != nil {
		log.Printf("handlers: metrics active_agents: %v", err)
	}

	// By provider
	provRows, err := db.Query(ctx, metricsByProviderSQL)
	if err != nil {
		log.Printf("handlers: metrics by_provider: %v", err)
	} else {
		defer provRows.Close()
		for provRows.Next() {
			var u modelUsage
			if err := provRows.Scan(&u.Name, &u.Calls, &u.InputTokens, &u.OutputTokens); err == nil {
				u.TotalTokens = u.InputTokens + u.OutputTokens
				resp.ByProvider = append(resp.ByProvider, u)
			}
		}
	}
	if resp.ByProvider == nil {
		resp.ByProvider = []modelUsage{}
	}

	// By model
	modelRows, err := db.Query(ctx, metricsByModelSQL)
	if err != nil {
		log.Printf("handlers: metrics by_model: %v", err)
	} else {
		defer modelRows.Close()
		for modelRows.Next() {
			var u modelUsage
			if err := modelRows.Scan(&u.Name, &u.Calls, &u.InputTokens, &u.OutputTokens); err == nil {
				u.TotalTokens = u.InputTokens + u.OutputTokens
				resp.ByModel = append(resp.ByModel, u)
			}
		}
	}
	if resp.ByModel == nil {
		resp.ByModel = []modelUsage{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── Agents ───────────────────────────────────────────────────────────────────

// agentStatusEntry is one element of the GET /api/agents/status response.
type agentStatusEntry struct {
	AgentType     string  `json:"agent_type"`
	InstanceID    string  `json:"instance_id"`
	Status        string  `json:"status"`
	CurrentTask   *string `json:"current_task"`
	WorkItemID    *string `json:"work_item_id,omitempty"`
	WorkItemTitle *string `json:"work_item_title,omitempty"`
	StartedAt     *string `json:"started_at,omitempty"`
}

// AgentStatus handles GET /api/agents/status.
// Returns the current status of every known agent type.  Active agents are
// sourced from the agent_assignments table (joined with work_items for the
// title); any type not found there is reported as idle.
func (h *Handlers) AgentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	rows, err := h.wg.DB().Query(r.Context(),
		`SELECT a.agent_type, a.agent_instance_id, a.status, a.work_item_id,
		        COALESCE(wi.title, ''), a.assigned_at
		 FROM agent_assignments a
		 LEFT JOIN work_items wi ON wi.id = a.work_item_id
		 WHERE a.status = 'active'
		 ORDER BY a.agent_type, a.assigned_at DESC`)
	if err != nil {
		log.Printf("handlers: agent status query: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	defer rows.Close()

	// Collect active assignments keyed by agent_type.
	type activeRow struct {
		instanceID    string
		workItemID    string
		workItemTitle string
		assignedAt    time.Time
	}
	active := make(map[string]activeRow)
	for rows.Next() {
		var agentType, instanceID, status, workItemID, wiTitle string
		var assignedAt time.Time
		if err := rows.Scan(&agentType, &instanceID, &status, &workItemID, &wiTitle, &assignedAt); err != nil {
			log.Printf("handlers: agent status scan: %v", err)
			continue
		}
		// Keep only the first (most recently assigned) row per type.
		if _, seen := active[agentType]; !seen {
			active[agentType] = activeRow{instanceID: instanceID, workItemID: workItemID, workItemTitle: wiTitle, assignedAt: assignedAt}
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("handlers: agent status rows error: %v", err)
	}

	result := make([]agentStatusEntry, 0, len(knownAgentTypes))
	for _, agentType := range knownAgentTypes {
		entry := agentStatusEntry{
			AgentType:  agentType,
			InstanceID: agentType + "-agent-1",
			Status:     "idle",
		}
		if row, ok := active[agentType]; ok {
			entry.InstanceID = row.instanceID
			entry.Status = "active"
			entry.CurrentTask = &row.workItemID
			entry.WorkItemID = &row.workItemID
			if row.workItemTitle != "" {
				entry.WorkItemTitle = &row.workItemTitle
			}
			startedStr := row.assignedAt.UTC().Format(time.RFC3339)
			entry.StartedAt = &startedStr
		}
		result = append(result, entry)
	}

	writeJSON(w, http.StatusOK, result)
}

// ─── Token Cost ───────────────────────────────────────────────────────────────

// WorkItemCost handles GET /api/work-items/cost?id={work_item_id}.
// Returns aggregated token usage for a specific work item from inference_logs.
func (h *Handlers) WorkItemCost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "id parameter is required")
		return
	}

	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id format")
		return
	}

	// M4-05: the item and its subtasks (every agent now tags its calls with
	// the work item it serves), priced through inference_log_costs. A read
	// failure is a 500, never a silent zero.
	var totalInput, totalOutput, totalCalls, tokenCalls, unpriced int64
	var cost *float64
	if err := h.wg.DB().QueryRow(r.Context(),
		`SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COUNT(*),
		        COUNT(*) FILTER (WHERE input_tokens + output_tokens > 0),
		        COUNT(*) FILTER (WHERE input_tokens + output_tokens > 0 AND cost_usd IS NULL),
		        SUM(cost_usd)::float8
		 FROM inference_log_costs
		 WHERE work_item_id = $1 OR work_item_id IN (SELECT id FROM work_items WHERE parent_id = $1)`, id,
	).Scan(&totalInput, &totalOutput, &totalCalls, &tokenCalls, &unpriced, &cost); err != nil {
		log.Printf("handlers: work item cost for %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to read inference cost")
		return
	}

	writeJSON(w, http.StatusOK, costSummary(id, totalInput, totalOutput, totalCalls, tokenCalls, unpriced, cost))
}

// WorkItemDiff handles GET /api/work-items/diff?id={work_item_id}.
// Returns the actual git diff between the agent branch and the base branch for
// the repository associated with the work item.
func (h *Handlers) WorkItemDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	idStr := r.URL.Query().Get("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}

	// Look up the work item to get repository_id
	item, err := h.wg.GetWorkItem(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	if item.RepositoryID == nil {
		writeError(w, http.StatusBadRequest, "no repository associated")
		return
	}

	// Look up clone_path and base_branch
	var clonePath, baseBranch string
	h.wg.DB().QueryRow(r.Context(),
		"SELECT clone_path, base_branch FROM repositories WHERE id = $1", item.RepositoryID,
	).Scan(&clonePath, &baseBranch) //nolint:errcheck

	if clonePath == "" {
		writeError(w, http.StatusBadRequest, "repository not cloned")
		return
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	// Find the agent branch for this work item.
	// Pattern: agent/{workID}/{taskID[:8]}
	branchCmd := exec.CommandContext(r.Context(), "git", "branch", "-r")
	branchCmd.Dir = clonePath
	branchOutput, err := branchCmd.CombinedOutput()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "git branch failed")
		return
	}

	var agentBranch string
	prefix := fmt.Sprintf("agent/%s/", id.String())
	for _, line := range strings.Split(string(branchOutput), "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "origin/")
		if strings.HasPrefix(trimmed, prefix) {
			agentBranch = "origin/" + trimmed
			break
		}
	}

	if agentBranch == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"diff":    "",
			"stats":   "",
			"message": "No agent branch found for this work item",
		})
		return
	}

	// Get diff stats
	statsCmd := exec.CommandContext(r.Context(), "git", "diff", "--stat", baseBranch+".."+agentBranch)
	statsCmd.Dir = clonePath
	statsOutput, _ := statsCmd.CombinedOutput()

	// Get full diff
	diffCmd := exec.CommandContext(r.Context(), "git", "diff", baseBranch+".."+agentBranch)
	diffCmd.Dir = clonePath
	diffOutput, _ := diffCmd.CombinedOutput()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"diff":   string(diffOutput),
		"stats":  string(statsOutput),
		"branch": agentBranch,
		"base":   baseBranch,
	})
}

// ---------------------------------------------------------------------------
// Chat session handlers (session-based API)
// ---------------------------------------------------------------------------

// HandleCreateChatSession creates a new chat session.
// POST /api/chat/sessions
func (h *Handlers) HandleCreateChatSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PageContext json.RawMessage `json:"page_context"`
		WorkItemID  *string         `json:"work_item_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	userID := r.Header.Get("X-User-Email")
	if userID == "" {
		userID = "admin"
	}

	id := uuid.New()
	now := time.Now().UTC()

	if body.PageContext == nil {
		body.PageContext = json.RawMessage(`{}`)
	}

	var workItemID *uuid.UUID
	if body.WorkItemID != nil && *body.WorkItemID != "" {
		parsed, err := uuid.Parse(*body.WorkItemID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid work_item_id")
			return
		}
		workItemID = &parsed
	}

	_, err := h.wg.DB().Exec(r.Context(),
		`INSERT INTO chat_sessions (id, user_id, page_context, active_work_item_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		id, userID, body.PageContext, workItemID, now,
	)
	if err != nil {
		log.Printf("handlers: create chat session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":           id.String(),
		"page_context": body.PageContext,
		"created_at":   now.Format(time.RFC3339),
	})
}

// HandleListChatSessions lists chat sessions for the current user.
// GET /api/chat/sessions?limit=&offset=
func (h *Handlers) HandleListChatSessions(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-Email")
	if userID == "" {
		userID = "admin"
	}

	limit := 20
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, err := h.wg.DB().Query(r.Context(),
		`SELECT s.id, s.title, s.page_context, s.active_work_item_id, s.product_id,
		        p.name AS product_name, s.created_at, s.updated_at,
		        (SELECT content FROM chat_messages WHERE session_id = s.id ORDER BY created_at DESC LIMIT 1) AS last_message
		 FROM chat_sessions s
		 LEFT JOIN products p ON p.id = s.product_id
		 WHERE s.user_id = $1
		   AND s.archived_at IS NULL
		   AND EXISTS (SELECT 1 FROM chat_messages WHERE session_id = s.id)
		 ORDER BY s.updated_at DESC
		 LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		log.Printf("handlers: list chat sessions: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list sessions")
		return
	}
	defer rows.Close()

	type session struct {
		ID               string          `json:"id"`
		Title            *string         `json:"title"`
		PageContext      json.RawMessage `json:"page_context"`
		ActiveWorkItemID *string         `json:"active_work_item_id"`
		ProductID        *string         `json:"product_id,omitempty"`
		ProductName      *string         `json:"product_name,omitempty"`
		CreatedAt        string          `json:"created_at"`
		UpdatedAt        string          `json:"updated_at"`
		LastMessage      *string         `json:"last_message"`
	}

	var sessions []session
	var sessionIDs []uuid.UUID

	for rows.Next() {
		var s session
		var sid uuid.UUID
		var createdAt, updatedAt time.Time
		var workItemID *uuid.UUID
		var productID *uuid.UUID
		var prodName *string
		var lastMsg *string
		if err := rows.Scan(&sid, &s.Title, &s.PageContext, &workItemID, &productID, &prodName, &createdAt, &updatedAt, &lastMsg); err != nil {
			log.Printf("handlers: scan chat session: %v", err)
			continue
		}
		s.ID = sid.String()
		s.CreatedAt = createdAt.Format(time.RFC3339)
		s.UpdatedAt = updatedAt.Format(time.RFC3339)
		s.LastMessage = lastMsg
		if workItemID != nil {
			wid := workItemID.String()
			s.ActiveWorkItemID = &wid
		}
		if productID != nil {
			pid := productID.String()
			s.ProductID = &pid
			s.ProductName = prodName
		}
		sessions = append(sessions, s)
		sessionIDs = append(sessionIDs, sid)
	}

	// Batch-load linked work items for all sessions.
	linkedItems := map[string][]map[string]string{}
	if len(sessionIDs) > 0 {
		wiRows, err := h.wg.DB().Query(r.Context(),
			`SELECT sw.session_id, sw.work_item_id, COALESCE(wi.title, '') AS title
			 FROM chat_session_work_items sw
			 LEFT JOIN work_items wi ON wi.id = sw.work_item_id
			 WHERE sw.session_id = ANY($1)`,
			sessionIDs,
		)
		if err == nil {
			defer wiRows.Close()
			for wiRows.Next() {
				var sessID, wiID uuid.UUID
				var title string
				if err := wiRows.Scan(&sessID, &wiID, &title); err != nil {
					continue
				}
				linkedItems[sessID.String()] = append(linkedItems[sessID.String()], map[string]string{
					"work_item_id": wiID.String(),
					"title":        title,
				})
			}
		}
	}

	for i := range sessions {
		if items, ok := linkedItems[sessions[i].ID]; ok {
			_ = items // attached below via JSON
		}
	}

	type sessionOut struct {
		session
		LinkedWorkItems []map[string]string `json:"linked_work_items"`
	}
	out := make([]sessionOut, len(sessions))
	for i, s := range sessions {
		out[i].session = s
		out[i].LinkedWorkItems = linkedItems[s.ID]
		if out[i].LinkedWorkItems == nil {
			out[i].LinkedWorkItems = []map[string]string{}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": out,
	})
}

// HandleGetChatSession returns a single chat session with linked work items.
// GET /api/chat/sessions/{id}
func (h *Handlers) HandleGetChatSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	var title *string
	var pageCtx json.RawMessage
	var activeWI *uuid.UUID
	var productID *uuid.UUID
	var providerID *uuid.UUID
	var createdAt, updatedAt time.Time

	err = h.wg.DB().QueryRow(r.Context(),
		`SELECT title, page_context, active_work_item_id, product_id, llm_provider_id, created_at, updated_at
		 FROM chat_sessions WHERE id = $1`,
		sessionID,
	).Scan(&title, &pageCtx, &activeWI, &productID, &providerID, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		log.Printf("handlers: get chat session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to get session")
		return
	}

	// Linked work items.
	wiRows, err := h.wg.DB().Query(r.Context(),
		`SELECT sw.work_item_id, COALESCE(wi.title, '') AS title
		 FROM chat_session_work_items sw
		 LEFT JOIN work_items wi ON wi.id = sw.work_item_id
		 WHERE sw.session_id = $1`,
		sessionID,
	)
	var linked []map[string]string
	if err == nil {
		defer wiRows.Close()
		for wiRows.Next() {
			var wiID uuid.UUID
			var wiTitle string
			if err := wiRows.Scan(&wiID, &wiTitle); err != nil {
				continue
			}
			linked = append(linked, map[string]string{
				"work_item_id": wiID.String(),
				"title":        wiTitle,
			})
		}
	}
	if linked == nil {
		linked = []map[string]string{}
	}

	resp := map[string]interface{}{
		"id":                sessionID.String(),
		"title":             title,
		"page_context":      pageCtx,
		"created_at":        createdAt.Format(time.RFC3339),
		"updated_at":        updatedAt.Format(time.RFC3339),
		"linked_work_items": linked,
	}
	if activeWI != nil {
		resp["active_work_item_id"] = activeWI.String()
	}
	if productID != nil {
		resp["product_id"] = productID.String()
		var prodName string
		if err := h.wg.DB().QueryRow(r.Context(), `SELECT name FROM products WHERE id = $1`, *productID).Scan(&prodName); err == nil {
			resp["product_name"] = prodName
		}
	}
	if providerID != nil {
		resp["provider_id"] = providerID.String()
		var provName, provType string
		var provModel *string
		if err := h.wg.DB().QueryRow(r.Context(),
			`SELECT name, provider_type, settings->>'model' FROM llm_providers WHERE id = $1`,
			*providerID).Scan(&provName, &provType, &provModel); err == nil {
			resp["provider_name"] = provName
			resp["provider_type"] = provType
			if provModel != nil {
				resp["provider_model"] = *provModel
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleDeleteChatSession deletes a chat session and its related data.
// DELETE /api/chat/sessions/{id}
func (h *Handlers) HandleDeleteChatSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	ctx := r.Context()
	tx, err := h.wg.DB().Begin(ctx)
	if err != nil {
		log.Printf("handlers: begin tx for delete session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete session")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM chat_messages WHERE session_id = $1`, sessionID); err != nil {
		log.Printf("handlers: delete chat messages: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete session")
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM chat_session_work_items WHERE session_id = $1`, sessionID); err != nil {
		log.Printf("handlers: delete session work items: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete session")
		return
	}
	tag, err := tx.Exec(ctx, `DELETE FROM chat_sessions WHERE id = $1`, sessionID)
	if err != nil {
		log.Printf("handlers: delete chat session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete session")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("handlers: commit delete session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleArchiveChatSession soft-deletes a session by setting archived_at.
// POST /api/chat/sessions/{id}/archive
func (h *Handlers) HandleArchiveChatSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE chat_sessions SET archived_at = NOW() WHERE id = $1 AND archived_at IS NULL`, sessionID)
	if err != nil {
		log.Printf("handlers: archive chat session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to archive session")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "session not found or already archived")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleUnarchiveChatSession restores an archived session.
// POST /api/chat/sessions/{id}/unarchive
func (h *Handlers) HandleUnarchiveChatSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE chat_sessions SET archived_at = NULL WHERE id = $1 AND archived_at IS NOT NULL`, sessionID)
	if err != nil {
		log.Printf("handlers: unarchive chat session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to unarchive session")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "session not found or not archived")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleUpdateChatSession updates a chat session's title.
// PATCH /api/chat/sessions/{id}
func (h *Handlers) HandleUpdateChatSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE chat_sessions SET title = $1, updated_at = now() WHERE id = $2`,
		body.Title, sessionID,
	)
	if err != nil {
		log.Printf("handlers: update chat session: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update session")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":    sessionID.String(),
		"title": body.Title,
	})
}

// HandleSetChatSessionProduct sets the active product for a chat session.
// PUT /api/chat/sessions/{id}/product
func (h *Handlers) HandleSetChatSessionProduct(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	var body struct {
		ProductID string `json:"product_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	productID, err := uuid.Parse(body.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id")
		return
	}

	var prodName string
	if err := h.wg.DB().QueryRow(r.Context(), `SELECT name FROM products WHERE id = $1`, productID).Scan(&prodName); err != nil {
		writeError(w, http.StatusNotFound, "product not found")
		return
	}

	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE chat_sessions SET product_id = $1, updated_at = now() WHERE id = $2`,
		productID, sessionID,
	)
	if err != nil {
		log.Printf("handlers: set chat session product: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to set product")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":           sessionID.String(),
		"product_id":   productID.String(),
		"product_name": prodName,
	})
}

// HandleListProducts returns all products (id, name, description) for any
// authenticated user. Used by the chat product selector.
// GET /api/products
func (h *Handlers) HandleListProducts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.wg.DB().Query(r.Context(),
		`SELECT id, name, COALESCE(description, '') FROM products ORDER BY name`)
	if err != nil {
		log.Printf("handlers: list products: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list products")
		return
	}
	defer rows.Close()

	type product struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var products []product
	for rows.Next() {
		var p product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description); err != nil {
			continue
		}
		products = append(products, p)
	}
	if products == nil {
		products = []product{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"products": products})
}

// HandleListLLMProviders returns enabled LLM providers for the chat provider selector.
// GET /api/llm-providers
func (h *Handlers) HandleListLLMProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.wg.DB().Query(r.Context(),
		`SELECT id, name, provider_type, settings->>'model' AS model, is_default
		 FROM llm_providers WHERE enabled = true ORDER BY is_default DESC, name`)
	if err != nil {
		log.Printf("handlers: list llm providers: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}
	defer rows.Close()

	type provider struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		ProviderType string `json:"provider_type"`
		Model        string `json:"model"`
		IsDefault    bool   `json:"is_default"`
	}
	var providers []provider
	for rows.Next() {
		var p provider
		var model *string
		if err := rows.Scan(&p.ID, &p.Name, &p.ProviderType, &model, &p.IsDefault); err != nil {
			continue
		}
		if model != nil {
			p.Model = *model
		}
		providers = append(providers, p)
	}
	if providers == nil {
		providers = []provider{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"providers": providers})
}

// HandleSetChatSessionProvider sets the LLM provider for a chat session.
// PUT /api/chat/sessions/{id}/provider
func (h *Handlers) HandleSetChatSessionProvider(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	var body struct {
		ProviderID string `json:"provider_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	providerID, err := uuid.Parse(body.ProviderID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid provider_id")
		return
	}

	// Verify provider exists and is enabled.
	var provName, provType string
	var provModel *string
	err = h.wg.DB().QueryRow(r.Context(),
		`SELECT name, provider_type, settings->>'model' FROM llm_providers WHERE id = $1 AND enabled = true`,
		providerID).Scan(&provName, &provType, &provModel)
	if err != nil {
		writeError(w, http.StatusNotFound, "provider not found or disabled")
		return
	}

	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE chat_sessions SET llm_provider_id = $1, updated_at = now() WHERE id = $2`,
		providerID, sessionID,
	)
	if err != nil {
		log.Printf("handlers: set chat session provider: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to set provider")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	model := ""
	if provModel != nil {
		model = *provModel
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":            sessionID.String(),
		"provider_id":   providerID.String(),
		"provider_name": provName,
		"provider_type": provType,
		"model":         model,
	})
}

// HandleSendChatMessage sends a user message to a chat session.
// POST /api/chat/sessions/{id}/messages
func (h *Handlers) HandleSendChatMessage(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	var body struct {
		Content     string          `json:"content"`
		PageContext json.RawMessage `json:"page_context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	userID := r.Header.Get("X-User-Email")
	if userID == "" {
		userID = "admin"
	}

	ctx := r.Context()

	// Look up the session's active work item.
	var activeWI *uuid.UUID
	var title *string
	err = h.wg.DB().QueryRow(ctx,
		`SELECT active_work_item_id, title FROM chat_sessions WHERE id = $1`, sessionID,
	).Scan(&activeWI, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		log.Printf("handlers: lookup session for message: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to send message")
		return
	}

	// Auto-generate title from first message if title is null.
	if title == nil {
		autoTitle := body.Content
		if len(autoTitle) > 60 {
			autoTitle = autoTitle[:60]
		}
		_, _ = h.wg.DB().Exec(ctx,
			`UPDATE chat_sessions SET title = $1, updated_at = now() WHERE id = $2 AND title IS NULL`,
			autoTitle, sessionID,
		)
	} else {
		// Touch updated_at.
		_, _ = h.wg.DB().Exec(ctx,
			`UPDATE chat_sessions SET updated_at = now() WHERE id = $1`, sessionID,
		)
	}

	// Persist user message immediately so it survives page reloads.
	msgID := uuid.New()
	if _, dbErr := h.wg.DB().Exec(ctx,
		`INSERT INTO chat_messages (id, session_id, work_item_id, role, content, created_at)
		 VALUES ($1, $2, $3, 'user', $4, NOW())`,
		msgID, sessionID, activeWI, body.Content,
	); dbErr != nil {
		log.Printf("handlers: insert user chat message: %v", dbErr)
		writeError(w, http.StatusInternalServerError, "failed to save message")
		return
	}

	// Build Kafka payload.
	payload := map[string]interface{}{
		"session_id":   sessionID.String(),
		"user_id":      userID,
		"message_id":   msgID.String(),
		"content":      body.Content,
		"page_context": body.PageContext,
	}
	if activeWI != nil {
		payload["work_item_id"] = activeWI.String()
	}
	msgPayload, _ := json.Marshal(payload)

	if err := h.msgBus.Publish(ctx, bus.TopicChatMessage, sessionID.String(), msgPayload); err != nil {
		log.Printf("handlers: publish chat.message: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to send message")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message_id": msgID.String(),
		"session_id": sessionID.String(),
		"status":     "sent",
	})
}

// HandleGetChatMessages returns messages for a chat session.
// GET /api/chat/sessions/{id}/messages?limit=50
func (h *Handlers) HandleGetChatMessages(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	rows, err := h.wg.DB().Query(r.Context(),
		`SELECT id, role, content, tool_calls, proposal, created_at
		 FROM chat_messages
		 WHERE session_id = $1
		 ORDER BY created_at ASC
		 LIMIT $2`,
		sessionID, limit,
	)
	if err != nil {
		log.Printf("handlers: query chat messages: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load messages")
		return
	}
	defer rows.Close()

	type chatMsg struct {
		ID        string          `json:"id"`
		Role      string          `json:"role"`
		Content   string          `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls"`
		Proposal  json.RawMessage `json:"proposal"`
		CreatedAt string          `json:"created_at"`
	}
	var messages []chatMsg

	for rows.Next() {
		var m chatMsg
		var createdAt time.Time
		var toolCalls, proposal []byte
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &toolCalls, &proposal, &createdAt); err != nil {
			log.Printf("handlers: scan chat message: %v", err)
			continue
		}
		m.CreatedAt = createdAt.Format(time.RFC3339)
		if toolCalls != nil {
			m.ToolCalls = json.RawMessage(toolCalls)
		}
		if proposal != nil {
			m.Proposal = json.RawMessage(proposal)
		}
		messages = append(messages, m)
	}

	if messages == nil {
		messages = []chatMsg{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sessionID.String(),
		"messages":   messages,
	})
}

// HandleApproveProposal marks a proposal as approved.
// POST /api/chat/sessions/{id}/proposals/{msgId}/approve
func (h *Handlers) HandleApproveProposal(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	msgID, err := uuid.Parse(r.PathValue("msgId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	ctx := r.Context()
	tag, err := h.wg.DB().Exec(ctx,
		`UPDATE chat_messages
		 SET proposal = jsonb_set(COALESCE(proposal, '{}'), '{status}', '"approved"')
		 WHERE id = $1 AND session_id = $2`,
		msgID, sessionID,
	)
	if err != nil {
		log.Printf("handlers: approve proposal: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to approve proposal")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}

	// Publish resolution event.
	payload, _ := json.Marshal(map[string]string{
		"session_id": sessionID.String(),
		"message_id": msgID.String(),
		"resolution": "approved",
	})
	if err := h.msgBus.Publish(ctx, bus.TopicChatProposalResolved, sessionID.String(), payload); err != nil {
		log.Printf("handlers: publish proposal resolved: %v", err)
		// Non-fatal: the DB update already succeeded.
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// HandleRejectProposal marks a proposal as rejected.
// POST /api/chat/sessions/{id}/proposals/{msgId}/reject
func (h *Handlers) HandleRejectProposal(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	msgID, err := uuid.Parse(r.PathValue("msgId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE chat_messages
		 SET proposal = jsonb_set(COALESCE(proposal, '{}'), '{status}', '"rejected"')
		 WHERE id = $1 AND session_id = $2`,
		msgID, sessionID,
	)
	if err != nil {
		log.Printf("handlers: reject proposal: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to reject proposal")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

func (h *Handlers) ListToolEvents(w http.ResponseWriter, r *http.Request) {
	workItemID := r.URL.Query().Get("work_item_id")
	if workItemID == "" {
		writeError(w, http.StatusBadRequest, "work_item_id required")
		return
	}
	wid, err := uuid.Parse(workItemID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid work_item_id")
		return
	}

	rows, err := h.wg.DB().Query(r.Context(),
		`SELECT work_item_id, task_id, agent_type, tool, arguments, result, is_error, created_at,
		        COALESCE(phase, ''), exit_code
		 FROM tool_events
		 WHERE work_item_id = $1
		 ORDER BY created_at ASC
		 LIMIT 2000`, wid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type toolEvent struct {
		WorkItemID string `json:"work_item_id"`
		TaskID     string `json:"task_id"`
		AgentType  string `json:"agent_type"`
		Tool       string `json:"tool"`
		Arguments  string `json:"arguments"`
		Result     string `json:"result"`
		IsError    bool   `json:"is_error"`
		Timestamp  string `json:"timestamp"`
		// Phase is the controller state the agent recorded with the event
		// (M3-02); empty for events without one. Never inferred here.
		Phase    string `json:"phase,omitempty"`
		ExitCode *int   `json:"exit_code,omitempty"`
	}
	var events []toolEvent
	for rows.Next() {
		var e toolEvent
		var ts time.Time
		if err := rows.Scan(&e.WorkItemID, &e.TaskID, &e.AgentType, &e.Tool,
			&e.Arguments, &e.Result, &e.IsError, &ts, &e.Phase, &e.ExitCode); err != nil {
			continue
		}
		e.Timestamp = ts.Format(time.RFC3339Nano)
		events = append(events, e)
	}
	if events == nil {
		events = []toolEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}
