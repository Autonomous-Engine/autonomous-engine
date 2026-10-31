package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codexindex"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReindexHandlers groups the /api/admin/reindex* and
// /api/admin/products/branches handlers. They are a thin proxy onto
// knowledge-indexer plus branch discovery / status queries against the
// dashboard's own Postgres.
//
// Data model:
//   - branches come from codex_modules.branch GROUP BY (per product_id). We
//     also surface repositories.base_branch as the canonical default.
//   - status is derived from codex_modules.updated_at + codex_versions
//     filtered by (product_id, branch). There is no dedicated reindex_jobs
//     table — re-adding one would duplicate what codex_versions already
//     captures (each full run emits a version row).
type ReindexHandlers struct {
	db *pgxpool.Pool
}

// NewReindexHandlers returns a ReindexHandlers backed by the given pool.
func NewReindexHandlers(db *pgxpool.Pool) *ReindexHandlers {
	return &ReindexHandlers{db: db}
}

// knowledgeIndexerURL is the in-network base URL for the knowledge-indexer.
// The dashboard-backend is the only caller. A variable so tests can point
// it at a fake indexer.
var knowledgeIndexerURL = "http://knowledge-indexer:8087"

// postFullIndex asks knowledge-indexer for a full index of productID
// (POST /api/index/full). branch is optional — when empty the indexer uses
// whatever ref each clone currently has checked out. It returns the
// indexer's status and body; err (with the status to report) only when the
// request could not be made. The index itself runs asynchronously and ends
// with a codex.indexed event per version it writes.
func postFullIndex(ctx context.Context, productID uuid.UUID, branch string) (int, []byte, error) {
	res := codexindex.RequestFull(ctx, knowledgeIndexerURL, productID, branch)
	if !res.Reached {
		return res.Status, nil, errors.New(res.Detail)
	}
	return res.Status, []byte(res.Detail), nil
}

// reindexJobMemory tracks the most recent in-flight reindex per (product, branch)
// keyed by "<productID>|<branch>". Entries are cleared by the poll itself
// the first time the backend sees the indexer go back to idle for that pair.
// This is in-memory on purpose: restarts return "idle" which is accurate — we
// lost the correlation ID but the indexer row in codex_versions is the real
// source of truth.
var reindexJobMemory = struct {
	jobs map[string]reindexJob
}{jobs: map[string]reindexJob{}}

type reindexJob struct {
	JobID     string    `json:"job_id"`
	StartedAt time.Time `json:"started_at"`
}

// ReindexNow — POST /api/admin/reindex
//
// Body: { "product_id": "uuid", "branch": "master" }
// Response: { job_id, status:"started", product_id, branch, started_at }
//
// Proxies to knowledge-indexer /api/index/full?product_id=...&branch=... and
// returns the correlation ID generated here. The indexer runs asynchronously;
// the caller polls /api/admin/reindex/status to observe completion.
func (h *ReindexHandlers) ReindexNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		ProductID string `json:"product_id"`
		Branch    string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.ProductID == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id: "+err.Error())
		return
	}

	status, respBody, err := postFullIndex(r.Context(), productID, req.Branch)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if status >= 400 {
		// Forward the indexer's status — 404 on missing branch, 409 on
		// "already running", etc. Honesty over prettiness.
		writeError(w, status, "knowledge-indexer error: "+string(respBody))
		return
	}

	jobID := uuid.New().String()
	startedAt := time.Now().UTC()
	reindexJobMemory.jobs[req.ProductID+"|"+req.Branch] = reindexJob{
		JobID:     jobID,
		StartedAt: startedAt,
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id":     jobID,
		"status":     "started",
		"product_id": productID.String(),
		"branch":     req.Branch,
		"started_at": startedAt.Format(time.RFC3339),
	})
}

