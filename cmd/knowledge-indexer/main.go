package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/langanalyzer"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding/prefill"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	serviceName      = "knowledge-indexer"
	consumerGroup    = "knowledge-indexer"
	servicePort      = 8087
	repoPath         = "/repo-cache"
	vectorCollection = "codebase"
)

// ─── Indexing phase constants ──────────────────────────────────────────────────

type IndexPhase string

const (
	PhaseIdle         IndexPhase = "idle"
	PhaseCloning      IndexPhase = "cloning"
	PhaseDetectLang   IndexPhase = "detecting_languages"
	PhaseScanFiles    IndexPhase = "scanning_files"
	PhaseAnalyzeAST   IndexPhase = "analyzing_ast"
	PhaseDependencies IndexPhase = "building_dependencies"
	PhasePatterns     IndexPhase = "detecting_patterns"
	PhaseGitHistory   IndexPhase = "analyzing_git_history"
	PhaseVectorIndex  IndexPhase = "vector_indexing"
	PhaseCodexUpdate  IndexPhase = "updating_codex"
	PhaseDeepAnalysis IndexPhase = "deep_analysis"
	PhasePrefill      IndexPhase = "prefilling_candidates"
	PhaseComplete     IndexPhase = "complete"
	PhaseFailed       IndexPhase = "failed"
)

// ─── Progress tracking ────────────────────────────────────────────────────────

// IndexProgress tracks the state of an in-flight indexing run.
type IndexProgress struct {
	mu             sync.RWMutex
	Phase          IndexPhase `json:"phase"`
	FilesProcessed int        `json:"files_processed"`
	TotalFiles     int        `json:"total_files"`
	ModulesFound   int        `json:"modules_found"`
	PatternsFound  int        `json:"patterns_found"`
	CandidateCount int        `json:"candidate_count"`
	CurrentFile    string     `json:"current_file"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`

	// onPhaseChange is called (if non-nil) after every setPhase, complete,
	// or fail to push real-time progress to the dashboard via Kafka.
	onPhaseChange func()
}

func newProgress() *IndexProgress {
	return &IndexProgress{
		Phase:     PhaseIdle,
		StartedAt: time.Now().UTC(),
	}
}

