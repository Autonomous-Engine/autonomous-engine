package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BackfillCodex handles POST /api/codex/backfill?repo_id=<uuid>.
// It publishes a codex.extract.requested event for the given repository's
// base branch at HEAD, triggering a full extraction run in the
// knowledge-indexer's commit-linked orchestrator.
func (h *Handlers) BackfillCodex(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("repo_id")
	repoID, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}
	var repo struct {
		CloneURL, BaseBranch string
	}
	err = h.wg.DB().QueryRow(r.Context(),
		`SELECT github_url, base_branch FROM repositories WHERE id = $1`, repoID,
	).Scan(&repo.CloneURL, &repo.BaseBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"repo_id":    repoID,
		"commit_sha": "HEAD",
		"branch":     repo.BaseBranch,
		"parent_sha": "",
	})
	if err := h.msgBus.Publish(r.Context(), bus.TopicCodexExtractRequested, repoID.String(), payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ─── Codex V2 Endpoints ─────────────────────────────────────────────────────

// codexBlob is the JSON representation of a row from codex_blobs.
type codexBlob struct {
	Hash       string          `json:"hash"`
	Section    string          `json:"section"`
	Extractor  string          `json:"extractor"`
	Payload    json.RawMessage `json:"payload"`
	Confidence float64         `json:"confidence"`
	CreatedAt  time.Time       `json:"created_at"`
}

// codexChangedPair represents a blob that changed between two snapshots.
type codexChangedPair struct {
	From codexBlob `json:"from"`
	To   codexBlob `json:"to"`
}

// CodexDiff handles GET /api/codex/diff?repo_id=…&from=…&to=…
// It compares two snapshots (by commit SHA) for a repo and returns the added,
// removed, and changed blobs.
func (h *Handlers) CodexDiff(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repo_id")
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}
	fromSHA := r.URL.Query().Get("from")
	toSHA := r.URL.Query().Get("to")
	if fromSHA == "" || toSHA == "" {
		http.Error(w, "from and to query params are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	pool := h.wg.DB()

	// Load blob_hashes for both snapshots.
	var fromHashes, toHashes []string
	err = pool.QueryRow(ctx,
		`SELECT blob_hashes FROM codex_snapshots WHERE repo_id = $1 AND commit_sha = $2`,
		repoID, fromSHA,
	).Scan(&fromHashes)
	if err != nil {
		http.Error(w, fmt.Sprintf("from snapshot not found: %v", err), http.StatusNotFound)
		return
	}
	err = pool.QueryRow(ctx,
		`SELECT blob_hashes FROM codex_snapshots WHERE repo_id = $1 AND commit_sha = $2`,
		repoID, toSHA,
	).Scan(&toHashes)
	if err != nil {
		http.Error(w, fmt.Sprintf("to snapshot not found: %v", err), http.StatusNotFound)
		return
	}

	fromSet := make(map[string]struct{}, len(fromHashes))
	for _, h := range fromHashes {
		fromSet[h] = struct{}{}
	}
	toSet := make(map[string]struct{}, len(toHashes))
	for _, h := range toHashes {
		toSet[h] = struct{}{}
	}

	// Collect all unique hashes we need to resolve.
	allHashes := make(map[string]struct{})
	for _, h := range fromHashes {
		allHashes[h] = struct{}{}
	}
	for _, h := range toHashes {
		allHashes[h] = struct{}{}
	}

	// Resolve all blobs in one query.
	hashList := make([]string, 0, len(allHashes))
	for h := range allHashes {
		hashList = append(hashList, h)
	}

	blobMap := make(map[string]codexBlob, len(hashList))
	if len(hashList) > 0 {
		rows, err := pool.Query(ctx,
			`SELECT hash, section, extractor, payload, confidence, created_at
			 FROM codex_blobs WHERE hash = ANY($1)`, hashList)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var b codexBlob
			if err := rows.Scan(&b.Hash, &b.Section, &b.Extractor, &b.Payload, &b.Confidence, &b.CreatedAt); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			blobMap[b.Hash] = b
		}
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Build extractor index for "from" blobs to detect changed (same extractor, different hash).
	fromByExtractor := make(map[string]string) // extractor → hash
	for _, h := range fromHashes {
		if b, ok := blobMap[h]; ok {
			fromByExtractor[b.Extractor] = h
		}
	}
	toByExtractor := make(map[string]string)
	for _, h := range toHashes {
		if b, ok := blobMap[h]; ok {
			toByExtractor[b.Extractor] = h
		}
	}

	var added []codexBlob
	var removed []codexBlob
	var changed []codexChangedPair

	// Changed: same extractor present in both, but different hash.
	changedFromHashes := make(map[string]struct{})
	changedToHashes := make(map[string]struct{})
	for ext, toHash := range toByExtractor {
		if fromHash, ok := fromByExtractor[ext]; ok && fromHash != toHash {
			changed = append(changed, codexChangedPair{
				From: blobMap[fromHash],
				To:   blobMap[toHash],
			})
			changedFromHashes[fromHash] = struct{}{}
			changedToHashes[toHash] = struct{}{}
		}
	}

	// Added: in "to" but not in "from", and not already accounted for as changed.
	for _, h := range toHashes {
		if _, inFrom := fromSet[h]; !inFrom {
			if _, isChanged := changedToHashes[h]; !isChanged {
				if b, ok := blobMap[h]; ok {
					added = append(added, b)
				}
			}
		}
	}

	// Removed: in "from" but not in "to", and not already accounted for as changed.
	for _, h := range fromHashes {
		if _, inTo := toSet[h]; !inTo {
			if _, isChanged := changedFromHashes[h]; !isChanged {
				if b, ok := blobMap[h]; ok {
					removed = append(removed, b)
				}
			}
		}
	}

	// Ensure non-nil slices for JSON encoding.
	if added == nil {
		added = []codexBlob{}
	}
	if removed == nil {
		removed = []codexBlob{}
	}
	if changed == nil {
		changed = []codexChangedPair{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"added":   added,
		"removed": removed,
		"changed": changed,
	})
}

// contractEdge is the JSON representation of a row from codex_contract_edges.
type contractEdge struct {
	Hash           string    `json:"hash"`
	ContractType   string    `json:"contract_type"`
	ContractName   string    `json:"contract_name"`
	ProducerRepoID uuid.UUID `json:"producer_repo_id"`
	ProducerFile   string    `json:"producer_file"`
	ProducerLine   int       `json:"producer_line"`
	ConsumerRepoID uuid.UUID `json:"consumer_repo_id"`
	ConsumerFile   string    `json:"consumer_file"`
	ConsumerLine   int       `json:"consumer_line"`
	Confidence     float64   `json:"confidence"`
	ExtractedAt    time.Time `json:"extracted_at"`
}

// ContractEdges handles GET /api/codex/contract-edges?product_id=…&contract_type=…&producer_repo=…&consumer_repo=…&limit=…&offset=…
// Returns a paginated list of contract edges filtered by product (via repositories).
func (h *Handlers) ContractEdges(w http.ResponseWriter, r *http.Request) {
	productIDStr := r.URL.Query().Get("product_id")
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 200 {
		limit = 200
	}

	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	ctx := r.Context()
	pool := h.wg.DB()

	// Build query dynamically based on optional filters.
	query := `SELECT ce.hash, ce.contract_type, ce.contract_name,
	                 ce.producer_repo_id, ce.producer_file, ce.producer_line,
	                 ce.consumer_repo_id, ce.consumer_file, ce.consumer_line,
	                 ce.confidence, ce.extracted_at
	          FROM codex_contract_edges ce
	          WHERE (ce.producer_repo_id IN (SELECT id FROM repositories WHERE product_id = $1)
	              OR ce.consumer_repo_id IN (SELECT id FROM repositories WHERE product_id = $1))`

	args := []any{productID}
	argIdx := 2

	if ct := r.URL.Query().Get("contract_type"); ct != "" {
		query += fmt.Sprintf(` AND ce.contract_type = $%d`, argIdx)
		args = append(args, ct)
		argIdx++
	}
	if pr := r.URL.Query().Get("producer_repo"); pr != "" {
		prID, err := uuid.Parse(pr)
		if err != nil {
			http.Error(w, "invalid producer_repo", http.StatusBadRequest)
			return
		}
		query += fmt.Sprintf(` AND ce.producer_repo_id = $%d`, argIdx)
		args = append(args, prID)
		argIdx++
	}
	if cr := r.URL.Query().Get("consumer_repo"); cr != "" {
		crID, err := uuid.Parse(cr)
		if err != nil {
			http.Error(w, "invalid consumer_repo", http.StatusBadRequest)
			return
		}
		query += fmt.Sprintf(` AND ce.consumer_repo_id = $%d`, argIdx)
		args = append(args, crID)
		argIdx++
	}

	query += fmt.Sprintf(` ORDER BY ce.extracted_at DESC LIMIT $%d OFFSET $%d`, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	edges := make([]contractEdge, 0)
	for rows.Next() {
		var e contractEdge
		if err := rows.Scan(
			&e.Hash, &e.ContractType, &e.ContractName,
			&e.ProducerRepoID, &e.ProducerFile, &e.ProducerLine,
			&e.ConsumerRepoID, &e.ConsumerFile, &e.ConsumerLine,
			&e.Confidence, &e.ExtractedAt,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(edges)
}

// topologyEntry is the JSON representation of a row from codex_product_topology.
type topologyEntry struct {
	RepoID     uuid.UUID       `json:"repo_id"`
	Role       string          `json:"role"`
	Confidence float64         `json:"confidence"`
	Evidence   json.RawMessage `json:"evidence"`
}

// Topology handles GET /api/codex/topology?product_id=…
// Returns the list of topology roles and repos for a product.
func (h *Handlers) Topology(w http.ResponseWriter, r *http.Request) {
	productIDStr := r.URL.Query().Get("product_id")
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	rows, err := h.wg.DB().Query(ctx,
		`SELECT repo_id, role, confidence, evidence
		 FROM codex_product_topology
		 WHERE product_id = $1
		 ORDER BY role, repo_id`, productID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	entries := make([]topologyEntry, 0)
	for rows.Next() {
		var e topologyEntry
		if err := rows.Scan(&e.RepoID, &e.Role, &e.Confidence, &e.Evidence); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// watchedBranch is the JSON representation of a row from codex_watched_branches.
type watchedBranch struct {
	RepoID             uuid.UUID  `json:"repo_id"`
	Branch             string     `json:"branch"`
	Source             string     `json:"source"`
	AddedBy            string     `json:"added_by"`
	AddedAt            time.Time  `json:"added_at"`
	Enabled            bool       `json:"enabled"`
	LastSnapshotCommit string     `json:"last_snapshot_commit"`
	LastSnapshotTime   *time.Time `json:"last_snapshot_time"`
}

// ListWatchedBranches handles GET /api/codex/watched-branches?repo_id=…
// Returns all watched branches (enabled and disabled) for a repository.
func (h *Handlers) ListWatchedBranches(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repo_id")
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	rows, err := h.wg.DB().Query(ctx,
		`SELECT wb.repo_id, wb.branch, wb.source, wb.added_by, wb.added_at, wb.enabled,
		        COALESCE(ls.commit_sha, ''), ls.extracted_at
		 FROM codex_watched_branches wb
		 LEFT JOIN LATERAL (
		   SELECT commit_sha, extracted_at
		   FROM codex_snapshots
		   WHERE repo_id = wb.repo_id AND branch = wb.branch
		   ORDER BY extracted_at DESC
		   LIMIT 1
		 ) ls ON true
		 WHERE wb.repo_id = $1
		 ORDER BY wb.branch`, repoID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	branches := make([]watchedBranch, 0)
	for rows.Next() {
		var b watchedBranch
		if err := rows.Scan(&b.RepoID, &b.Branch, &b.Source, &b.AddedBy, &b.AddedAt, &b.Enabled,
			&b.LastSnapshotCommit, &b.LastSnapshotTime); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		branches = append(branches, b)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(branches)
}

// addWatchedBranchRequest is the JSON body for POST /api/codex/watched-branches.
type addWatchedBranchRequest struct {
	RepoID  uuid.UUID `json:"repo_id"`
	Branch  string    `json:"branch"`
	Source  string    `json:"source"`
	AddedBy string    `json:"added_by"`
}

// AddWatchedBranch handles POST /api/codex/watched-branches.
// Inserts a new watched branch. Uses ON CONFLICT DO NOTHING to be idempotent.
func (h *Handlers) AddWatchedBranch(w http.ResponseWriter, r *http.Request) {
	var req addWatchedBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.RepoID == uuid.Nil || req.Branch == "" {
		http.Error(w, "repo_id and branch are required", http.StatusBadRequest)
		return
	}

	_, err := h.wg.DB().Exec(r.Context(),
		`INSERT INTO codex_watched_branches (repo_id, branch, source, added_by, added_at, enabled)
		 VALUES ($1, $2, $3, $4, now(), true)
		 ON CONFLICT DO NOTHING`,
		req.RepoID, req.Branch, req.Source, req.AddedBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
}

// disableWatchedBranchRequest is the JSON body for POST /api/codex/watched-branches/disable.
type disableWatchedBranchRequest struct {
	RepoID uuid.UUID `json:"repo_id"`
	Branch string    `json:"branch"`
}

// DisableWatchedBranch handles POST /api/codex/watched-branches/disable.
// Sets enabled=false for the specified watched branch.
func (h *Handlers) DisableWatchedBranch(w http.ResponseWriter, r *http.Request) {
	var req disableWatchedBranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.RepoID == uuid.Nil || req.Branch == "" {
		http.Error(w, "repo_id and branch are required", http.StatusBadRequest)
		return
	}

	tag, err := h.wg.DB().Exec(r.Context(),
		`UPDATE codex_watched_branches SET enabled = false
		 WHERE repo_id = $1 AND branch = $2`,
		req.RepoID, req.Branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "watched branch not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// gitStrategy is the JSON representation of a row from codex_git_strategy.
type gitStrategy struct {
	RepoID           uuid.UUID       `json:"repo_id"`
	Pattern          string          `json:"pattern"`
	MainBranch       string          `json:"main_branch"`
	LongLivedBranches []string       `json:"long_lived_branches"`
	MergeStrategy    string          `json:"merge_strategy"`
	PRRequired       bool            `json:"pr_required"`
	Evidence         json.RawMessage `json:"evidence"`
	ExtractedAt      time.Time       `json:"extracted_at"`
}

// GitStrategy handles GET /api/codex/git-strategy?repo_id=…
// Returns the full git strategy row for a repository.
func (h *Handlers) GitStrategy(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repo_id")
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}

	var gs gitStrategy
	err = h.wg.DB().QueryRow(r.Context(),
		`SELECT repo_id, pattern, main_branch, long_lived_branches,
		        merge_strategy, pr_required, evidence, extracted_at
		 FROM codex_git_strategy WHERE repo_id = $1`, repoID,
	).Scan(&gs.RepoID, &gs.Pattern, &gs.MainBranch, &gs.LongLivedBranches,
		&gs.MergeStrategy, &gs.PRRequired, &gs.Evidence, &gs.ExtractedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "git strategy not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gs)
}

// codexSnapshot is the JSON representation of a row from codex_snapshots (without resolved blobs).
type codexSnapshot struct {
	RepoID      uuid.UUID `json:"repo_id"`
	CommitSHA   string    `json:"commit_sha"`
	Branch      string    `json:"branch"`
	ParentSHA   string    `json:"parent_sha"`
	BlobHashes  []string  `json:"blob_hashes"`
	ExtractedAt time.Time `json:"extracted_at"`
}

// ListSnapshots handles GET /api/codex/snapshots?repo_id=…&branch=…&limit=…
// Returns a list of snapshots for a repository, ordered by extracted_at DESC.
func (h *Handlers) ListSnapshots(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repo_id")
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}

	ctx := r.Context()
	pool := h.wg.DB()

	var rows pgx.Rows
	branch := r.URL.Query().Get("branch")
	if branch != "" {
		rows, err = pool.Query(ctx,
			`SELECT repo_id, commit_sha, branch, parent_sha, blob_hashes, extracted_at
			 FROM codex_snapshots
			 WHERE repo_id = $1 AND branch = $2
			 ORDER BY extracted_at DESC
			 LIMIT $3`, repoID, branch, limit)
	} else {
		rows, err = pool.Query(ctx,
			`SELECT repo_id, commit_sha, branch, parent_sha, blob_hashes, extracted_at
			 FROM codex_snapshots
			 WHERE repo_id = $1
			 ORDER BY extracted_at DESC
			 LIMIT $2`, repoID, limit)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	snapshots := make([]codexSnapshot, 0)
	for rows.Next() {
		var s codexSnapshot
		if err := rows.Scan(&s.RepoID, &s.CommitSHA, &s.Branch, &s.ParentSHA, &s.BlobHashes, &s.ExtractedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		snapshots = append(snapshots, s)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshots)
}

// snapshotWithBlobs is a snapshot with all blob payloads resolved.
type snapshotWithBlobs struct {
	RepoID      uuid.UUID   `json:"repo_id"`
	CommitSHA   string      `json:"commit_sha"`
	Branch      string      `json:"branch"`
	ParentSHA   string      `json:"parent_sha"`
	ExtractedAt time.Time   `json:"extracted_at"`
	Blobs       []codexBlob `json:"blobs"`
}

// GetSnapshot handles GET /api/codex/snapshot?repo_id=…&commit_sha=…
// Returns a single snapshot with all blob payloads resolved.
func (h *Handlers) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	repoIDStr := r.URL.Query().Get("repo_id")
	repoID, err := uuid.Parse(repoIDStr)
	if err != nil {
		http.Error(w, "invalid repo_id", http.StatusBadRequest)
		return
	}
	commitSHA := r.URL.Query().Get("commit_sha")
	if commitSHA == "" {
		http.Error(w, "commit_sha is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	pool := h.wg.DB()

	var snap snapshotWithBlobs
	var blobHashes []string
	err = pool.QueryRow(ctx,
		`SELECT repo_id, commit_sha, branch, parent_sha, blob_hashes, extracted_at
		 FROM codex_snapshots
		 WHERE repo_id = $1 AND commit_sha = $2`, repoID, commitSHA,
	).Scan(&snap.RepoID, &snap.CommitSHA, &snap.Branch, &snap.ParentSHA, &blobHashes, &snap.ExtractedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			http.Error(w, "snapshot not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Resolve blobs.
	snap.Blobs = make([]codexBlob, 0, len(blobHashes))
	if len(blobHashes) > 0 {
		rows, err := pool.Query(ctx,
			`SELECT hash, section, extractor, payload, confidence, created_at
			 FROM codex_blobs WHERE hash = ANY($1)`, blobHashes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var b codexBlob
			if err := rows.Scan(&b.Hash, &b.Section, &b.Extractor, &b.Payload, &b.Confidence, &b.CreatedAt); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			snap.Blobs = append(snap.Blobs, b)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}