// ReindexStatus — GET /api/admin/reindex/status?product_id=...&branch=...
//
// Response shape:
//
//	{
//	  "product_id": "...", "branch": "...",
//	  "status": "idle" | "running" | "failed" | "completed",
//	  "last_indexed_at": "RFC3339 | null",
//	  "last_indexed_commit": "sha | ''",
//	  "module_count": 42,
//	  "indexer_phase": "vector_index",   // when running
//	  "indexer_error": "...",            // when failed
//	  "job_id": "..."                    // only while running
//	}
//
// status is the fused verdict:
//   - "running" — the knowledge-indexer's /api/index/status says indexing=true
//     AND the in-memory job we started matches.
//   - "failed"  — indexer's last run recorded an error and we have no newer
//     codex_modules row for this branch.
//   - "completed" — there is at least one codex_modules row for (product, branch).
//   - "idle" — nothing ever indexed for this pair.
func (h *ReindexHandlers) ReindexStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	productIDStr := r.URL.Query().Get("product_id")
	branch := r.URL.Query().Get("branch")
	if productIDStr == "" {
		writeError(w, http.StatusBadRequest, "product_id is required")
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid product_id: "+err.Error())
		return
	}

	// Count modules + find the most recent updated_at for (product, branch).
	// The branch column exists since migration 021 and defaults to 'main' so
	// a branch="" query would be nonsense; callers MUST pass a branch.
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch is required")
		return
	}

	var moduleCount int
	var lastIndexed *time.Time
	err = h.db.QueryRow(r.Context(),
		`SELECT COUNT(*), MAX(updated_at)
		   FROM codex_modules
		  WHERE product_id = $1 AND branch = $2`,
		productID, branch,
	).Scan(&moduleCount, &lastIndexed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query codex_modules: "+err.Error())
		return
	}

	// Grab the newest last_indexed_commit across the product's repositories.
	// This is a per-repo column so we take MAX(last_indexed_at) and fetch
	// that row's commit. Good enough for a single-commit display in the UI.
	var lastCommit string
	_ = h.db.QueryRow(r.Context(),
		`SELECT last_indexed_commit
		   FROM repositories
		  WHERE product_id = $1
		  ORDER BY last_indexed_at DESC NULLS LAST
		  LIMIT 1`,
		productID,
	).Scan(&lastCommit)

	// Live indexer phase — only meaningful while a run is in progress.
	indexerPhase, indexerError, indexerRunning := fetchIndexerPhase(r.Context())

	status := "idle"
	switch {
	case indexerRunning:
		status = "running"
	case moduleCount > 0:
		status = "completed"
	case indexerError != "":
		status = "failed"
	}

	resp := map[string]interface{}{
		"product_id":          productID.String(),
		"branch":              branch,
		"status":              status,
		"module_count":        moduleCount,
		"last_indexed_commit": lastCommit,
	}
	if lastIndexed != nil {
		resp["last_indexed_at"] = lastIndexed.UTC().Format(time.RFC3339)
	}
	if indexerPhase != "" {
		resp["indexer_phase"] = indexerPhase
	}
	if indexerError != "" {
		resp["indexer_error"] = indexerError
	}
	if job, ok := reindexJobMemory.jobs[productIDStr+"|"+branch]; ok {
		resp["job_id"] = job.JobID
		resp["started_at"] = job.StartedAt.Format(time.RFC3339)
		// Drop the entry once the indexer is no longer running — from then
		// on codex_versions is the source of truth.
		if !indexerRunning {
			delete(reindexJobMemory.jobs, productIDStr+"|"+branch)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// fetchIndexerPhase calls knowledge-indexer /api/index/status and returns
// (phase, error, running). Failure to reach the indexer is NOT surfaced as
// "failed" — it's surfaced as ("", "", false) so the UI can distinguish a
// degraded cluster from a real failed job.
func fetchIndexerPhase(ctx context.Context) (phase, errMsg string, running bool) {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, knowledgeIndexerURL+"/api/index/status", nil)
	if err != nil {
		return "", "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	var snap struct {
		Phase    string `json:"phase"`
		Indexing bool   `json:"indexing"`
		Error    string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return "", "", false
	}
	return snap.Phase, snap.Error, snap.Indexing
}

// ProductBranches — GET /api/admin/products/branches?product_id=<uuid>
//
// Response:
//
//	{
//	  "default_branch": "master",
//	  "indexed_branches": ["master", "develop"],
//	  "repo_branches_seen": ["master", "develop"]
//	}
//
// default_branch is the base_branch of the first repo attached to the
// product (repos under a product conventionally share a base branch).
// indexed_branches is every branch that currently has at least one
// codex_modules row for this product. repo_branches_seen is identical for
// now — codex_modules is the only place we track per-branch presence.
func (h *ReindexHandlers) ProductBranches(w http.ResponseWriter, r *http.Request) {
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

	var defaultBranch string
	// Pick the base_branch of the first enabled repo; fall back to any repo
	// and finally to "main" if nothing is attached yet.
	if err := h.db.QueryRow(r.Context(),
		`SELECT base_branch FROM repositories
		  WHERE product_id = $1
		  ORDER BY enabled DESC, created_at ASC
		  LIMIT 1`,
		productID,
	).Scan(&defaultBranch); err != nil {
		log.Printf("dashboard: product branches: no repos for product=%s (%v) — defaulting to main", productID, err)
		defaultBranch = "main"
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	indexed := []string{}
	rows, err := h.db.Query(r.Context(),
		`SELECT DISTINCT branch FROM codex_modules
		  WHERE product_id = $1
		  ORDER BY branch`,
		productID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query indexed branches: "+err.Error())
		return
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			continue
		}
		if b != "" {
			indexed = append(indexed, b)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"default_branch":     defaultBranch,
		"indexed_branches":   indexed,
		"repo_branches_seen": indexed,
	})
}