func (p *IndexProgress) setPhase(phase IndexPhase) {
	p.mu.Lock()
	p.Phase = phase
	cb := p.onPhaseChange
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (p *IndexProgress) setFile(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CurrentFile = path
	p.FilesProcessed++
}

func (p *IndexProgress) setDetail(detail string) {
	p.mu.Lock()
	p.CurrentFile = detail
	cb := p.onPhaseChange
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (p *IndexProgress) setTotal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.TotalFiles = n
}

func (p *IndexProgress) addModules(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ModulesFound += n
}

func (p *IndexProgress) addPatterns(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.PatternsFound += n
}

func (p *IndexProgress) addCandidates(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CandidateCount += n
}

func (p *IndexProgress) appendError(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Error == "" {
		p.Error = msg
	} else {
		p.Error = p.Error + "; " + msg
	}
}

func (p *IndexProgress) complete() {
	p.mu.Lock()
	p.Phase = PhaseComplete
	now := time.Now().UTC()
	p.FinishedAt = &now
	cb := p.onPhaseChange
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (p *IndexProgress) fail(err error) {
	p.mu.Lock()
	p.Phase = PhaseFailed
	p.Error = err.Error()
	now := time.Now().UTC()
	p.FinishedAt = &now
	cb := p.onPhaseChange
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// indexProgressSnapshot is a lockless copy of IndexProgress used for
// serializing the current state to HTTP clients. Defining a separate
// struct (rather than returning IndexProgress by value) keeps govet's
// copylocks check happy since IndexProgress embeds a sync.RWMutex.
type indexProgressSnapshot struct {
	Phase          IndexPhase `json:"phase"`
	FilesProcessed int        `json:"files_processed"`
	TotalFiles     int        `json:"total_files"`
	ModulesFound   int        `json:"modules_found"`
	PatternsFound  int        `json:"patterns_found"`
	CandidateCount int        `json:"candidate_count"`
	CurrentFile    string     `json:"current_file"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
}

func (p *IndexProgress) snapshot() indexProgressSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return indexProgressSnapshot{
		Phase:          p.Phase,
		FilesProcessed: p.FilesProcessed,
		TotalFiles:     p.TotalFiles,
		ModulesFound:   p.ModulesFound,
		PatternsFound:  p.PatternsFound,
		CandidateCount: p.CandidateCount,
		CurrentFile:    p.CurrentFile,
		StartedAt:      p.StartedAt,
		FinishedAt:     p.FinishedAt,
		Error:          p.Error,
	}
}

// ─── Analysis result types ────────────────────────────────────────────────────

// detectedLanguage holds info about a language found in the repo.
type detectedLanguage struct {
	Name      string
	Primary   bool
	FileCount int
}

// fileInfo holds metadata about a single file in the repo.
type fileInfo struct {
	Path     string
	Category string // source, test, config, docs
	Language string
	Size     int64
}

// functionChunk represents an extracted function or method.
type functionChunk struct {
	FilePath  string
	Package   string
	Name      string
	Signature string
	Body      string
	StartLine int
}

// moduleInfo holds analysis results for a single package/module.
type moduleInfo struct {
	Name      string
	Path      string
	Language  string
	Functions []functionChunk
	Types     []string
	Imports   []string
	IsTest    bool
}

// gitFileStats holds change frequency info from git log.
type gitFileStats struct {
	Path         string
	CommitCount  int
	LastModified string
	Contributors []string
}

// patternGroup groups files that share a naming/structural pattern.
type patternGroup struct {
	Pattern string
	Files   []string
	Count   int
}

// ─── Legacy Rust regex (kept for Rust files not yet handled by langanalyzer) ──

var reRustFn = regexp.MustCompile(`(?m)^(?:pub\s+)?fn\s+(\w+)\s*\(`)

// ─── Server ───────────────────────────────────────────────────────────────────

type server struct {
	cfg             *config.Config
	pool            *pgxpool.Pool
	kb              bus.MessageBus
	cx              *codex.Codex
	vs              *vector.VectorStore
	llmSvc          *llm.Service
	productStore    *products.ProductStore
	onboardingStore *onboarding.Store

	// vectorSize is the embedding dimension returned by the configured
	// embedding provider (see BuildEmbeddingService). It is NOT a compile-time
	// constant any more — nomic-embed-text produces 768, text-embedding-3-small
	// produces 1536, text-embedding-3-large produces 3072, etc. The value is
	// baked into the Qdrant collection at creation; a mismatch at boot
	// refuses to start the service.
	vectorSize int

	progressMu sync.RWMutex
	progress   *IndexProgress
	indexing   bool
}

func newServer(
	cfg *config.Config,
	pool *pgxpool.Pool,
	kb bus.MessageBus,
	cx *codex.Codex,
	vs *vector.VectorStore,
	llmSvc *llm.Service,
	ps *products.ProductStore,
	vectorSize int,
) *server {
	var obStore *onboarding.Store
	if pool != nil {
		obStore = onboarding.NewStore(pool)
	}
	return &server{
		cfg:             cfg,
		pool:            pool,
		kb:              kb,
		cx:              cx,
		vs:              vs,
		llmSvc:          llmSvc,
		productStore:    ps,
		onboardingStore: obStore,
		vectorSize:      vectorSize,
		progress:        newProgress(),
	}
}

// ─── HTTP handlers ────────────────────────────────────────────────────────────

// fullIndexHandler triggers a full codebase analysis for all repos in a product.
// POST /api/index/full
// Body: {"product_id":"...", "branch":"..."}  (both optional)
// Query params: ?product_id=...&branch=... (take precedence over body when present)
//
// When `branch` is supplied the indexer performs `git checkout <branch>` in
// each clone before walking; if the branch is not present locally it is
// fetched from origin. A missing branch returns HTTP 404. When `branch` is
// omitted the existing behaviour (use gitCurrentBranch of each clone) runs.
func (s *server) fullIndexHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID string `json:"product_id"`
		Branch    string `json:"branch"`
	}
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

	// Query params override body (handy for the dashboard-backend proxy).
	if q := r.URL.Query().Get("product_id"); q != "" {
		req.ProductID = q
	}
	if q := r.URL.Query().Get("branch"); q != "" {
		req.Branch = q
	}
	req.Branch = strings.TrimSpace(req.Branch)

	s.progressMu.Lock()
	if s.indexing {
		s.progressMu.Unlock()
		http.Error(w, "indexing already in progress", http.StatusConflict)
		return
	}

	// Pre-validate the branch exists in every enabled repo clone so we can
	// return a real 404 BEFORE flipping the progress state. Falling through
	// silently when a branch is missing is exactly the failure mode we want
	// to avoid.
	if req.Branch != "" && req.ProductID != "" && s.productStore != nil {
		if status, err := s.prepareReposForBranch(r.Context(), req.ProductID, req.Branch); err != nil {
			s.progressMu.Unlock()
			http.Error(w, err.Error(), status)
			return
		}
	}

	s.indexing = true
	prog := newProgress()
	s.progress = prog
	s.progressMu.Unlock()

	go func() {
		var err error
		if req.ProductID != "" && s.productStore != nil {
			err = s.runFullIndexProductBranch(context.Background(), prog, req.ProductID, req.Branch)
		} else {
			// Legacy single-repo fallback — branch override is only honoured
			// when a product_id is supplied. Without the product store we do
			// not know which clones to operate on.
			repoCheck := guards.CheckRepoAvailable(repoPath)
			if !repoCheck.Valid {
				log.Printf("knowledge-indexer: repo check failed: %s", repoCheck.Message)
				prog.fail(fmt.Errorf("%s", repoCheck.Message))
				s.progressMu.Lock()
				s.indexing = false
				s.progressMu.Unlock()
				return
			}
			err = s.runFullIndex(context.Background(), prog)
		}
		if err != nil {
			log.Printf("knowledge-indexer: full index failed: %v", err)
			prog.fail(err)
		}
		s.progressMu.Lock()
		s.indexing = false
		s.progressMu.Unlock()
	}()

	resp := map[string]string{
		"status":  "started",
		"message": "Full codebase indexing started",
	}
	if req.Branch != "" {
		resp["branch"] = req.Branch
	}
	if req.ProductID != "" {
		resp["product_id"] = req.ProductID
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// prepareReposForBranch checks out `branch` in every enabled repo clone for
// the given product. If the branch is not present locally it attempts a
// `git fetch origin <branch>:<branch>` first. Returns (status, err) where
// status is an HTTP code suitable for the client. Callers MUST hold
// s.progressMu while invoking this so no concurrent run mutates state.
func (s *server) prepareReposForBranch(ctx context.Context, productIDStr, branch string) (int, error) {
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid product_id: %w", err)
	}
	repos, err := s.productStore.GetRepositoriesForProduct(ctx, productID)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("list repositories: %w", err)
	}
	hit := 0
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		hit++
		if err := gitCheckoutBranch(repo.ClonePath, branch); err != nil {
			return http.StatusNotFound, fmt.Errorf("branch %q not available in repo %q: %v", branch, repo.Name, err)
		}
	}
	if hit == 0 {
		return http.StatusBadRequest, fmt.Errorf("no enabled repositories for product %s", productIDStr)
	}
	return http.StatusOK, nil
}

// gitCheckoutBranch ensures the worktree at `path` is on `branch`.
//
// The clone volume is mounted read-only in the knowledge-indexer container
// (the github-service owns writes) so a blind `git checkout` would fail
// even when we're already on the right branch. We therefore:
//  1. short-circuit when gitCurrentBranch(path) already matches `branch`
//     (the common case — same-branch reindex)
//  2. check whether the branch ref exists locally (show-ref is read-only)
//     and only then attempt the write-side checkout.
//  3. if the ref is missing, attempt `git fetch origin <branch>:<branch>`
//     then the checkout.
//
// Returns an error if the branch cannot be made current — callers should
// translate that into HTTP 404.
func gitCheckoutBranch(path, branch string) error {
	// Fast path: already on the branch. No writes needed.
	if gitCurrentBranch(path) == branch {
		log.Printf("knowledge-indexer: branch=%s already current at %s", branch, path)
		return nil
	}

	// Does the ref exist locally? `show-ref` does not write, so it works
	// even on read-only mounts.
	sr := exec.Command("git", "-C", path, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	sr.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := sr.Run(); err == nil {
		// Local branch exists — attempt checkout.
		co := exec.Command("git", "-C", path, "checkout", branch)
		co.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, coErr := co.CombinedOutput(); coErr != nil {
			return fmt.Errorf("checkout %s: %w: %s", branch, coErr, strings.TrimSpace(string(out)))
		}
		log.Printf("knowledge-indexer: checked out branch=%s in %s", branch, path)
		return nil
	}

	// Ref is missing locally — fetch it and then check out.
	fetch := exec.Command("git", "-C", path, "fetch", "origin", branch+":"+branch)
	fetch.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch origin %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	co2 := exec.Command("git", "-C", path, "checkout", branch)
	co2.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := co2.CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s after fetch: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	log.Printf("knowledge-indexer: fetched+checked out branch=%s in %s", branch, path)
	return nil
}

// incrementalIndexHandler triggers incremental re-indexing for changed files.
// POST /api/index/incremental
// Body: {"repository_id":"...", "since":"HEAD~1"}
func (s *server) incrementalIndexHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RepositoryID string `json:"repository_id"`
		Since        string `json:"since"` // git ref or commit SHA
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Since = "HEAD~1"
	}
	if req.Since == "" {
		req.Since = "HEAD~1"
	}

	// Determine the clone (and its repository and product) to index.
	step := indexStep{path: repoPath}
	if req.RepositoryID != "" && s.productStore != nil {
		repoID, err := uuid.Parse(req.RepositoryID)
		if err == nil {
			repo, err := s.productStore.GetRepository(context.Background(), repoID)
			if err == nil {
				step = indexStep{path: repo.ClonePath, productID: repo.ProductID, repoID: repo.ID}
			}
		}
	}

	// Validate that the repository is available.
	repoCheck := guards.CheckRepoAvailable(step.path)
	if !repoCheck.Valid {
		log.Printf("knowledge-indexer: repo check failed: %s", repoCheck.Message)
		http.Error(w, repoCheck.Message, http.StatusPreconditionFailed)
		return
	}

	s.progressMu.Lock()
	if s.indexing {
		s.progressMu.Unlock()
		http.Error(w, "indexing already in progress", http.StatusConflict)
		return
	}
	s.indexing = true
	prog := newProgress()
	s.progress = prog
	s.progressMu.Unlock()

	go func() {
		if err := s.runIncrementalIndex(context.Background(), prog, step, req.Since); err != nil {
			log.Printf("knowledge-indexer: incremental index failed: %v", err)
			prog.fail(err)
		}
		s.progressMu.Lock()
		s.indexing = false
		s.progressMu.Unlock()
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":        "started",
		"message":       "Incremental indexing started",
		"since":         req.Since,
		"repository_id": req.RepositoryID,
	})
}

// statusHandler returns current indexing progress.
// GET /api/index/status
func (s *server) statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.progressMu.RLock()
	snap := s.progress.snapshot()
	s.progressMu.RUnlock()

	writeJSON(w, http.StatusOK, snap)
}

// ─── Kafka event handler ──────────────────────────────────────────────────────

// handleKafkaMessage processes indexing trigger messages from Kafka.
func (s *server) handleKafkaMessage(ctx context.Context, msg *bus.Message) error {
	log.Printf("knowledge-indexer: received Kafka message on topic=%s key=%s", msg.Topic, msg.Key)

	t, err := decodeIndexTrigger(msg.Value)
	if err != nil {
		log.Printf("knowledge-indexer: unmarshal kafka payload: %v — skipping", err)
		return nil // non-retryable
	}

	if t.notification() {
		log.Printf("knowledge-indexer: debug: topic=%s key=%s carries no trigger and no repository_id/product_id — a notification, not an index request; skipping", msg.Topic, msg.Key)
		return nil
	}

	trigger := t.Trigger
	since := t.Since
	productID := t.ProductID

	// Ignore self-triggered events BEFORE creating new progress — otherwise we
	// overwrite the completed progress from the real index with an empty one.
	// "incremental_index" and "incremental_update" were published by the indexer
	// itself after a run completed; consuming them would create an infinite loop.
	// Since migration 060 completions go to codex.indexed instead, but messages
	// enqueued by an older build may still arrive, so they stay ignored.
	switch trigger {
	case "full_index", "full_index_product", "incremental_index", "incremental_update":
		log.Printf("knowledge-indexer: ignoring self-triggered %s event", trigger)
		return nil
	}

	// Every trigger but the onboarding full scan is an incremental index of
	// the clones planIncrementalTrigger resolves; resolve them before
	// claiming the indexing slot so a skip leaves the last progress intact.
	var plan triggerPlan
	if trigger != "onboarding_initial_scan" {
		plan = planIncrementalTrigger(ctx, t, s.triggerRepos(), repoPath, hasClone)
		if plan.skip != "" {
			log.Printf("knowledge-indexer: topic=%s key=%s trigger=%q: %s — skipping", msg.Topic, msg.Key, trigger, plan.skip)
			return nil
		}
	}

	s.progressMu.Lock()
	if s.indexing {
		s.progressMu.Unlock()
		log.Printf("knowledge-indexer: indexing already in progress, skipping Kafka trigger=%s", trigger)
		return nil
	}
	s.indexing = true
	prog := newProgress()
	s.progress = prog
	s.progressMu.Unlock()

	go func() {
		var err error
		switch trigger {
		case "onboarding_initial_scan":
			if productID != "" && s.productStore != nil {
				err = s.runFullIndexProduct(ctx, prog, productID)
			} else {
				err = s.runFullIndex(ctx, prog)
			}
		default:
			// post_merge, a repository's or a product's change, or a legacy
			// trigger: see planIncrementalTrigger. Refreshing is git pull,
			// as post_merge has always done.
			err = runTriggerPlan(plan, gitPull, func(st indexStep) error {
				return s.runIncrementalIndex(ctx, prog, st, since)
			})
		}
		if err != nil {
			log.Printf("knowledge-indexer: kafka-triggered index failed (trigger=%s): %v", trigger, err)
			prog.fail(err)
		}
		s.progressMu.Lock()
		s.indexing = false
		s.progressMu.Unlock()
	}()

	return nil
}

// triggerRepos is the product store as the trigger planner's repository
// source, nil without one (never a non-nil interface holding nil).
func (s *server) triggerRepos() triggerRepoSource {
	if s.productStore == nil {
		return nil
	}
	return s.productStore
}

// ─── Full indexing flow ───────────────────────────────────────────────────────

// runFullIndexProduct indexes all enabled repos for a product using the
// branch currently checked out in each clone. Legacy behaviour.
func (s *server) runFullIndexProduct(ctx context.Context, prog *IndexProgress, productIDStr string) error {
	return s.runFullIndexProductBranch(ctx, prog, productIDStr, "")
}

// runFullIndexProductBranch indexes all enabled repos for a product. When
// `branch` is non-empty every codex row + Qdrant payload is tagged with that
// branch; otherwise the indexer uses gitCurrentBranch() of each clone.
func (s *server) runFullIndexProductBranch(ctx context.Context, prog *IndexProgress, productIDStr, branch string) error {
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		return fmt.Errorf("invalid product_id: %w", err)
	}

	repos, err := s.productStore.GetRepositoriesForProduct(ctx, productID)
	if err != nil {
		return fmt.Errorf("get repos for product: %w", err)
	}

	enabledRepos := make([]*products.Repository, 0, len(repos))
	for _, r := range repos {
		if r.Enabled {
			enabledRepos = append(enabledRepos, r)
		}
	}

	if len(enabledRepos) == 0 {
		return fmt.Errorf("no enabled repositories for product %s", productIDStr)
	}

	// Look up product name for progress events.
	productName := productIDStr
	if product, pErr := s.productStore.GetProduct(ctx, productID); pErr == nil && product != nil {
		productName = product.Name
	}

	log.Printf("knowledge-indexer: full index product=%s repos=%d branch=%q", productIDStr, len(enabledRepos), branch)

	for _, repo := range enabledRepos {
		currentRepoName := repo.Name
		prog.onPhaseChange = func() {
			s.publishProgress(ctx, prog, productName, currentRepoName, &productID)
		}
		log.Printf("knowledge-indexer: indexing repo=%s path=%s branch=%q", repo.Name, repo.ClonePath, branch)
		token := repo.TokenOverride
		if token == "" {
			token = s.cfg.GitHub.Token
		}
		// Ensure the repo is cloned.
		if err := ensureRepo(repo.ClonePath, repo.GitHubURL, token); err != nil {
			log.Printf("knowledge-indexer: ensure repo %s: %v — skipping", repo.Name, err)
			continue
		}
		pid := productID
		rid := repo.ID
		if branch != "" {
			// The handler already validated the branch exists; re-run
			// checkout here as a safety net and then index with the
			// explicit branch tag.
			if err := gitCheckoutBranch(repo.ClonePath, branch); err != nil {
				log.Printf("knowledge-indexer: checkout branch %s in %s: %v — skipping", branch, repo.Name, err)
				continue
			}
			if err := s.indexOneRepoBranch(ctx, prog, repo.ClonePath, branch, &pid, &rid); err != nil {
				log.Printf("knowledge-indexer: index repo %s on branch %s: %v — continuing", repo.Name, branch, err)
			}
			continue
		}
		if err := s.indexOneRepo(ctx, prog, repo.ClonePath, &pid, &rid); err != nil {
			log.Printf("knowledge-indexer: index repo %s: %v — continuing", repo.Name, err)
		}
	}

	// Each repository's codex version above enqueued its codex.indexed
	// (updateCodex); there is no separate completion notice.
	prog.onPhaseChange = func() {
		s.publishProgress(ctx, prog, productName, "", &productID)
	}
	prog.complete()
	log.Printf("knowledge-indexer: full product index complete product=%s branch=%q", productIDStr, branch)
	return nil
}

func (s *server) runFullIndex(ctx context.Context, prog *IndexProgress) error {
	log.Println("knowledge-indexer: starting full index")

	// 1. Clone/pull repo
	prog.setPhase(PhaseCloning)
	if err := ensureRepo(repoPath, s.cfg.GitHub.Repo, s.cfg.GitHub.Token); err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	// The legacy single-repo index has no product: it writes codex rows but
	// no codex version, so no codex.indexed either.
	if err := s.indexOneRepo(ctx, prog, repoPath, nil, nil); err != nil {
		return err
	}

	prog.complete()
	log.Println("knowledge-indexer: full index complete")
	return nil
}

// indexOneRepo runs the full analysis pipeline for a single repo at the given
// path. When productID and repoID are non-nil, modules and patterns are tagged
// with the product/repo so per-product queries can find them. Legacy callers
// (single-repo env-based) pass nil, nil and the rows get NULL product_id.
//
// The branch tag is resolved via gitCurrentBranch(rp) — whatever ref the
// repo is currently checked out to. The indexer does NOT re-clone or
// checkout a specific ref; it indexes whatever's on disk and records which
// branch that is. Today that's always "main" (or whatever the github-service
// cloned) but once a per-branch re-index trigger lands, this will correctly
// tag each re-index with its own branch.
func (s *server) indexOneRepo(ctx context.Context, prog *IndexProgress, rp string, productID, repoID *uuid.UUID) error {
	branch := gitCurrentBranch(rp)
	log.Printf("knowledge-indexer: indexing repo=%s branch=%s product=%v", rp, branch, productID)
	return s.indexOneRepoBranch(ctx, prog, rp, branch, productID, repoID)
}

// indexOneRepoBranch is the branch-aware core. All upserts during this run
// are tagged with `branch`. See indexOneRepo for the default resolution.
func (s *server) indexOneRepoBranch(ctx context.Context, prog *IndexProgress, rp, branch string, productID, repoID *uuid.UUID) error {
	// 2. Detect languages
	prog.setPhase(PhaseDetectLang)
	languages, err := detectLanguages(rp)
	if err != nil {
		return fmt.Errorf("detect languages: %w", err)
	}
	log.Printf("knowledge-indexer: detected languages in %s: %v", rp, languageNames(languages))

	// 3. Scan files
	prog.setPhase(PhaseScanFiles)
	files, err := scanFiles(rp)
	if err != nil {
		return fmt.Errorf("scan files: %w", err)
	}
	prog.setTotal(len(files))
	log.Printf("knowledge-indexer: found %d files in %s", len(files), rp)

	// 4. AST-like analysis per module
	prog.setPhase(PhaseAnalyzeAST)
	modules, err := analyzeModules(rp, files, prog)
	if err != nil {
		return fmt.Errorf("analyze modules: %w", err)
	}

	// 5. Dependency mapping
	prog.setPhase(PhaseDependencies)
	depGraph := buildDependencyGraph(modules)

	// 6. Pattern detection
	prog.setPhase(PhasePatterns)
	patterns := detectPatterns(files)

	// 7. Git history analysis
	prog.setPhase(PhaseGitHistory)
	gitStats, err := analyzeGitHistory(rp)
	if err != nil {
		log.Printf("knowledge-indexer: git history analysis: %v — continuing", err)
	}

	// 8. Vector indexing — tag every point with product/repo/branch so
	// downstream product-scoped searches (codex_similar_code) can filter.
	prog.setPhase(PhaseVectorIndex)
	if err := s.vectorIndexModules(ctx, modules, productID, repoID, branch); err != nil {
		log.Printf("knowledge-indexer: vector indexing: %v — continuing", err)
	}

	// 9. Update Codex
	prog.setPhase(PhaseCodexUpdate)
	if err := s.updateCodex(ctx, languages, modules, patterns, depGraph, gitStats, productID, repoID, branch); err != nil {
		return fmt.Errorf("update codex: %w", err)
	}

	// 10. Deep analysis — LLM-powered architectural understanding.
	// Produces codex sections answering senior-engineer questions:
	// system boundaries, invariants, coupling model, failure modes, etc.
	// Skip if all 12 questions are already answered for this product (idempotent).
	if productID != nil && s.llmSvc != nil {
		existing := s.countDeepAnalysisSections(ctx, *productID)
		if existing >= len(deepAnalysisQuestions) {
			log.Printf("knowledge-indexer: deep analysis already complete (%d/%d sections) — skipping", existing, len(deepAnalysisQuestions))
		} else {
			prog.setPhase(PhaseDeepAnalysis)
			if err := s.runDeepAnalysis(ctx, rp, modules, depGraph, files, gitStats, *productID, repoID, branch); err != nil {
				log.Printf("knowledge-indexer: deep analysis failed: %v — continuing", err)
				prog.appendError(fmt.Sprintf("deep analysis: %v", err))
			}
		}
	}

	// 11. Prefill onboarding candidates — only runs when the indexer was invoked
	// for a specific product (legacy single-repo path has no product_id, so there
	// is nowhere to attach candidates). Errors are surfaced on prog.Error but
	// never abort the outer index run — the codex update already succeeded.
	if productID != nil && s.onboardingStore != nil {
		prog.setPhase(PhasePrefill)
		if err := s.runPrefill(ctx, prog, rp, languages, modules, patterns, gitStats, *productID, repoID); err != nil {
			log.Printf("knowledge-indexer: prefill candidates failed: %v", err)
			prog.appendError(fmt.Sprintf("prefill: %v", err))
		}
	}

	return nil
}

// runPrefill invokes prefill.RunCandidateExtractors, UPSERTs every candidate
// into onboarding_candidates, detects drift against already-reviewed rows,
// and publishes the appropriate Kafka events. Fail Honestly: errors degrade
// to a logged error + prog.Error entry rather than a silent skip. Still
// publishes onboarding.prefill.completed at the end so the dashboard knows
// the run finished even when individual steps failed.
func (s *server) runPrefill(
	ctx context.Context,
	prog *IndexProgress,
	rp string,
	languages []detectedLanguage,
	modules []moduleInfo,
	patterns []patternGroup,
	gitStats []gitFileStats,
	productID uuid.UUID,
	repoID *uuid.UUID,
) error {
	// Build extractor inputs from what the indexer already computed.
	langNames := make([]string, 0, len(languages))
	for _, l := range languages {
		langNames = append(langNames, l.Name)
	}
	moduleNames := make([]string, 0, len(modules))
	for _, m := range modules {
		moduleNames = append(moduleNames, m.Name)
	}

	// top-10 hotspots by commit count
	sortedStats := append([]gitFileStats(nil), gitStats...)
	for i := 0; i < len(sortedStats); i++ {
		for j := i + 1; j < len(sortedStats); j++ {
			if sortedStats[j].CommitCount > sortedStats[i].CommitCount {
				sortedStats[i], sortedStats[j] = sortedStats[j], sortedStats[i]
			}
		}
	}
	hotspots := make([]string, 0, 10)
	for i, gs := range sortedStats {
		if i >= 10 {
			break
		}
		hotspots = append(hotspots, gs.Path)
	}

	// Pattern-group summaries (cap ref files at 5 — matches extractor expectations).
	groups := make([]prefill.PatternGroupSummary, 0, len(patterns))
	for _, g := range patterns {
		groups = append(groups, prefill.PatternGroupSummary{
			Pattern:  g.Pattern,
			Count:    g.Count,
			RefFiles: firstN(g.Files, 5),
		})
	}

	// High-risk modules: dependents>5 (mirrors updateCodexModules).
	dependentsByMod := map[string]int{}
	for _, m := range modules {
		for _, imp := range m.Imports {
			dependentsByMod[imp]++
		}
	}
	var highRisk []string
	for _, m := range modules {
		if dependentsByMod[m.Name] > 5 {
			highRisk = append(highRisk, m.Name)
		}
	}

	// Sibling repo URLs for cross_repo_imports (same product, this repo excluded).
	var siblings []string
	if s.productStore != nil {
		repos, err := s.productStore.GetRepositoriesForProduct(ctx, productID)
		if err == nil {
			for _, r := range repos {
				if repoID != nil && r.ID == *repoID {
					continue
				}
				siblings = append(siblings, r.GitHubURL)
			}
		}
	}

	// Test framework + count.
	testCmd := testdetect.DetectTestCommand(rp)
	testFnCount := 0
	for _, m := range modules {
		for _, fn := range m.Functions {
			if m.IsTest || strings.Contains(strings.ToLower(fn.FilePath), "_test.") {
				testFnCount++
			}
		}
	}

	in := prefill.Inputs{
		ProductID:       productID,
		RepositoryID:    repoID,
		RepoPath:        rp,
		Languages:       langNames,
		ModuleNames:     moduleNames,
		HotspotFiles:    hotspots,
		PatternGroups:   groups,
		TestFramework:   testCmd,
		TestFunctions:   testFnCount,
		HighRiskModules: highRisk,
		SiblingRepoURLs: siblings,
	}
	fresh := prefill.RunCandidateExtractors(ctx, s.llmSvc, in)

	// Load stored BEFORE upsert so drift comparison sees the user-reviewed rows
	// (Upsert preserves user_status/user_value, but we still want the old value
	// to compare against the fresh one).
	stored, listErr := s.onboardingStore.List(ctx, productID)
	if listErr != nil {
		log.Printf("knowledge-indexer: list candidates for drift check: %v", listErr)
	}

	// Upsert every fresh candidate.
	upserted := 0
	for i := range fresh {
		if err := s.onboardingStore.Upsert(ctx, &fresh[i]); err != nil {
			log.Printf("knowledge-indexer: upsert candidate %s: %v", fresh[i].QuestionKey, err)
			continue
		}
		upserted++
	}
	prog.addCandidates(upserted)

	// Drift detection: only meaningful if we successfully loaded stored rows.
	var drifts []onboarding.Drift
	if listErr == nil {
		drifts = onboarding.DetectDrift(stored, fresh)
		if len(drifts) > 0 {
			log.Printf("knowledge-indexer: %d drift(s) detected for product %s", len(drifts), productID)
			payload, _ := json.Marshal(map[string]interface{}{
				"product_id": productID.String(),
				"drifts":     drifts,
			})
			if err := s.kb.Publish(ctx, bus.TopicOnboardingPrefillDriftDetected, productID.String(), payload); err != nil {
				log.Printf("knowledge-indexer: publish prefill.drift_detected: %v", err)
			}
		}
	}

	// Completion notification — always published so the dashboard knows the run
	// finished, even if listErr was non-nil or some upserts failed.
	completion, _ := json.Marshal(map[string]interface{}{
		"product_id":  productID.String(),
		"candidates":  upserted,
		"drift_count": len(drifts),
	})
	if err := s.kb.Publish(ctx, bus.TopicOnboardingPrefillCompleted, productID.String(), completion); err != nil {
		log.Printf("knowledge-indexer: publish prefill.completed: %v", err)
	}
	return nil
}

// ─── Incremental indexing flow ────────────────────────────────────────────────

func (s *server) runIncrementalIndex(ctx context.Context, prog *IndexProgress, st indexStep, since string) error {
	indexRepoPath := st.path
	log.Printf("knowledge-indexer: starting incremental index path=%s product=%s since=%s", indexRepoPath, st.productID, since)

	// 1. Verify repo exists (github-service handles cloning/pulling)
	prog.setPhase(PhaseCloning)
	if _, err := os.Stat(filepath.Join(indexRepoPath, ".git")); err != nil {
		return fmt.Errorf("repo not found at %s — ensure github-service has cloned it", indexRepoPath)
	}

	// 2. Find changed files
	prog.setPhase(PhaseScanFiles)
	changedFiles, err := gitDiffFiles(indexRepoPath, since)
	if err != nil {
		return fmt.Errorf("git diff: %w", err)
	}
	if len(changedFiles) == 0 {
		log.Println("knowledge-indexer: no changed files, incremental index complete")
		prog.complete()
		return nil
	}
	prog.setTotal(len(changedFiles))
	log.Printf("knowledge-indexer: %d changed files to re-index", len(changedFiles))

	// 3. The files of every module (directory) a changed file is in: the
	// module rows rewritten below must describe the whole module.
	allFiles, err := scanFiles(indexRepoPath)
	if err != nil {
		return fmt.Errorf("scan files: %w", err)
	}
	changedFileInfos := moduleFilesFor(allFiles, changedFiles)

	// 4. Re-analyze only changed modules
	prog.setPhase(PhaseAnalyzeAST)
	modules, err := analyzeModules(indexRepoPath, changedFileInfos, prog)
	if err != nil {
		return fmt.Errorf("analyze changed modules: %w", err)
	}

	// The branch is the indexed clone's (F3: per-repository clones come
	// through here, not only the legacy REPO_PATH).
	incBranch := incrementalIndexBranch(indexRepoPath)
	if incBranch == "" {
		incBranch = "main"
	}
	// The product's stored modules of this repository and branch: the
	// changed modules' imports of unchanged modules resolve against them,
	// and their dependents are kept (this run sees only the changed modules).
	var productID, repoID *uuid.UUID
	var stored []*models.CodexModule
	if st.productID != uuid.Nil && st.repoID != uuid.Nil {
		pid, rid := st.productID, st.repoID
		productID, repoID = &pid, &rid
		stored, err = s.storedRepoModules(ctx, pid, rid, incBranch)
		if err != nil {
			return fmt.Errorf("read stored modules: %w", err)
		}
	}

	// 5. Update dependency graph for affected modules, resolved against
	// every known module of the repository.
	prog.setPhase(PhaseDependencies)
	depGraph := buildDependencyGraphWith(modules, storedModuleNames(stored))

	// 6. Re-embed changed functions in Qdrant. Incremental indexing doesn't
	// thread product/repo context yet; pass nil so the payload drops those
	// keys but still carries branch (for branch-aware benchmark filters).
	prog.setPhase(PhaseVectorIndex)
	if err := s.reIndexModules(ctx, modules, nil, nil, incBranch); err != nil {
		log.Printf("knowledge-indexer: re-indexing vectors: %v — continuing", err)
	}

	// 7. Update relevant Codex entries. Branch is resolved from the indexed
	// clone. When the trigger resolved the clone's repository (migration
	// 060), the rows are written to its product's partition — the one every
	// product-scoped reader uses — with dependents added to the stored ones
	// (this run sees only the changed modules); the legacy REPO_PATH has no
	// product and keeps the unscoped rows.
	prog.setPhase(PhaseCodexUpdate)
	var existingDependents map[string][]string
	if productID != nil {
		existingDependents = storedDependentsOf(stored)
	}
	changed, modErr := s.updateCodexModules(ctx, modules, depGraph, productID, repoID, incBranch, existingDependents)

	// 8. A codex version (and its codex.indexed event) for the product, when
	// a module row changed — also when some other module failed to upsert,
	// so the rows that did change are announced.
	if v, ok := incrementalIndexVersion(st, incBranch, changed); ok {
		rec, err := s.cx.RecordVersion(ctx, v)
		if err != nil {
			return errors.Join(modErr, fmt.Errorf("record codex version: %w", err))
		}
		log.Printf("knowledge-indexer: incremental index product=%s branch=%s: codex v%d (%d modules changed)", st.productID, incBranch, rec.ProductVersion, len(changed))
	} else {
		log.Printf("knowledge-indexer: incremental index path=%s: no codex version (product=%s, %d modules changed)", st.path, st.productID, len(changed))
	}
	if modErr != nil {
		return fmt.Errorf("update codex modules: %w", modErr)
	}

	prog.complete()
	log.Println("knowledge-indexer: incremental index complete")
	return nil
}

// ─── Repo management ──────────────────────────────────────────────────────────

// ensureRepo verifies the repo exists on disk. The github-service is responsible
// for cloning and pulling — the knowledge-indexer only reads.
// Returns nil if the repo is present, error if not.
func ensureRepo(path, repoURL, token string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		// Repo exists on disk — ready to index. Don't pull (volume may be read-only
		// and the github-service handles updates).
		return nil
	}
	return fmt.Errorf("repository not found at %s — ensure the github-service has cloned it first", path)
}

// gitCurrentBranch returns the name of the branch currently checked out at
// the given path, or "main" if the branch cannot be resolved (detached
// HEAD, non-git directory, or any other error). The fallback keeps the
// indexer honest: every codex row it writes lands on a real branch, never
// on an empty string. If we can't tell, we say main — the spec's stated
// default.
func gitCurrentBranch(path string) string {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("knowledge-indexer: gitCurrentBranch(%s): %v — defaulting to main", path, err)
		return "main"
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		// HEAD means detached (e.g. a tag checkout); empty is implausible
		// but handled defensively.
		log.Printf("knowledge-indexer: gitCurrentBranch(%s) returned %q (detached HEAD?) — defaulting to main", path, branch)
		return "main"
	}
	return branch
}

// gitDiffFiles returns the list of files changed since the given ref.
func gitDiffFiles(path, since string) ([]string, error) {
	cmd := exec.Command("git", "-C", path, "diff", "--name-only", since, "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	var files []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// ─── Language detection ───────────────────────────────────────────────────────

func detectLanguages(repoPath string) ([]detectedLanguage, error) {
	indicators := map[string]string{
		"go.mod":           "Go",
		"package.json":     "JavaScript/TypeScript",
		"requirements.txt": "Python",
		"Cargo.toml":       "Rust",
		"pom.xml":          "Java",
		"build.gradle":     "Java/Kotlin",
		"*.gemspec":        "Ruby",
		"Gemfile":          "Ruby",
		"go.sum":           "Go",
		"pyproject.toml":   "Python",
		"setup.py":         "Python",
	}

	detected := make(map[string]bool)
	var result []detectedLanguage

	for indicator, lang := range indicators {
		var matched bool
		if strings.Contains(indicator, "*") {
			matches, _ := filepath.Glob(filepath.Join(repoPath, indicator))
			matched = len(matches) > 0
		} else {
			_, err := os.Stat(filepath.Join(repoPath, indicator))
			matched = err == nil
		}
		if matched && !detected[lang] {
			detected[lang] = true
		}
	}

	// Count files per extension to set primary language
	extCounts := make(map[string]int)
	_ = filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, "/.git/") || strings.Contains(path, "\\.git\\") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		extCounts[ext]++
		return nil
	})

	extToLang := map[string]string{
		".go":   "Go",
		".js":   "JavaScript/TypeScript",
		".ts":   "JavaScript/TypeScript",
		".tsx":  "JavaScript/TypeScript",
		".jsx":  "JavaScript/TypeScript",
		".py":   "Python",
		".rs":   "Rust",
		".java": "Java",
		".kt":   "Java/Kotlin",
		".rb":   "Ruby",
	}

	langCounts := make(map[string]int)
	for ext, cnt := range extCounts {
		if lang, ok := extToLang[ext]; ok {
			langCounts[lang] += cnt
		}
	}

	// Merge detected languages with counts
	allLangs := make(map[string]int)
	for lang := range detected {
		allLangs[lang] = langCounts[lang]
	}
	for lang, cnt := range langCounts {
		if cnt > 0 {
			allLangs[lang] = cnt
		}
	}

	// Find primary (most files)
	maxCount := 0
	primaryLang := ""
	for lang, cnt := range allLangs {
		if cnt > maxCount {
			maxCount = cnt
			primaryLang = lang
		}
	}

	for lang, cnt := range allLangs {
		result = append(result, detectedLanguage{
			Name:      lang,
			Primary:   lang == primaryLang,
			FileCount: cnt,
		})
	}

	if len(result) == 0 {
		result = append(result, detectedLanguage{Name: "Unknown", Primary: true, FileCount: 0})
	}

	return result, nil
}

func languageNames(langs []detectedLanguage) []string {
	names := make([]string, len(langs))
	for i, l := range langs {
		names[i] = l.Name
	}
	return names
}

// ─── File scanning ────────────────────────────────────────────────────────────

// scanFiles walks the repo and categorizes all files.
func scanFiles(repoPath string) ([]fileInfo, error) {
	var files []fileInfo
	err := filepath.Walk(repoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			// Skip hidden dirs, vendor, node_modules
			if name == ".git" || name == "vendor" || name == "node_modules" ||
				name == ".next" || name == "dist" || name == "build" ||
				name == "__pycache__" || name == ".pytest_cache" {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		fi := fileInfo{
			Path:     rel,
			Category: categorizeFile(rel),
			Language: detectFileLanguage(rel),
			Size:     info.Size(),
		}
		files = append(files, fi)
		return nil
	})
	return files, err
}

func categorizeFile(path string) string {
	lp := strings.ToLower(path)
	switch {
	case strings.Contains(lp, "_test.") || strings.Contains(lp, "/test/") ||
		strings.Contains(lp, "/tests/") || strings.HasSuffix(lp, "_test.go") ||
		strings.Contains(lp, ".test.") || strings.Contains(lp, ".spec."):
		return "test"
	case strings.HasSuffix(lp, ".md") || strings.HasSuffix(lp, ".rst") ||
		strings.HasSuffix(lp, ".txt") && strings.Contains(lp, "readme"):
		return "docs"
	case strings.HasSuffix(lp, ".yaml") || strings.HasSuffix(lp, ".yml") ||
		strings.HasSuffix(lp, ".toml") || strings.HasSuffix(lp, ".json") ||
		strings.HasSuffix(lp, ".env") || strings.HasSuffix(lp, ".ini") ||
		strings.HasSuffix(lp, ".cfg") || strings.Contains(lp, "dockerfile") ||
		strings.Contains(lp, "makefile"):
		return "config"
	default:
		return "source"
	}
}

func detectFileLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "Go"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JavaScript"
	case ".ts", ".tsx":
		return "TypeScript"
	case ".py":
		return "Python"
	case ".rs":
		return "Rust"
	case ".java":
		return "Java"
	case ".kt":
		return "Kotlin"
	case ".rb":
		return "Ruby"
	case ".c", ".h":
		return "C"
	case ".cpp", ".cc", ".hpp", ".cxx", ".hxx":
		return "C++"
	case ".cs":
		return "C#"
	case ".php":
		return "PHP"
	case ".html", ".htm":
		return "HTML"
	case ".css", ".scss", ".sass", ".less":
		return "CSS"
	case ".s", ".asm":
		return "Assembly"
	default:
		return "Other"
	}
}

// ─── AST-like module analysis ─────────────────────────────────────────────────

// analyzeModules reads source files and extracts structural information.
func analyzeModules(repoPath string, files []fileInfo, prog *IndexProgress) ([]moduleInfo, error) {
	// Group files by their directory (package/module)
	dirFiles := make(map[string][]fileInfo)
	for _, f := range files {
		if f.Category != "source" && f.Category != "test" {
			continue
		}
		dir := filepath.Dir(f.Path)
		dirFiles[dir] = append(dirFiles[dir], f)
	}

	var modules []moduleInfo
	for dir, dirFileList := range dirFiles {
		mi := moduleInfo{
			Name:   dir,
			Path:   dir,
			IsTest: false,
		}

		// A module is only "test-only" if ALL its files are tests.
		// Directories with both source and test files are source modules.
		hasSource := false
		for _, fi := range dirFileList {
			if fi.Category == "source" {
				hasSource = true
				break
			}
		}
		if !hasSource {
			mi.IsTest = true
		}

		for _, fi := range dirFileList {
			prog.setFile(fi.Path)
			if mi.Language == "" {
				mi.Language = fi.Language
			}

			absPath := filepath.Join(repoPath, fi.Path)
			content, err := os.ReadFile(absPath)
			if err != nil {
				continue
			}

			// Rust is handled by a lightweight inline extractor; all other
			// supported languages go through the deep langanalyzer package.
			if fi.Language == "Rust" {
				rustChunks, rustImports := extractRustSymbols(fi.Path, string(content))
				mi.Functions = append(mi.Functions, rustChunks...)
				mi.Imports = mergeUnique(mi.Imports, rustImports)
			} else {
				analysis := langanalyzer.AnalyzeFile(fi.Path, fi.Language, string(content))
				if analysis.Package != "" && mi.Name == dir {
					mi.Name = analysis.Package
				}
				if mi.Language == "" && analysis.Language != "" {
					mi.Language = analysis.Language
				}
				// Convert langanalyzer Symbols → functionChunks + type names.
				for _, sym := range analysis.Symbols {
					switch sym.Kind {
					case langanalyzer.KindFunction, langanalyzer.KindMethod:
						mi.Functions = append(mi.Functions, functionChunk{
							FilePath:  fi.Path,
							Package:   analysis.Package,
							Name:      sym.Name,
							Signature: sym.Signature,
							Body:      sym.Signature, // full signature provides embedding context
							StartLine: sym.Line,
						})
					case langanalyzer.KindClass, langanalyzer.KindStruct,
						langanalyzer.KindInterface, langanalyzer.KindEnum,
						langanalyzer.KindType:
						mi.Types = mergeUnique(mi.Types, []string{sym.Name})
					}
				}
				// Convert langanalyzer Imports → module import paths.
				for _, imp := range analysis.Imports {
					mi.Imports = mergeUnique(mi.Imports, []string{imp.Module})
				}
			}
		}

		if len(mi.Functions) > 0 || len(mi.Types) > 0 {
			modules = append(modules, mi)
		}
	}

	return modules, nil
}

// extractRustSymbols is a lightweight fallback for Rust files (not yet covered
// by the langanalyzer package). It mirrors the depth of the old extractFileSymbols
// for Rust so existing Rust-repo indexing is not regressed.
func extractRustSymbols(path, content string) ([]functionChunk, []string) {
	var chunks []functionChunk
	var imports []string
	lines := strings.Split(content, "\n")
	for lineNum, line := range lines {
		if m := reRustFn.FindStringSubmatch(line); m != nil {
			end := lineNum + 30
			if end > len(lines) {
				end = len(lines)
			}
			chunks = append(chunks, functionChunk{
				FilePath:  path,
				Name:      m[1],
				Signature: strings.TrimSpace(line),
				Body:      strings.Join(lines[lineNum:end], "\n"),
				StartLine: lineNum + 1,
			})
		}
	}
	rustUseRe := regexp.MustCompile(`(?m)^use\s+([^;]+);`)
	for _, m := range rustUseRe.FindAllStringSubmatch(content, -1) {
		imports = append(imports, m[1])
	}
	return chunks, imports
}

// ─── Dependency graph ─────────────────────────────────────────────────────────

// depGraph maps module name -> list of imported module names.
type depGraph map[string][]string

func buildDependencyGraph(modules []moduleInfo) depGraph {
	return buildDependencyGraphWith(modules, nil)
}

// buildDependencyGraphWith is buildDependencyGraph with otherModules, the
// names of known modules not analysed in this run (an incremental run's
// stored modules), also matched as import targets. Each module's
// dependencies are sorted, so the stored array does not change with map
// iteration order.
func buildDependencyGraphWith(modules []moduleInfo, otherModules []string) depGraph {
	graph := make(depGraph)
	// Build a set of known module paths for internal dep detection
	knownModules := make(map[string]string)
	for _, name := range otherModules {
		knownModules[name] = name
	}
	for _, m := range modules {
		knownModules[m.Path] = m.Name
		knownModules[m.Name] = m.Name
	}

	for _, m := range modules {
		seen := make(map[string]bool)
		for _, imp := range m.Imports {
			// Try to match against known modules
			for knownPath, knownName := range knownModules {
				if strings.Contains(imp, knownPath) || strings.HasSuffix(imp, knownName) {
					if !seen[knownName] && knownName != m.Name {
						graph[m.Name] = append(graph[m.Name], knownName)
						seen[knownName] = true
					}
				}
			}
		}
		sort.Strings(graph[m.Name])
	}
	return graph
}

// ─── Pattern detection ────────────────────────────────────────────────────────

func detectPatterns(files []fileInfo) []patternGroup {
	type matcher struct {
		pattern string
		match   func(base string) bool
	}
	matchers := []matcher{
		// Underscore-separated (Go, Python, Ruby)
		{"handler", func(b string) bool { return strings.Contains(b, "_handler.") || strings.Contains(b, ".handler.") }},
		{"repository", func(b string) bool {
			return strings.Contains(b, "_repository.") || strings.Contains(b, ".repository.") || strings.Contains(b, "_repo.") || strings.Contains(b, ".repo.")
		}},
		{"service", func(b string) bool { return strings.Contains(b, "_service.") || strings.Contains(b, ".service.") }},
		{"controller", func(b string) bool { return strings.Contains(b, "_controller.") || strings.Contains(b, ".controller.") }},
		{"middleware", func(b string) bool { return strings.Contains(b, "_middleware.") || strings.Contains(b, ".middleware.") }},
		{"model", func(b string) bool { return strings.Contains(b, "_model.") || strings.Contains(b, ".model.") }},
		{"test", func(b string) bool {
			return strings.Contains(b, "_test.") || strings.Contains(b, ".test.") || strings.Contains(b, ".spec.") || strings.Contains(b, "_spec.")
		}},
		{"mock", func(b string) bool {
			return strings.Contains(b, "_mock.") || strings.Contains(b, ".mock.") || strings.Contains(b, "__mocks__")
		}},
		{"client", func(b string) bool { return strings.Contains(b, "_client.") || strings.Contains(b, ".client.") }},
		{"config", func(b string) bool {
			return strings.Contains(b, "_config.") || strings.Contains(b, ".config.") || b == "config.ts" || b == "config.js" || b == "config.py"
		}},
		{"util", func(b string) bool {
			return strings.Contains(b, "_util.") || strings.Contains(b, ".util.") || strings.Contains(b, "_utils.") || strings.Contains(b, ".utils.") || b == "utils.ts" || b == "utils.js" || b == "helpers.ts" || b == "helpers.js"
		}},
		// TypeScript/React patterns
		{"hook", func(b string) bool {
			return strings.HasPrefix(b, "use") && (strings.HasSuffix(b, ".ts") || strings.HasSuffix(b, ".tsx"))
		}},
		{"component", func(b string) bool {
			if !strings.HasSuffix(b, ".tsx") && !strings.HasSuffix(b, ".jsx") {
				return false
			}
			if len(b) > 0 && b[0] >= 'A' && b[0] <= 'Z' {
				return true
			}
			return false
		}},
		{"route", func(b string) bool { return strings.Contains(b, "route") || strings.Contains(b, "router") }},
		{"store", func(b string) bool {
			return strings.Contains(b, "_store.") || strings.Contains(b, ".store.") || strings.Contains(b, "store.ts") || strings.Contains(b, "store.js")
		}},
		{"schema", func(b string) bool {
			return strings.Contains(b, "_schema.") || strings.Contains(b, ".schema.") || strings.Contains(b, "schema.ts")
		}},
		{"migration", func(b string) bool { return strings.Contains(b, "migration") || strings.Contains(b, "migrate") }},
		{"factory", func(b string) bool { return strings.Contains(b, "_factory.") || strings.Contains(b, ".factory.") }},
		{"type", func(b string) bool {
			return strings.Contains(b, ".types.") || strings.Contains(b, "_types.") || b == "types.ts" || b == "types.d.ts"
		}},
		{"index", func(b string) bool {
			return b == "index.ts" || b == "index.js" || b == "index.tsx" || b == "__init__.py"
		}},
		{"worker", func(b string) bool { return strings.Contains(b, "worker") }},
	}

	groups := make(map[string]*patternGroup)
	for _, m := range matchers {
		groups[m.pattern] = &patternGroup{Pattern: m.pattern}
	}

	for _, f := range files {
		base := strings.ToLower(filepath.Base(f.Path))
		for _, m := range matchers {
			if m.match(base) {
				groups[m.pattern].Files = append(groups[m.pattern].Files, f.Path)
				groups[m.pattern].Count++
			}
		}
	}

	var result []patternGroup
	for _, g := range groups {
		if g.Count > 0 {
			result = append(result, *g)
		}
	}
	return result
}

// ─── Git history analysis ─────────────────────────────────────────────────────

func analyzeGitHistory(repoPath string) ([]gitFileStats, error) {
	// Get commit counts per file from git log
	cmd := exec.Command("git", "-C", repoPath, "log",
		"--pretty=format:", "--name-only", "--no-renames",
		"-n", "500")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	fileCounts := make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			fileCounts[line]++
		}
	}

	// Get most recent modification dates
	cmd2 := exec.Command("git", "-C", repoPath, "log",
		"--pretty=format:%ae|%ad", "--date=short",
		"--name-only", "-n", "200")
	out2, err := cmd2.Output()
	if err != nil {
		// Non-fatal
		out2 = []byte{}
	}

	fileLastMod := make(map[string]string)
	fileContributors := make(map[string]map[string]bool)
	currentAuthor := ""
	currentDate := ""
	scanner2 := bufio.NewScanner(strings.NewReader(string(out2)))
	for scanner2.Scan() {
		line := strings.TrimSpace(scanner2.Text())
		if strings.Contains(line, "|") {
			parts := strings.SplitN(line, "|", 2)
			currentAuthor = parts[0]
			if len(parts) > 1 {
				currentDate = parts[1]
			}
		} else if line != "" && currentAuthor != "" {
			if _, ok := fileLastMod[line]; !ok {
				fileLastMod[line] = currentDate
			}
			if fileContributors[line] == nil {
				fileContributors[line] = make(map[string]bool)
			}
			fileContributors[line][currentAuthor] = true
		}
	}

	var stats []gitFileStats
	for path, count := range fileCounts {
		contribs := make([]string, 0)
		for c := range fileContributors[path] {
			contribs = append(contribs, c)
		}
		stats = append(stats, gitFileStats{
			Path:         path,
			CommitCount:  count,
			LastModified: fileLastMod[path],
			Contributors: contribs,
		})
	}
	return stats, nil
}

// ─── Vector indexing ──────────────────────────────────────────────────────────

// vectorIndexModules generates embeddings for all function chunks and upserts
// to Qdrant. Every point's payload is tagged with product_id / repo_id / branch
// so product-scoped searches (e.g. codex_similar_code) can filter hits to a
// single product and keep the benchmark contamination-free. An empty branch
// normalizes to "main" — matching codex.normalizeBranch.
func (s *server) vectorIndexModules(
	ctx context.Context,
	modules []moduleInfo,
	productID *uuid.UUID,
	repoID *uuid.UUID,
	branch string,
) error {
	if err := s.vs.EnsureCollection(ctx, s.vectorSize); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}
	if branch == "" {
		branch = "main"
	}

	var docs []vector.Document
	for _, m := range modules {
		for _, chunk := range m.Functions {
			doc, err := s.chunkToDocument(ctx, chunk, m.Language, productID, repoID, branch)
			if err != nil {
				log.Printf("knowledge-indexer: embed chunk %s.%s: %v — skipping", m.Name, chunk.Name, err)
				continue
			}
			docs = append(docs, doc)

			// Batch upsert every 50 documents
			if len(docs) >= 50 {
				if err := s.vs.Index(ctx, docs); err != nil {
					log.Printf("knowledge-indexer: vector index batch: %v", err)
				}
				docs = docs[:0]
			}
		}
	}

	if len(docs) > 0 {
		if err := s.vs.Index(ctx, docs); err != nil {
			log.Printf("knowledge-indexer: vector index final batch: %v", err)
		}
	}
	return nil
}

// reIndexModules deletes old vectors for changed files and re-inserts them.
// productID / repoID / branch flow through to the new payload so re-indexed
// chunks stay product-scoped.
func (s *server) reIndexModules(
	ctx context.Context,
	modules []moduleInfo,
	productID *uuid.UUID,
	repoID *uuid.UUID,
	branch string,
) error {
	if err := s.vs.EnsureCollection(ctx, s.vectorSize); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}

	// Collect IDs to delete (deterministic from file+function name)
	var idsToDelete []string
	for _, m := range modules {
		for _, chunk := range m.Functions {
			idsToDelete = append(idsToDelete, chunkID(chunk))
		}
	}
	if len(idsToDelete) > 0 {
		if err := s.vs.Delete(ctx, idsToDelete); err != nil {
			log.Printf("knowledge-indexer: delete old vectors: %v — continuing", err)
		}
	}

	return s.vectorIndexModules(ctx, modules, productID, repoID, branch)
}

// chunkBodyCapBytes caps the embedded body at 8 KiB so one enormous
// generated file doesn't push a chunk past the embedding model's limit.
// Past this cap we truncate on a line boundary and note it in the text.
const chunkBodyCapBytes = 8 * 1024

// chunkBodyCapLines caps the embedded body at 200 lines — same intent
// as the byte cap but catches chatty files with short lines.
const chunkBodyCapLines = 200

// chunkToDocument generates a vector.Document for a function chunk. The
// payload carries product_id / repo_id / branch so callers (codex_similar_code)
// can filter hits to the calling product's namespace. Missing IDs are simply
// omitted from the payload rather than written as the empty string, keeping
// the Qdrant filter semantics clean.
//
// The embedded content includes the header metadata AND the full function
// body (capped). Earlier versions embedded only the signature, which gave
// small models surface-level hits — enough to point at a file, not enough
// to write code against. The body field in metadata carries the same
// bounded text so codex_similar_code can return it directly.
func (s *server) chunkToDocument(
	ctx context.Context,
	chunk functionChunk,
	language string,
	productID *uuid.UUID,
	repoID *uuid.UUID,
	branch string,
) (vector.Document, error) {
	body := capBody(chunk.Body)
	content := fmt.Sprintf("// File: %s\n// Language: %s\n// Function: %s\n\n%s",
		chunk.FilePath, language, chunk.Name, body)

	embedding, err := s.generateEmbedding(ctx, content)
	if err != nil {
		return vector.Document{}, err
	}

	metadata := map[string]string{
		"file":     chunk.FilePath,
		"package":  chunk.Package,
		"function": chunk.Name,
		"language": language,
		"line":     fmt.Sprintf("%d", chunk.StartLine),
		"branch":   branch,
		// Keep the bounded body in payload so callers reading the hit get
		// the actual code, not just coordinates. Downstream tools can use
		// this without a follow-up codex_read_file call.
		"body": body,
	}
	if productID != nil {
		metadata["product_id"] = productID.String()
	}
	if repoID != nil {
		metadata["repo_id"] = repoID.String()
	}

	return vector.Document{
		ID:       chunkID(chunk),
		Content:  content,
		Vector:   embedding,
		Metadata: metadata,
	}, nil
}

// capBody truncates the function body to the smaller of
// chunkBodyCapBytes and chunkBodyCapLines, on a line boundary, appending
// a marker line when truncation happens.
func capBody(body string) string {
	if body == "" {
		return body
	}
	lines := strings.Split(body, "\n")
	if len(lines) > chunkBodyCapLines {
		lines = lines[:chunkBodyCapLines]
		lines = append(lines, "// … [truncated at line cap]")
	}
	body = strings.Join(lines, "\n")
	if len(body) > chunkBodyCapBytes {
		// Trim to last newline before the cap so we don't bisect a token.
		trimmed := body[:chunkBodyCapBytes]
		if idx := strings.LastIndex(trimmed, "\n"); idx > 0 {
			trimmed = trimmed[:idx]
		}
		body = trimmed + "\n// … [truncated at byte cap]"
	}
	return body
}

// warnOnThinChunks samples a small number of points from the Qdrant
// collection and logs a WARNING when the mean `content` length is below
// the signature-only threshold. This is how operators learn they need to
// re-index after we started embedding full bodies — the header text alone
// sits well below 300 chars.
func warnOnThinChunks(ctx context.Context, vs *vector.VectorStore) {
	sampleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	samples, err := vs.Sample(sampleCtx, 32)
	if err != nil {
		// Non-fatal — the warning is advisory, not load-bearing.
		log.Printf("knowledge-indexer: chunk-sanity sample skipped: %v", err)
		return
	}
	if len(samples) == 0 {
		return
	}
	total := 0
	for _, s := range samples {
		total += len(s.Content)
	}
	mean := total / len(samples)
	if mean < 300 {
		log.Printf(
			"knowledge-indexer: WARNING mean chunk content=%d chars across %d samples (<300 = signature-only). "+
				"Re-index is recommended so codex_similar_code returns full function bodies, not just signatures.",
			mean, len(samples),
		)
	}
}

// chunkID returns a deterministic UUID-like string ID for a function chunk.
func chunkID(chunk functionChunk) string {
	raw := fmt.Sprintf("%s::%s::%d", chunk.FilePath, chunk.Name, chunk.StartLine)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(raw)).String()
}

// generateEmbedding generates an embedding vector for the given text via
// the configured embedding provider. Returns an error if no embedding
// service is wired up or if the provider call fails — the indexer is
// useless without real embeddings, so it fails loud instead of falling back
// to a deterministic hash (which produced hits that scored high on exact
// duplicate text and meaningless on everything else).
func (s *server) generateEmbedding(ctx context.Context, text string) ([]float32, error) {
	embSvc := s.vs.EmbeddingService()
	if embSvc == nil {
		return nil, fmt.Errorf("knowledge-indexer: no embedding service configured (set a default in Admin → Providers → Embeddings)")
	}
	vec, err := embSvc.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("knowledge-indexer: embed: %w", err)
	}
	return vec, nil
}

// ─── Codex updates ────────────────────────────────────────────────────────────

// updateCodex writes architecture, module, and pattern entries to the Codex,
// tagging every row with the branch the indexer is currently scanning. An
// empty branch is normalized to "main" — matching codex.normalizeBranch —
// so a pre-branch-aware caller still lands on the main partition.
func (s *server) updateCodex(
	ctx context.Context,
	languages []detectedLanguage,
	modules []moduleInfo,
	patterns []patternGroup,
	deps depGraph,
	gitStats []gitFileStats,
	productID *uuid.UUID,
	repoID *uuid.UUID,
	branch string,
) error {
	if branch == "" {
		branch = "main"
	}
	// Architecture section — now carries product/repo/branch so it lands on
	// the correct (product_id, branch, name) partition. Previously this
	// wrote with NULL product_id and thus NULL branch partition, leaking
	// into the legacy/global namespace regardless of which product was
	// being indexed.
	archContent := buildArchitectureContent(languages, modules, gitStats)
	archJSON, _ := json.Marshal(archContent)
	archSection := &models.CodexSection{
		SectionType:  models.CodexSectionTypeArchitecture,
		Name:         "codebase_architecture",
		Content:      archJSON,
		ProductID:    productID,
		RepositoryID: repoID,
		Branch:       branch,
	}
	if err := s.cx.UpsertSection(ctx, archSection); err != nil {
		log.Printf("knowledge-indexer: upsert architecture section: %v", err)
	}

	// Module entries
	changedModules, err := s.updateCodexModules(ctx, modules, deps, productID, repoID, branch, nil)
	if err != nil {
		// The rows that did change are still announced.
		if productID != nil && len(changedModules) > 0 {
			if _, vErr := s.cx.RecordVersion(ctx, fullIndexVersion(*productID, branch, changedModules)); vErr != nil {
				log.Printf("knowledge-indexer: create codex version: %v", vErr)
			}
		}
		// Surface the error so callers (and eventually the UI) see a real
		// failure instead of silent "0 modules". This is Fail Honestly.
		return fmt.Errorf("update codex modules: %w", err)
	}

	// Pattern entries
	patternsUpserted := 0
	var patternErrs []string
	for _, pg := range patterns {
		if pg.Count < 2 {
			continue
		}
		keyRules := []string{
			fmt.Sprintf("Files matching this pattern: %d", pg.Count),
			fmt.Sprintf("Example files: %s", strings.Join(firstN(pg.Files, 3), ", ")),
		}
		refFiles := firstN(pg.Files, 5)
		pattern := &models.CodexPattern{
			Name:           fmt.Sprintf("naming_pattern_%s", pg.Pattern),
			Category:       "naming",
			WhenToUse:      fmt.Sprintf("When creating a new %s component", pg.Pattern),
			ReferenceFiles: refFiles,
			Rationale:      fmt.Sprintf("Detected %d files following the %s naming pattern", pg.Count, pg.Pattern),
			KeyRules:       keyRules,
			CommonMistakes: []string{"Inconsistent naming breaks discoverability"},
			Branch:         branch,
		}
		var err error
		if productID != nil && repoID != nil {
			err = s.cx.UpsertPatternForRepoBranch(ctx, *productID, *repoID, branch, pattern)
		} else {
			err = s.cx.UpsertPattern(ctx, pattern)
		}
		if err != nil {
			log.Printf("knowledge-indexer: upsert pattern %s: %v", pg.Pattern, err)
			patternErrs = append(patternErrs, fmt.Sprintf("%s: %v", pg.Pattern, err))
			continue
		}
		patternsUpserted++
	}
	// Track progress so the UI can display a real count.
	if progRef := s.progress; progRef != nil {
		progRef.addPatterns(patternsUpserted)
	}
	if len(patternErrs) > 0 {
		return fmt.Errorf("some patterns failed to upsert (%d total failures): %s", len(patternErrs), strings.Join(patternErrs, "; "))
	}

	// A new codex version: the architecture section and the modules whose
	// rows changed (fullIndexVersion), with its codex.indexed event.
	// product_id is required on codex_versions — without it the portal's
	// guards.CheckOnboardingComplete can't see the row and rejects submissions
	// for the product. The legacy single-repo index has none and writes none.
	if productID == nil {
		log.Printf("knowledge-indexer: refusing to create codex version without product_id")
		return nil
	}
	rec, err := s.cx.RecordVersion(ctx, fullIndexVersion(*productID, branch, changedModules))
	if err != nil {
		log.Printf("knowledge-indexer: create codex version: %v", err)
	} else {
		log.Printf("knowledge-indexer: product=%s branch=%s: codex v%d (%d of %d modules changed)", *productID, branch, rec.ProductVersion, len(changedModules), len(modules))
	}

	return nil
}

// storedRepoModules is the product's stored modules of repoID on branch
// (name and dependents only; exactly that branch, no default-branch
// fallback).
func (s *server) storedRepoModules(ctx context.Context, productID, repoID uuid.UUID, branch string) ([]*models.CodexModule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, dependents FROM codex_modules
		 WHERE product_id = $1 AND repository_id = $2 AND branch = $3
		 ORDER BY name`, productID, repoID, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.CodexModule
	for rows.Next() {
		m := &models.CodexModule{}
		if err := rows.Scan(&m.Name, &m.Dependents); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// updateCodexModules upserts module entries for the given modules. When
// productID + repoID are non-nil, the modules are tagged with the product/repo
// via UpsertModuleForRepoBranch so the dashboard's per-product queries find
// them. Nil pointers use the plain UpsertModule (legacy, non-product-scoped).
// Every row is tagged with `branch`; empty normalizes to "main".
//
// It returns the names of the modules whose rows changed
// (codex.UpsertModuleForRepoBranchChanged; every upserted module on the
// unscoped path), the module: entries of the run's codex version.
// existingDependents is nil for a full index; an incremental index passes
// the stored dependents, which the computed ones are added to
// (mergeDependents).
func (s *server) updateCodexModules(ctx context.Context, modules []moduleInfo, deps depGraph, productID, repoID *uuid.UUID, branch string, existingDependents map[string][]string) ([]string, error) {
	var changed []string
	if branch == "" {
		branch = "main"
	}
	log.Printf("knowledge-indexer: updating codex with %d modules (product=%v repo=%v branch=%s)", len(modules), productID, repoID, branch)
	upserted := 0
	var errs []string
	for _, m := range modules {
		if m.IsTest {
			log.Printf("knowledge-indexer: skipping test-only module %s", m.Name)
			continue
		}
		log.Printf("knowledge-indexer: upserting module %s (%d functions, %d types)", m.Name, len(m.Functions), len(m.Types))
		s.progress.setDetail(fmt.Sprintf("module %d/%d: %s", upserted+1, len(modules), m.Name))

		pubInterface, _ := json.Marshal(map[string]interface{}{
			"functions": functionNames(m.Functions),
			"types":     m.Types,
		})

		computedDependents := make([]string, 0)
		// Find modules that depend on this one
		for depModule, depList := range deps {
			for _, dep := range depList {
				if dep == m.Name {
					computedDependents = append(computedDependents, depModule)
					break
				}
			}
		}
		dependents := mergeDependents(existingDependents, m.Name, computedDependents)

		riskLevel := "low"
		if len(m.Functions) > 20 || len(deps[m.Name]) > 5 {
			riskLevel = "medium"
		}
		if len(dependents) > 5 {
			riskLevel = "high"
		}

		testNotes := "No test coverage detected"
		for _, testFn := range m.Functions {
			if strings.HasPrefix(testFn.Name, "Test") || strings.HasPrefix(testFn.Name, "test_") {
				testNotes = fmt.Sprintf("%d test functions detected", countTestFunctions(m.Functions))
				break
			}
		}

		moduleDeps := deps[m.Name]
		if moduleDeps == nil {
			moduleDeps = []string{}
		}
		if dependents == nil {
			dependents = []string{}
		}

		purpose := s.summarizeModulePurpose(ctx, m, moduleDeps, dependents)

		module := &models.CodexModule{
			Name:              m.Name,
			Purpose:           purpose,
			PublicInterface:   pubInterface,
			Dependencies:      moduleDeps,
			Dependents:        dependents,
			InternalPatterns:  m.Language,
			OwnerDomain:       inferDomain(m.Path),
			RiskLevel:         riskLevel,
			TestCoverageNotes: testNotes,
			Branch:            branch,
		}
		var err error
		rowChanged := true
		if productID != nil && repoID != nil {
			rowChanged, err = s.cx.UpsertModuleForRepoBranchChanged(ctx, *productID, *repoID, branch, module)
		} else {
			err = s.cx.UpsertModule(ctx, module)
		}
		if err != nil {
			log.Printf("knowledge-indexer: upsert module %s: %v", m.Name, err)
			errs = append(errs, fmt.Sprintf("%s: %v", m.Name, err))
			continue
		}
		upserted++
		if rowChanged {
			changed = append(changed, m.Name)
		}
	}
	// Track progress so the UI can display a real count instead of em-dash.
	if progRef := s.progress; progRef != nil {
		progRef.addModules(upserted)
	}
	log.Printf("knowledge-indexer: upserted %d/%d modules (%d changed)", upserted, len(modules), len(changed))
	if upserted == 0 && len(modules) > 0 {
		return changed, fmt.Errorf("no modules successfully upserted out of %d attempted: %s",
			len(modules), strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		return changed, fmt.Errorf("%d of %d modules failed to upsert: %s",
			len(errs), len(modules), strings.Join(errs, "; "))
	}
	return changed, nil
}

// buildArchitectureContent assembles the architecture section content map.
func buildArchitectureContent(
	languages []detectedLanguage,
	modules []moduleInfo,
	gitStats []gitFileStats,
) map[string]interface{} {
	langSummary := make([]map[string]interface{}, 0, len(languages))
	for _, l := range languages {
		langSummary = append(langSummary, map[string]interface{}{
			"name":       l.Name,
			"primary":    l.Primary,
			"file_count": l.FileCount,
		})
	}

	moduleNames := make([]string, 0, len(modules))
	for _, m := range modules {
		if !m.IsTest {
			moduleNames = append(moduleNames, m.Name)
		}
	}

	// Find hotspot files (most commits)
	hotspots := make([]string, 0)
	maxCount := 0
	for _, gs := range gitStats {
		if gs.CommitCount > maxCount {
			maxCount = gs.CommitCount
		}
	}
	threshold := maxCount / 2
	if threshold < 3 {
		threshold = 3
	}
	for _, gs := range gitStats {
		if gs.CommitCount >= threshold {
			hotspots = append(hotspots, gs.Path)
		}
	}

	return map[string]interface{}{
		"languages":     langSummary,
		"module_count":  len(moduleNames),
		"modules":       moduleNames,
		"hotspot_files": firstN(hotspots, 10),
		"indexed_at":    time.Now().UTC().Format(time.RFC3339),
	}
}

// ─── Publishing helpers ───────────────────────────────────────────────────────

func (s *server) publishProgress(ctx context.Context, prog *IndexProgress, productName, repoName string, productID *uuid.UUID) {
	snap := prog.snapshot()
	var pid string
	if productID != nil {
		pid = productID.String()
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"product_id":      pid,
		"product_name":    productName,
		"repo_name":       repoName,
		"phase":           snap.Phase,
		"detail":          snap.CurrentFile,
		"files_processed": snap.FilesProcessed,
		"total_files":     snap.TotalFiles,
		"modules_found":   snap.ModulesFound,
		"patterns_found":  snap.PatternsFound,
		"started_at":      snap.StartedAt.Format(time.RFC3339),
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	})
	if err := s.kb.Publish(ctx, bus.TopicIndexingProgress, serviceName, payload); err != nil {
		log.Printf("knowledge-indexer: publish indexing.progress: %v", err)
	}
}

// ─── Utility helpers ──────────────────────────────────────────────────────────

func mergeUnique(existing, additions []string) []string {
	seen := make(map[string]bool, len(existing))
	for _, s := range existing {
		seen[s] = true
	}
	result := append([]string{}, existing...)
	for _, s := range additions {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func functionNames(chunks []functionChunk) []string {
	names := make([]string, 0, len(chunks))
	for _, c := range chunks {
		names = append(names, c.Name)
	}
	return names
}

func countTestFunctions(chunks []functionChunk) int {
	n := 0
	for _, c := range chunks {
		if strings.HasPrefix(c.Name, "Test") || strings.HasPrefix(c.Name, "test_") ||
			strings.HasPrefix(c.Name, "Benchmark") {
			n++
		}
	}
	return n
}

func inferDomain(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, p := range parts {
		switch p {
		case "auth", "authentication", "authz", "authorization":
			return "auth"
		case "payment", "billing", "invoice":
			return "billing"
		case "api", "gateway", "handler":
			return "api"
		case "db", "database", "repository", "store":
			return "data"
		case "queue", "bus", "kafka", "event":
			return "messaging"
		case "config", "configuration":
			return "configuration"
		}
	}
	return "general"
}

// ─── Module purpose summarization ────────────────────────────────────────────

func (s *server) summarizeModulePurpose(ctx context.Context, m moduleInfo, deps, dependents []string) string {
	funcNames := make([]string, 0, len(m.Functions))
	signatures := make([]string, 0, len(m.Functions))
	for _, fn := range m.Functions {
		funcNames = append(funcNames, fn.Name)
		if fn.Signature != "" {
			signatures = append(signatures, fn.Signature)
		}
	}

	if s.llmSvc != nil {
		ctxMap := map[string]string{
			"question_topic": "module_purpose",
			"question_text":  "In 1-2 sentences, describe what this module does and its role in the codebase. Be specific — name the domain concepts it handles.",
			"module_name":    m.Name,
			"language":       m.Language,
			"function_count": fmt.Sprintf("%d", len(m.Functions)),
			"type_count":     fmt.Sprintf("%d", len(m.Types)),
			"functions":      strings.Join(firstNStrings(funcNames, 30), ", "),
			"types":          strings.Join(firstNStrings(m.Types, 20), ", "),
			"imports":        strings.Join(firstNStrings(m.Imports, 15), ", "),
			"dependencies":   strings.Join(deps, ", "),
			"dependents":     strings.Join(dependents, ", "),
		}
		if len(signatures) > 0 {
			ctxMap["signatures"] = strings.Join(firstNStrings(signatures, 15), "\n")
		}
		intent := strategy.Intent{
			Type:        strategy.IntentOnboardingQuestion,
			Context:     ctxMap,
			MaxTokens:   120,
			Temperature: 0.1,
		}
		res, err := s.llmSvc.Execute(ctx, intent)
		if err == nil && res != nil && strings.TrimSpace(res.Content) != "" {
			return strings.TrimSpace(res.Content)
		}
	}

	var roles []string
	for _, fn := range funcNames {
		lower := strings.ToLower(fn)
		switch {
		case strings.Contains(lower, "handler") || strings.Contains(lower, "serve"):
			roles = append(roles, "HTTP/API handling")
		case strings.Contains(lower, "parse") || strings.Contains(lower, "decode"):
			roles = append(roles, "data parsing")
		case strings.Contains(lower, "render") || strings.Contains(lower, "template"):
			roles = append(roles, "UI rendering")
		case strings.Contains(lower, "store") || strings.Contains(lower, "save") || strings.Contains(lower, "insert"):
			roles = append(roles, "data storage")
		case strings.Contains(lower, "auth") || strings.Contains(lower, "login") || strings.Contains(lower, "token"):
			roles = append(roles, "authentication")
		case strings.Contains(lower, "validate") || strings.Contains(lower, "check"):
			roles = append(roles, "validation")
		case strings.Contains(lower, "transform") || strings.Contains(lower, "convert") || strings.Contains(lower, "process"):
			roles = append(roles, "data processing pipeline")
		case strings.Contains(lower, "subscribe") || strings.Contains(lower, "publish") || strings.Contains(lower, "emit"):
			roles = append(roles, "event handling")
		case strings.Contains(lower, "connect") || strings.Contains(lower, "client") || strings.Contains(lower, "request"):
			roles = append(roles, "external service integration")
		case strings.Contains(lower, "config") || strings.Contains(lower, "setting"):
			roles = append(roles, "configuration management")
		case strings.Contains(lower, "test") || strings.Contains(lower, "assert"):
			roles = append(roles, "test utilities")
		case strings.Contains(lower, "migrate") || strings.Contains(lower, "schema"):
			roles = append(roles, "database migration")
		case strings.Contains(lower, "log") || strings.Contains(lower, "metric") || strings.Contains(lower, "trace"):
			roles = append(roles, "observability")
		case strings.Contains(lower, "route") || strings.Contains(lower, "middleware"):
			roles = append(roles, "request routing")
		case strings.Contains(lower, "cache") || strings.Contains(lower, "memo"):
			roles = append(roles, "caching")
		}
	}
	seen := make(map[string]bool)
	unique := roles[:0]
	for _, r := range roles {
		if !seen[r] {
			seen[r] = true
			unique = append(unique, r)
		}
	}
	if len(unique) == 0 {
		unique = append(unique, "general logic")
	}
	if len(unique) > 4 {
		unique = unique[:4]
	}
	return fmt.Sprintf("%s module (%s, %d functions, %d types). Responsibilities: %s.",
		m.Name, m.Language, len(m.Functions), len(m.Types), strings.Join(unique, ", "))
}

func firstNStrings(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ─── Deep analysis ──────────────────────────────────────────────────────────

type deepAnalysisQuestion struct {
	SectionName string
	SectionType models.CodexSectionType
	Prompt      string
}

var deepAnalysisQuestions = []deepAnalysisQuestion{
	{
		SectionName: "System Boundaries",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "Identify the system boundaries: where does this codebase's responsibility start and stop? What does it own vs. delegate to external services? Name specific integration points (databases, APIs, queues, file systems).",
	},
	{
		SectionName: "Core Invariants",
		SectionType: models.CodexSectionTypeDomain,
		Prompt:      "What are the core invariants this system must maintain? What conditions, if violated, would mean the system is fundamentally broken? Look for validation logic, constraint checks, and domain rules that are enforced across multiple modules.",
	},
	{
		SectionName: "Data Flow & Source of Truth",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "For each major data entity, where is the source of truth? Trace the data flow from ingestion to storage to retrieval. Identify any places where the same data is duplicated or cached, and how consistency is maintained.",
	},
	{
		SectionName: "Coupling Model",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "Analyze the coupling between modules. Which modules are tightly coupled (changes in one require changes in the other)? Which are loosely coupled? Identify the key interfaces/contracts that define module boundaries. Are there any circular dependencies?",
	},
	{
		SectionName: "Failure Modes",
		SectionType: models.CodexSectionTypeDomain,
		Prompt:      "What are the known failure modes? How does the system handle: network failures, database unavailability, invalid input, resource exhaustion, concurrent access conflicts? Identify error handling patterns and recovery mechanisms.",
	},
	{
		SectionName: "Scaling Bottlenecks",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "Identify potential scaling bottlenecks. What components are stateful vs. stateless? Where are the serialization points (locks, single-writer, sequential processing)? What would break first under 10x load?",
	},
	{
		SectionName: "Trust Boundaries",
		SectionType: models.CodexSectionTypeDomain,
		Prompt:      "Map the trust boundaries. Where does user input enter the system? Where is authentication/authorization enforced? What data crosses trust boundaries without validation? Identify the security-critical code paths.",
	},
	{
		SectionName: "Architectural Decisions",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "What architectural decisions are baked in and would be expensive to change? Identify choices in: data storage technology, communication patterns (sync vs async), deployment model, language/framework selection, and API design that constrain future evolution.",
	},
	{
		SectionName: "Complexity Map",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "Where does complexity concentrate? Identify the most complex modules/files and explain WHY they are complex (business logic density, many integrations, state management, concurrency). Distinguish essential complexity from accidental complexity.",
	},
	{
		SectionName: "State Lifecycle",
		SectionType: models.CodexSectionTypeDomain,
		Prompt:      "For each stateful entity, describe its lifecycle: creation, valid state transitions, and termination/cleanup. Identify any state machines (explicit or implicit). Where is state persisted vs. held in memory? What happens to in-flight state during restarts?",
	},
	{
		SectionName: "Observability",
		SectionType: models.CodexSectionTypeProcess,
		Prompt:      "How would you know if this system is healthy or degraded? Identify: logging patterns and what gets logged, metrics collection points, health check endpoints, alerting hooks. What failure scenarios would be invisible to current monitoring?",
	},
	{
		SectionName: "High-Impact Change Points",
		SectionType: models.CodexSectionTypeArchitecture,
		Prompt:      "Identify the files and modules where a small change could have outsized impact (positive or negative). These are the leverage points — shared utilities, core abstractions, database schemas, API contracts, configuration. Explain the blast radius of changes to each.",
	},
}

func (s *server) countDeepAnalysisSections(ctx context.Context, productID uuid.UUID) int {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM codex_sections WHERE product_id = $1 AND content::text LIKE '%"source": "deep_analysis"%'`,
		productID,
	).Scan(&count)
	if err != nil {
		return 0
	}
	return count
}

func (s *server) runDeepAnalysis(
	ctx context.Context,
	repoPath string,
	modules []moduleInfo,
	deps depGraph,
	files []fileInfo,
	gitStats []gitFileStats,
	productID uuid.UUID,
	repoID *uuid.UUID,
	branch string,
) error {
	log.Printf("knowledge-indexer: starting deep analysis — %d questions, %d modules, %d files", len(deepAnalysisQuestions), len(modules), len(files))

	var ctxBuilder strings.Builder
	ctxBuilder.WriteString("# Codebase Structure\n\n")

	ctxBuilder.WriteString("## File Tree\n")
	for i, f := range files {
		if i >= 200 {
			fmt.Fprintf(&ctxBuilder, "... and %d more files\n", len(files)-200)
			break
		}
		fmt.Fprintf(&ctxBuilder, "- %s (%s, %s)\n", f.Path, f.Language, f.Category)
	}

	ctxBuilder.WriteString("\n## Modules\n")
	for _, m := range modules {
		depList := deps[m.Name]
		fmt.Fprintf(&ctxBuilder, "\n### %s (%s)\n", m.Name, m.Language)
		fmt.Fprintf(&ctxBuilder, "Functions (%d): ", len(m.Functions))
		names := make([]string, 0, len(m.Functions))
		for _, fn := range m.Functions {
			names = append(names, fn.Name)
		}
		ctxBuilder.WriteString(strings.Join(firstNStrings(names, 25), ", "))
		ctxBuilder.WriteString("\n")
		if len(m.Types) > 0 {
			fmt.Fprintf(&ctxBuilder, "Types: %s\n", strings.Join(firstNStrings(m.Types, 15), ", "))
		}
		if len(m.Imports) > 0 {
			fmt.Fprintf(&ctxBuilder, "Imports: %s\n", strings.Join(firstNStrings(m.Imports, 10), ", "))
		}
		if len(depList) > 0 {
			fmt.Fprintf(&ctxBuilder, "Depends on: %s\n", strings.Join(depList, ", "))
		}
	}

	ctxBuilder.WriteString("\n## Git Activity (most changed files)\n")
	sortedStats := append([]gitFileStats(nil), gitStats...)
	for i := 0; i < len(sortedStats); i++ {
		for j := i + 1; j < len(sortedStats); j++ {
			if sortedStats[j].CommitCount > sortedStats[i].CommitCount {
				sortedStats[i], sortedStats[j] = sortedStats[j], sortedStats[i]
			}
		}
	}
	for i, gs := range sortedStats {
		if i >= 15 {
			break
		}
		fmt.Fprintf(&ctxBuilder, "- %s: %d commits, last modified %s\n", gs.Path, gs.CommitCount, gs.LastModified)
	}

	ctxBuilder.WriteString("\n## Source Code Samples\n")
	var sampleBytes int
	const maxSampleBytes = 32 * 1024
	for _, m := range modules {
		if sampleBytes >= maxSampleBytes {
			break
		}
		for _, fn := range m.Functions {
			if fn.Body == "" || len(fn.Body) < 20 {
				continue
			}
			if sampleBytes >= maxSampleBytes {
				break
			}
			chunk := fn.Body
			if len(chunk) > 2000 {
				chunk = chunk[:2000] + "\n// ... truncated"
			}
			fmt.Fprintf(&ctxBuilder, "\n```%s\n// %s — %s\n%s\n```\n", strings.ToLower(m.Language), m.Name, fn.Name, chunk)
			sampleBytes += len(chunk)
		}
	}

	codebaseContext := ctxBuilder.String()
	if len(codebaseContext) > 48*1024 {
		codebaseContext = codebaseContext[:48*1024] + "\n... (context truncated)"
	}

	var completed int
	for i, q := range deepAnalysisQuestions {
		s.progress.setDetail(fmt.Sprintf("question %d/%d: %s", i+1, len(deepAnalysisQuestions), q.SectionName))
		intent := strategy.Intent{
			Type: strategy.IntentOnboardingQuestion,
			Context: map[string]string{
				"question_topic": "deep_analysis",
				"question_text":  q.Prompt,
				"codebase":       codebaseContext,
			},
			MaxTokens:   800,
			Temperature: 0.15,
		}
		res, err := s.llmSvc.Execute(ctx, intent)
		if err != nil {
			log.Printf("knowledge-indexer: deep analysis question %q failed: %v — skipping", q.SectionName, err)
			continue
		}
		if res == nil || strings.TrimSpace(res.Content) == "" {
			log.Printf("knowledge-indexer: deep analysis question %q returned empty — skipping", q.SectionName)
			continue
		}

		content, _ := json.Marshal(map[string]string{
			"analysis": strings.TrimSpace(res.Content),
			"source":   "deep_analysis",
		})
		section := &models.CodexSection{
			SectionType:  q.SectionType,
			Name:         q.SectionName,
			Content:      content,
			ProductID:    &productID,
			RepositoryID: repoID,
			Branch:       branch,
		}
		if err := s.cx.UpsertSection(ctx, section); err != nil {
			log.Printf("knowledge-indexer: deep analysis upsert %q failed: %v", q.SectionName, err)
			continue
		}
		completed++
		log.Printf("knowledge-indexer: deep analysis [%d/%d] %s — stored", completed, len(deepAnalysisQuestions), q.SectionName)
	}

	log.Printf("knowledge-indexer: deep analysis complete — %d/%d questions answered", completed, len(deepAnalysisQuestions))
	return nil
}

// ─── JSON response helper ─────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Configuration ──────────────────────────────────────────────────────────

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("knowledge-indexer: load config: %v", err)
	}

	// ── Database ───────────────────────────────────────────────────────────────

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("knowledge-indexer: connect db: %v", err)
	}
	defer pool.Close()

	// ── Kafka ──────────────────────────────────────────────────────────────────

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("knowledge-indexer: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// ── Domain services ────────────────────────────────────────────────────────

	cx := codex.New(pool)
	ps := products.NewStore(pool)

	// ── Embedding provider (DB-configured) ───────────────────────────────────
	//
	// Resolve the default embedding provider from the embedding_providers
	// table. The indexer has NO useful work without real embeddings — the
	// pseudo-embedding fallback that used to live here was a footgun that
	// wrote hash-based vectors into Qdrant and corrupted every similarity
	// search for months. We now fail loudly: an admin must configure a
	// provider in Admin → Providers → Embeddings before the indexer boots.
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		log.Fatalf("knowledge-indexer: MASTER_ENCRYPTION_KEY is not set — cannot decrypt embedding provider credentials")
	}
	masterKey, err := crypto.LoadMasterKey(masterKeyStr)
	if err != nil {
		log.Fatalf("knowledge-indexer: load master key: %v", err)
	}
	cs := configstore.New(pool, masterKey)
	cs.StartWatcher(ctx)

	embSvc, declaredDim, err := vector.BuildEmbeddingService(ctx, pool, cs)
	if err != nil {
		log.Fatalf("knowledge-indexer: %v", err)
	}

	// Probe the provider to learn the ACTUAL dimension it produces. The
	// row's `dimension` column is populated by the admin UI's Test button,
	// but we never trust DB metadata over a live probe when we can cheaply
	// verify. Boot-time probe also fails fast if the provider is
	// mis-configured (unreachable, wrong model name, bad API key).
	probeCtx, probeCancel := context.WithTimeout(ctx, 15*time.Second)
	liveDim, err := embSvc.Dimension(probeCtx)
	probeCancel()
	if err != nil {
		log.Fatalf("knowledge-indexer: embedding provider probe failed (provider=%s model=%s): %v",
			embSvc.ProviderType(), embSvc.Model(), err)
	}
	if declaredDim != 0 && declaredDim != liveDim {
		log.Printf("knowledge-indexer: WARNING embedding_providers.dimension=%d but provider returned %d — using live value", declaredDim, liveDim)
	}
	vectorSize := liveDim
	log.Printf("knowledge-indexer: embedding provider ready (type=%s model=%s dim=%d)",
		embSvc.ProviderType(), embSvc.Model(), vectorSize)

	vs := vector.NewWithEmbeddings(cfg.Qdrant.URL, vectorCollection, embSvc)

	// Validate that the existing Qdrant collection (if any) matches our
	// embedding dimension. A mismatch means the admin swapped providers
	// without re-indexing: old points were written with (say) 1536-D
	// OpenAI vectors, the new provider emits 768-D nomic vectors, and
	// searches would silently return garbage. Refuse to start so the
	// operator sees the problem and either re-indexes or picks a
	// matching provider.
	collCtx, collCancel := context.WithTimeout(ctx, 10*time.Second)
	info, err := vs.DescribeCollection(collCtx)
	collCancel()
	if err != nil {
		log.Fatalf("knowledge-indexer: describe Qdrant collection: %v", err)
	}
	if info.Exists && info.VectorSize != vectorSize {
		log.Fatalf(
			"knowledge-indexer: embedding dimension mismatch: provider=%d, qdrant collection '%s'=%d. "+
				"Re-index is required. Delete the '%s' collection in Qdrant or configure a matching embedding provider.",
			vectorSize, vectorCollection, info.VectorSize, vectorCollection,
		)
	}
	if !info.Exists {
		ensureCtx, ensureCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := vs.EnsureCollection(ensureCtx, vectorSize); err != nil {
			ensureCancel()
			log.Fatalf("knowledge-indexer: create Qdrant collection: %v", err)
		}
		ensureCancel()
		log.Printf("knowledge-indexer: created Qdrant collection '%s' with dim=%d", vectorCollection, vectorSize)
	} else {
		// Chunk-size sentinel: the content field used to carry only the
		// function signature (< 300 chars on average). Now we embed the
		// full bounded body. When the existing collection still looks
		// "thin" the operator needs to re-index so searches don't return
		// signature-only hits.
		go warnOnThinChunks(ctx, vs)
	}

	// Build the LLM service from the credential registry (same store the
	// admin dashboard writes to). Falls back to env-var config if the
	// registry has no LLM credentials yet.
	llmSvc, err := llm.NewServiceFromRegistry(cs, "knowledge-indexer", "instance-1", pool)
	if err != nil {
		// Fallback: try env-var config (MODEL_PROVIDER etc.)
		llmSvc, err = llm.NewService(&cfg.Model, pool)
		if err != nil {
			log.Println("knowledge-indexer: LLM not configured. Configure a model provider in the admin dashboard (Admin → Models).")
		}
	}
	if llmSvc != nil {
		log.Println("knowledge-indexer: LLM service ready — deep analysis enabled")
	}

	// ── Health ─────────────────────────────────────────────────────────────────

	checker := health.New(serviceName)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())
	checker.AddCheck("qdrant", func() error {
		checkCtx, checkCancel := context.WithTimeout(ctx, 3*time.Second)
		defer checkCancel()
		return vs.EnsureCollection(checkCtx, vectorSize)
	})

	// ── Server setup ───────────────────────────────────────────────────────────

	// codex.updated, codex.update.from_interaction and the commit-linked
	// extract requests are enqueued in the outbox; this dispatcher delivers
	// them. A start failure is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), serviceName)
	if err != nil {
		log.Fatalf("knowledge-indexer: start outbox dispatcher: %v", err)
	}

	svc := newServer(cfg, pool, kb, cx, vs, llmSvc, ps, vectorSize)

	// ── Kafka subscriptions ────────────────────────────────────────────────────

	topics := []string{bus.TopicCodexUpdated, bus.TopicExternalChangeDetected}
	// M1-05: deduplicate on the processed-event ledger. The handler starts
	// an asynchronous index run and returns, so the event is recorded on
	// receipt (Ledger.Handler records after a handler with no guarded
	// transaction); a recorded duplicate no longer starts a second run.
	if err := kb.SubscribeMultiple(topics, consumerGroup, idempotency.NewLedger(pool).Handler(serviceName, svc.handleKafkaMessage)); err != nil {
		log.Fatalf("knowledge-indexer: subscribe kafka topics: %v", err)
	}
	log.Printf("knowledge-indexer: subscribed to Kafka topics: %v", topics)

	// ── Unified Interaction Surface consumers ─────────────────────────────────
	// Three handlers land the user-answer → triage → codex + qdrant + revoke
	// cascade. Their code exists in cmd/knowledge-indexer/*.go (Tasks 12/14/15);
	// this block starts them. interaction_wiring.go holds the glue helpers.
	interactionStore := interactions.NewStore(pool)
	triageLLM := newTriageFromLLMService(llmSvc)
	triage := interactions.NewTriage(triageLLM, codexPromotionThreshold())
	userDecisionStore := NewUserDecisionStore(cfg.Qdrant.URL, vs.EmbeddingService())
	codexFromInteractionPub := &outboxCodexPromoter{db: pool, store: interactionStore}

	answeredConsumer := NewAnsweredConsumer(interactionStore, userDecisionStore, codexFromInteractionPub, triage)
	codexFromInteractionHandler := NewCodexFromInteractionHandler(pool)
	revokedConsumer := NewRevokedConsumer(pool, cfg.Qdrant.URL)

	if err := kb.Subscribe(bus.TopicUserPromptAnswered, "knowledge-indexer-answered",
		func(ctx context.Context, m *bus.Message) error { return answeredConsumer.Handle(ctx, m) }); err != nil {
		log.Fatalf("knowledge-indexer: subscribe user_prompt.answered: %v", err)
	}
	if err := kb.Subscribe(bus.TopicCodexUpdateFromInteraction, "knowledge-indexer-codex-from-interaction",
		func(ctx context.Context, m *bus.Message) error { return codexFromInteractionHandler.Handle(ctx, m) }); err != nil {
		log.Fatalf("knowledge-indexer: subscribe codex.update.from_interaction: %v", err)
	}
	if err := kb.Subscribe(bus.TopicUserPromptRevoked, "knowledge-indexer-revoked",
		func(ctx context.Context, m *bus.Message) error { return revokedConsumer.Handle(ctx, m) }); err != nil {
		log.Fatalf("knowledge-indexer: subscribe user_prompt.revoked: %v", err)
	}
	log.Printf("knowledge-indexer: subscribed to interaction topics: [%s %s %s]",
		bus.TopicUserPromptAnswered, bus.TopicCodexUpdateFromInteraction, bus.TopicUserPromptRevoked)

	// ── Commit-linked codex orchestrator ──────────────────────────────────────
	clo := newCommitLinkedOrchestrator(pool, kb)
	if err := clo.Start(ctx); err != nil {
		log.Fatalf("knowledge-indexer: commit-linked orchestrator start: %v", err)
	}
	log.Printf("knowledge-indexer: commit-linked codex orchestrator started")

	// ── HTTP mux ───────────────────────────────────────────────────────────────

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/api/index/full", svc.fullIndexHandler)
	mux.HandleFunc("/api/index/incremental", svc.incrementalIndexHandler)
	mux.HandleFunc("/api/index/status", svc.statusHandler)

	addr := fmt.Sprintf(":%d", servicePort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("knowledge-indexer: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("knowledge-indexer: server error: %v", err)
		}
	}()

	// ── Graceful shutdown ──────────────────────────────────────────────────────

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("knowledge-indexer: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("knowledge-indexer: shutdown error: %v", err)
	}
	dispatcher.Wait()
	log.Println("knowledge-indexer: stopped")
}
