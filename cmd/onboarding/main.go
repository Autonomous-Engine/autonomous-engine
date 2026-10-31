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
	"os/signal"
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
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	jirapkg "github.com/autonomousengine/autonomous-engine/internal/jira"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	serviceName    = "onboarding"
	onboardingPort = 8086
)

// ─── Onboarding session types ────────────────────────────────────────────────

// OnboardingPhase represents the current stage of the onboarding process.
type OnboardingPhase string

const (
	PhaseInit            OnboardingPhase = "init"
	PhaseConversation    OnboardingPhase = "conversation"
	PhaseAnalysis        OnboardingPhase = "analysis"
	PhaseModelEvaluation OnboardingPhase = "model_evaluation"
	PhaseCodexApproval   OnboardingPhase = "codex_approval"
	PhaseValidation      OnboardingPhase = "validation"
	PhaseComplete        OnboardingPhase = "complete"
)

// onboardingSession holds state for a single onboarding run.
//
// ProductID is captured at approve-codex time (the earliest point we
// reliably have it in the onboarding flow) and is used downstream to
// scope validation work items so they surface on per-product dashboards
// and run product-scoped guards — see audit finding A.9.
type onboardingSession struct {
	mu           sync.RWMutex
	SessionID    string            `json:"session_id"`
	Phase        OnboardingPhase   `json:"phase"`
	Progress     int               `json:"progress"` // 0-100
	StartedAt    time.Time         `json:"started_at"`
	CodexDraft   map[string]string `json:"codex_draft"`
	Approved     bool              `json:"approved"`
	ValidationWI []string          `json:"validation_work_items"`
	ProductID    uuid.UUID         `json:"product_id"`
}

func newSession() *onboardingSession {
	return &onboardingSession{
		SessionID:  uuid.New().String(),
		Phase:      PhaseInit,
		Progress:   0,
		StartedAt:  time.Now().UTC(),
		CodexDraft: make(map[string]string),
	}
}

// ─── Connectivity result types ────────────────────────────────────────────────

type serviceCheckResult struct {
	Connected bool   `json:"connected"`
	Details   string `json:"details"`
}

type connectivityResponse struct {
	Results    map[string]serviceCheckResult `json:"results"`
	CanProceed bool                          `json:"can_proceed"`
	Blockers   []string                      `json:"blockers"`
	Warnings   []string                      `json:"warnings"`
}

// prerequisites holds a snapshot of readiness state used by status and gate checks.
type prerequisites struct {
	LLMConfigured   bool         `json:"llm_configured"`
	GitHubConnected bool         `json:"github_connected"`
	JiraConnected   bool         `json:"jira_connected"`
	RepoCloned      bool         `json:"repo_cloned"`
	CodexPopulated  bool         `json:"codex_populated"`
	VectorIndexed   bool         `json:"vector_indexed"`
	RepoCount       int          `json:"repo_count"`
	Repos           []repoStatus `json:"repos,omitempty"`
}

// repoStatus carries per-repo clone and index status.
type repoStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CloneOK   bool   `json:"clone_ok"`
	IndexedAt string `json:"indexed_at,omitempty"`
}

// ─── Validation work item request ────────────────────────────────────────────

type validationRequest struct {
	WorkItems []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Type        string `json:"type"`
	} `json:"work_items"`
}

// ─── WebSocket conversation types ────────────────────────────────────────────

// wsIncoming is a message sent by the user through the WebSocket.
type wsIncoming struct {
	Action  string `json:"action"` // "answer" | "skip"
	Payload string `json:"payload"`
}

// wsOutgoing is a message sent by the server to the client.
type wsOutgoing struct {
	Type    string            `json:"type"` // "question" | "update" | "error" | "complete"
	Payload string            `json:"payload"`
	Phase   string            `json:"phase,omitempty"`
	Draft   map[string]string `json:"draft,omitempty"`
}

// onboardingQuestion describes a single question in the conversation flow.
type onboardingQuestion struct {
	key   string
	topic string
	text  string
}

// orderedQuestions defines the full interactive onboarding conversation.
var orderedQuestions = []onboardingQuestion{
	{key: "architecture_overview", topic: "architecture_confirmation", text: "Please describe your high-level system architecture. What are the main services, their responsibilities, and how they communicate?"},
	{key: "tech_stack", topic: "architecture_confirmation", text: "What is your primary technology stack? (languages, frameworks, databases, cloud provider)"},
	{key: "patterns_discovered", topic: "pattern_discovery", text: "What coding patterns or conventions does your team consistently follow? (e.g., repository pattern, event sourcing, specific naming conventions)"},
	{key: "anti_patterns", topic: "anti_pattern_collection", text: "Are there any patterns or approaches your team explicitly avoids? Why were these decisions made?"},
	{key: "branching_strategy", topic: "process_discovery", text: "Describe your Git branching strategy. How are features, hotfixes, and releases managed?"},
	{key: "pr_process", topic: "process_discovery", text: "What is your pull request process? Who reviews, what are the required checks, and how are merges handled?"},
	{key: "deployment_process", topic: "process_discovery", text: "How does your deployment pipeline work? What environments exist and what are the promotion criteria?"},
	{key: "domain_knowledge", topic: "domain_knowledge", text: "What are the core domain concepts an engineer must understand to contribute effectively? Any ubiquitous language or bounded contexts?"},
	{key: "critical_constraints", topic: "domain_knowledge", text: "Are there any regulatory, compliance, or business constraints that affect how code must be written or deployed?"},
	{key: "testing_philosophy", topic: "testing_philosophy", text: "What is your testing philosophy? What types of tests are required (unit, integration, e2e)? What coverage expectations exist?"},
	{key: "test_conventions", topic: "testing_philosophy", text: "Are there specific testing frameworks, utilities, or patterns your team uses? Any patterns to avoid in tests?"},
	{key: "autonomous_scope", topic: "trust_and_scope", text: "What types of changes should the autonomous system be allowed to make without human review? (e.g., bug fixes, refactoring, new features)"},
	{key: "review_triggers", topic: "trust_and_scope", text: "What conditions should always trigger a human review request? (e.g., changes to auth, database schema, public APIs)"},
	{key: "risk_zones", topic: "trust_and_scope", text: "Which parts of the codebase are highest risk and require extra caution? (e.g., payment processing, security modules)"},
}

// ─── Server ───────────────────────────────────────────────────────────────────

// server holds all dependencies for the onboarding service.
type server struct {
	cfg             *config.Config
	pool            *pgxpool.Pool
	configStore     *configstore.ConfigStore
	kb              bus.MessageBus
	wg              *workgraph.WorkGraph
	cx              *codex.Codex
	vs              *vector.VectorStore
	llmSvc          *llm.Service
	productStore    *products.ProductStore
	onboardingStore *onboarding.Store
	// policyStore overrides the verification-policy backend (tests); nil
	// means the database-backed one (see verification_policy.go).
	policyStore      policyBackend
	githubServiceURL string
	session          *onboardingSession
	sessionMu        sync.RWMutex
	upgrader         websocket.Upgrader
}

func newServer(
	cfg *config.Config,
	pool *pgxpool.Pool,
	configStore *configstore.ConfigStore,
	kb bus.MessageBus,
	wg *workgraph.WorkGraph,
	cx *codex.Codex,
	vs *vector.VectorStore,
	llmSvc *llm.Service,
	ps *products.ProductStore,
	githubServiceURL string,
	obStore *onboarding.Store,
) *server {
	return &server{
		cfg:              cfg,
		pool:             pool,
		configStore:      configStore,
		kb:               kb,
		wg:               wg,
		cx:               cx,
		vs:               vs,
		llmSvc:           llmSvc,
		productStore:     ps,
		onboardingStore:  obStore,
		githubServiceURL: githubServiceURL,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

// ─── Prerequisite helpers ─────────────────────────────────────────────────────

const workspaceRepo = "/workspace/repo"

// isRepoCloned reports whether a git repository is present at workspaceRepo.
// If the directory is missing or empty it attempts to trigger a clone via the
// github-service before returning the final verdict.
func (s *server) isRepoCloned(ctx context.Context) bool {
	// Fast path: .git directory exists.
	if info, err := os.Stat(workspaceRepo + "/.git"); err == nil && info.IsDir() {
		return true
	}

	// Attempt to trigger a clone through the github-service so that a missing
	// repo is recovered automatically during onboarding.
	if s.githubServiceURL != "" && s.cfg.GitHub.Repo != "" {
		repoURL := fmt.Sprintf("https://github.com/%s", s.cfg.GitHub.Repo)
		payload, _ := json.Marshal(map[string]string{
			"repo_url": repoURL,
			"branch":   s.cfg.GitHub.BaseBranch,
		})
		cloneURL := strings.TrimRight(s.githubServiceURL, "/") + "/clone"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cloneURL, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			cloneCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			req = req.WithContext(cloneCtx)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close() //nolint:errcheck
			} else {
				log.Printf("onboarding: trigger clone via github-service: %v", err)
			}
		}
	}

	// Re-check after the clone attempt.
	info, err := os.Stat(workspaceRepo + "/.git")
	return err == nil && info.IsDir()
}

// gatherPrerequisites collects the current readiness state for all gates.
// If productID is provided, repo statuses are gathered for that product.
func (s *server) gatherPrerequisites(ctx context.Context, productID *uuid.UUID) prerequisites {
	p := prerequisites{}

	// LLM configured and reachable.
	if s.llmSvc != nil {
		if err := s.llmSvc.HealthCheck(ctx); err == nil {
			p.LLMConfigured = true
		}
	}

	// GitHub credentials present.
	if s.cfg.GitHub.Token != "" {
		p.GitHubConnected = true
	}

	// Jira credentials present.
	if s.cfg.Jira.URL != "" && s.cfg.Jira.APIToken != "" {
		p.JiraConnected = true
	}

	// Per-product repo status.
	if productID != nil && s.productStore != nil {
		repos, err := s.productStore.GetRepositoriesForProduct(ctx, *productID)
		if err == nil {
			p.RepoCount = len(repos)
			for _, r := range repos {
				clonePath := r.ClonePath
				cloneOK := false
				if info, statErr := os.Stat(clonePath + "/.git"); statErr == nil && info.IsDir() {
					cloneOK = true
				}
				rs := repoStatus{
					ID:      r.ID.String(),
					Name:    r.Name,
					CloneOK: cloneOK,
				}
				if r.LastIndexedAt != nil {
					rs.IndexedAt = r.LastIndexedAt.Format(time.RFC3339)
				}
				p.Repos = append(p.Repos, rs)
				if cloneOK {
					p.RepoCloned = true // at least one repo cloned
				}
			}
		}
	} else {
		// Legacy single-repo fallback.
		if info, err := os.Stat(workspaceRepo + "/.git"); err == nil && info.IsDir() {
			p.RepoCloned = true
		}
	}

	// Codex populated (modules + patterns).
	if s.pool != nil {
		var moduleCount, patternCount int
		_ = s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM codex_modules").Scan(&moduleCount)
		_ = s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM codex_patterns").Scan(&patternCount)
		p.CodexPopulated = moduleCount > 0 && patternCount > 0
	}

	// Vector store populated.
	if s.vs != nil {
		if count, err := s.vs.CountPoints(ctx); err == nil && count > 0 {
			p.VectorIndexed = true
		}
	}

	return p
}

// ─── REST handlers ─────────────────────────────────────────────────────────

// startHandler creates a new onboarding session.
// POST /api/onboarding/start
func (s *server) startHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.sessionMu.Lock()
	sess := newSession()
	s.session = sess
	s.sessionMu.Unlock()

	log.Printf("onboarding: started session %s", sess.SessionID)

	writeJSON(w, http.StatusCreated, map[string]string{
		"session_id": sess.SessionID,
		"phase":      string(sess.Phase),
	})
}

// statusHandler returns current onboarding phase, progress, and prerequisite readiness.
// GET /api/onboarding/status
func (s *server) statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()

	if sess == nil {
		http.Error(w, "no active session", http.StatusNotFound)
		return
	}

	sess.mu.RLock()
	phase := sess.Phase
	progress := sess.Progress
	startedAt := sess.StartedAt
	approved := sess.Approved
	sess.mu.RUnlock()

	ctx := r.Context()

	// Parse optional product_id from query string.
	var productIDPtr *uuid.UUID
	if pidStr := r.URL.Query().Get("product_id"); pidStr != "" {
		if pid, parseErr := uuid.Parse(pidStr); parseErr == nil {
			productIDPtr = &pid
		}
	}

	pre := s.gatherPrerequisites(ctx, productIDPtr)

	// Gather product list for the status response.
	var productList interface{} = nil
	if s.productStore != nil {
		prods, _ := s.productStore.ListProducts(ctx)
		productList = prods
	}

	canApproveCodex := pre.LLMConfigured && pre.GitHubConnected && pre.RepoCloned &&
		pre.CodexPopulated && pre.VectorIndexed

	canStartValidation := canApproveCodex && approved

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id":           sess.SessionID,
		"phase":                string(phase),
		"progress":             progress,
		"started_at":           startedAt.Format(time.RFC3339),
		"approved":             approved,
		"prerequisites":        pre,
		"products":             productList,
		"can_approve_codex":    canApproveCodex,
		"can_start_validation": canStartValidation,
	})
}

// verifyConnectivityHandler tests GitHub, Jira, and LLM connectivity.
// POST /api/onboarding/verify-connectivity
// Body: {"product_id":"..."} (optional)
func (s *server) verifyConnectivityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID string `json:"product_id"`
	}
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

	ctx := r.Context()
	resp := connectivityResponse{
		Results:  make(map[string]serviceCheckResult),
		Blockers: []string{},
		Warnings: []string{},
	}

	// Test LLM connectivity — hard gate: blocker.
	llmResult := serviceCheckResult{}
	if s.llmSvc != nil {
		if err := s.llmSvc.HealthCheck(ctx); err != nil {
			llmResult.Details = err.Error()
		} else {
			llmResult.Connected = true
			llmResult.Details = "LLM endpoint reachable"
		}
	} else {
		llmResult.Details = "LLM service not initialised"
	}
	resp.Results["llm"] = llmResult
	if !llmResult.Connected {
		resp.Blockers = append(resp.Blockers,
			"An LLM endpoint is required. Configure a model provider before proceeding.")
	}

	// Test GitHub token.
	ghResult := serviceCheckResult{}
	if s.cfg.GitHub.Token != "" {
		ghResult.Connected = true
		ghResult.Details = "GitHub token configured"
	} else {
		ghResult.Details = "GITHUB_TOKEN not configured"
	}
	resp.Results["github"] = ghResult

	// Check that the product has at least one repo — hard gate: blocker.
	if req.ProductID != "" && s.productStore != nil {
		if productID, parseErr := uuid.Parse(req.ProductID); parseErr == nil {
			repos, listErr := s.productStore.GetRepositoriesForProduct(ctx, productID)
			repoResult := serviceCheckResult{}
			if listErr != nil || len(repos) == 0 {
				repoResult.Details = "No repositories added to this product"
				resp.Results["repos"] = repoResult
				resp.Blockers = append(resp.Blockers,
					"At least one repository must be added to the product before proceeding. Use POST /api/onboarding/add-repo.")
			} else {
				repoResult.Connected = true
				repoResult.Details = fmt.Sprintf("%d repository(ies) configured", len(repos))
				resp.Results["repos"] = repoResult
			}
		}
	} else if s.cfg.GitHub.Repo == "" {
		resp.Blockers = append(resp.Blockers,
			"No product or repository configured. Add a product and repo first.")
	}

	// Test Jira connectivity — purely informational: never a blocker, never a warning.
	// Jira is optional. The system operates fully without it.
	jiraResult := serviceCheckResult{}
	if s.cfg.Jira.URL != "" && s.cfg.Jira.APIToken != "" {
		jiraResult.Connected = true
		jiraResult.Details = fmt.Sprintf("Jira configured for %s", s.cfg.Jira.URL)
	} else {
		jiraResult.Details = "Jira is not configured. Ticket sync will be disabled. You can add it later from Admin → Integrations."
	}
	resp.Results["jira"] = jiraResult
	// Jira not configured is INFO only — no blocker, no warning.

	resp.CanProceed = len(resp.Blockers) == 0

	writeJSON(w, http.StatusOK, resp)
}

// runAnalysisHandler triggers an initial codebase scan via Knowledge Indexer.
// POST /api/onboarding/run-analysis
// Body: {"product_id":"..."} (optional but recommended)
func (s *server) runAnalysisHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID string `json:"product_id"`
	}
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()
	if sess == nil {
		http.Error(w, "no active session", http.StatusBadRequest)
		return
	}

	// Hard gate: if a product_id is given, ensure repos are cloned.
	if req.ProductID != "" && s.productStore != nil {
		productID, parseErr := uuid.Parse(req.ProductID)
		if parseErr != nil {
			http.Error(w, "invalid product_id", http.StatusBadRequest)
			return
		}
		repos, listErr := s.productStore.GetRepositoriesForProduct(r.Context(), productID)
		if listErr != nil || len(repos) == 0 {
			http.Error(w, "no repositories configured for this product — add repos and clone them first", http.StatusPreconditionFailed)
			return
		}
		anyCloned := false
		for _, repo := range repos {
			if info, statErr := os.Stat(repo.ClonePath + "/.git"); statErr == nil && info.IsDir() {
				anyCloned = true
				break
			}
		}
		if !anyCloned {
			http.Error(w, "no repositories are cloned yet — run POST /api/onboarding/clone-repos first", http.StatusPreconditionFailed)
			return
		}
	}

	sess.mu.Lock()
	sess.Phase = PhaseAnalysis
	sess.Progress = 20
	sess.mu.Unlock()

	// Enqueue the analysis trigger so the knowledge-indexer picks it up.
	payload := map[string]string{
		"session_id": sess.SessionID,
		"trigger":    "onboarding_initial_scan",
		"product_id": req.ProductID,
	}
	aggType, aggID := analysisTriggerAggregate(sess.SessionID, req.ProductID)
	if err := enqueueCodexUpdated(r.Context(), s.pool, aggType, aggID, sess.SessionID, payload); err != nil {
		log.Printf("onboarding: enqueue analysis trigger: %v", err)
		http.Error(w, "failed to trigger analysis", http.StatusInternalServerError)
		return
	}

	log.Printf("onboarding: analysis triggered for session %s product_id=%s", sess.SessionID, req.ProductID)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "triggered",
		"message": "Initial codebase analysis started. This may take several minutes.",
	})
}

// evaluateModelHandler runs the LLM capability evaluation.
// POST /api/onboarding/evaluate-model
func (s *server) evaluateModelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()
	if sess == nil {
		http.Error(w, "no active session", http.StatusBadRequest)
		return
	}

	// Hard gate: LLM must be configured.
	if s.llmSvc == nil {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error":         "LLM service not configured",
			"action_needed": "Configure a model provider in the admin dashboard (Admin → Models) before running evaluation.",
		})
		return
	}
	if err := s.llmSvc.HealthCheck(r.Context()); err != nil {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{
			"error":         "LLM endpoint not reachable: " + err.Error(),
			"action_needed": "Ensure the configured LLM endpoint is accessible.",
		})
		return
	}

	sess.mu.Lock()
	sess.Phase = PhaseModelEvaluation
	sess.Progress = 40
	sess.mu.Unlock()

	prof, err := s.llmSvc.RunEvaluation(r.Context())
	if err != nil {
		log.Printf("onboarding: model evaluation failed: %v", err)
		http.Error(w, "model evaluation failed", http.StatusInternalServerError)
		return
	}

	sess.mu.Lock()
	sess.Progress = 50
	sess.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model_id":       prof.ModelID,
		"provider":       prof.Provider,
		"operating_mode": string(prof.OperatingMode),
		"scores": map[string]float64{
			"instruction_following": float64(prof.InstructionFollowing),
			"code_generation":       float64(prof.CodeGeneration),
			"code_understanding":    float64(prof.CodeUnderstanding),
			"reasoning":             float64(prof.Reasoning),
			"synthesis":             float64(prof.Synthesis),
		},
	})
}

// approveCodexHandler marks the Codex as approved and commits version 1.
// POST /api/onboarding/approve-codex
// Body: {"product_id":"..."} (optional)
func (s *server) approveCodexHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID string `json:"product_id"`
	}
	json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck

	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()
	if sess == nil {
		http.Error(w, "no active session", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Parse product_id up front so every downstream write (sections,
	// module, pattern, version, session) can be scoped to the correct
	// product. Before this, module/pattern rows landed with NULL
	// product_id and the dashboard's per-product stats for the newly
	// onboarded product were always zero — see audit findings A.1/A.2/A.3.
	productUUID, err := uuid.Parse(req.ProductID)
	if err != nil {
		log.Printf("onboarding: parse product_id %q: %v", req.ProductID, err)
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	// Resolve the first enabled repository for this product so codex
	// module/pattern rows can also carry repository_id. Onboarding today
	// expects a single repo per product; if multiple are registered we
	// pick the first (stable by name). Missing repo is non-fatal — we
	// fall back to product-only scoping.
	var repoIDPtr *uuid.UUID
	// repoBranch is the product's default branch, pulled from the
	// repositories row that addRepoHandler populated via
	// `git ls-remote --symref`. Every product-scoped codex row we write
	// below is tagged with this branch so lookups on the correct branch
	// partition actually find our rows — previously this defaulted to
	// "main" silently and a repository on master returned 0 rows
	// from every codex_* tool.
	var repoBranch string
	if s.productStore != nil {
		if repos, listErr := s.productStore.GetRepositoriesForProduct(ctx, productUUID); listErr != nil {
			log.Printf("onboarding: list repos for product %s: %v (continuing with product-only scope)", productUUID, listErr)
		} else if len(repos) > 0 {
			rid := repos[0].ID
			repoIDPtr = &rid
			repoBranch = repos[0].BaseBranch
		}
	}
	if repoBranch == "" {
		// No default branch recorded — refuse to write product-scoped rows
		// with an empty branch. The caller must re-run addRepoHandler so
		// the remote default is resolved via git ls-remote --symref.
		log.Printf("onboarding: no default branch recorded for product %s; cannot write product-scoped codex rows", productUUID)
		http.Error(w, fmt.Sprintf("no default branch configured for product %s; re-run add-repo to populate repositories.base_branch from the git remote", productUUID), http.StatusPreconditionFailed)
		return
	}

	// Persist product_id on the session so the validation handler can
	// later thread it onto the WorkItems and intake publish payload.
	sess.mu.Lock()
	sess.ProductID = productUUID
	sess.mu.Unlock()

	// First, persist conversation answers to the Codex as sections + a module entry.
	// This happens BEFORE any count checks so the conversation data populates the Codex.
	sess.mu.RLock()
	draft := make(map[string]string, len(sess.CodexDraft))
	for k, v := range sess.CodexDraft {
		draft[k] = v
	}
	sess.mu.RUnlock()

	// Overlay pre-fill candidates: for any question where the user confirmed
	// or edited the detection, use that value (user_value or value). The
	// session draft still wins when present (explicit conversation turns).
	// Failures here are non-fatal — approving the Codex must not block if the
	// pre-fill store is unavailable.
	if s.onboardingStore != nil {
		cands, listErr := s.onboardingStore.List(ctx, productUUID)
		if listErr != nil {
			log.Printf("onboarding: list candidates during approve-codex (continuing without prefill overlay): %v", listErr)
		} else {
			for _, c := range cands {
				if _, alreadyDrafted := draft[c.QuestionKey]; alreadyDrafted {
					continue
				}
				if c.UserStatus == onboarding.StatusConfirmed || c.UserStatus == onboarding.StatusEdited {
					draft[c.QuestionKey] = c.EffectiveValue()
				}
			}
		}
	}

	sectionKeys := make([]string, 0, len(draft))
	for key, content := range draft {
		sectionType := models.CodexSectionTypeArchitecture
		switch key {
		case "patterns_discovered", "anti_patterns":
			sectionType = models.CodexSectionTypePattern
		case "testing_philosophy", "test_conventions":
			sectionType = models.CodexSectionTypeTesting
		case "branching_strategy", "pr_process", "deployment_process":
			sectionType = models.CodexSectionTypeProcess
		case "domain_knowledge", "critical_constraints":
			sectionType = models.CodexSectionTypeDomain
		case "autonomous_scope", "review_triggers", "risk_zones":
			sectionType = models.CodexSectionTypeStyle
		}
		pid := productUUID
		section := &models.CodexSection{
			SectionType:  sectionType,
			Name:         key,
			Content:      json.RawMessage(fmt.Sprintf("%q", content)),
			ProductID:    &pid,
			RepositoryID: repoIDPtr,
			Branch:       repoBranch,
		}
		if err := s.cx.UpsertSection(ctx, section); err != nil {
			log.Printf("onboarding: upsert section %q: %v", key, err)
		}
		sectionKeys = append(sectionKeys, key)
	}

	// Create a module entry for the product so the Codex has at least one module.
	// This represents what was learned about the codebase from analysis + conversation.
	modulePurpose := draft["architecture_overview"]
	if modulePurpose == "" {
		modulePurpose = "Primary application module"
	}
	testingNotes := draft["testing_philosophy"]
	if testingNotes == "" {
		testingNotes = "Not specified"
	}

	moduleEntry := &models.CodexModule{
		Name:              "main",
		Purpose:           modulePurpose,
		PublicInterface:   json.RawMessage(`{}`),
		Dependencies:      []string{},
		Dependents:        []string{},
		InternalPatterns:  draft["patterns_discovered"],
		OwnerDomain:       draft["domain_knowledge"],
		RiskLevel:         "medium",
		TestCoverageNotes: testingNotes,
		Branch:            repoBranch,
	}
	// Prefer the product-scoped -ForRepo variant when we have a repo;
	// fall back to the plain UpsertModule (which writes NULL product_id
	// to the legacy/global partition) only if no repo is available.
	var moduleErr error
	if repoIDPtr != nil {
		moduleErr = s.cx.UpsertModuleForRepoBranch(ctx, productUUID, *repoIDPtr, repoBranch, moduleEntry)
	} else {
		pid := productUUID
		moduleEntry.ProductID = &pid
		moduleErr = s.cx.UpsertModule(ctx, moduleEntry)
	}
	if moduleErr != nil {
		log.Printf("onboarding: upsert module: %v", moduleErr)
	}

	// Create a pattern entry from the conversation data.
	patternWritten := false
	if patterns := draft["patterns_discovered"]; patterns != "" {
		patternEntry := &models.CodexPattern{
			Name:           "team_conventions",
			Category:       "conventions",
			WhenToUse:      "Always — these are the team's established patterns",
			ReferenceFiles: []string{},
			Rationale:      patterns,
			KeyRules:       []string{patterns},
			CommonMistakes: []string{},
			Branch:         repoBranch,
		}
		if antiPatterns := draft["anti_patterns"]; antiPatterns != "" {
			patternEntry.CommonMistakes = []string{antiPatterns}
		}
		var patternErr error
		if repoIDPtr != nil {
			patternErr = s.cx.UpsertPatternForRepoBranch(ctx, productUUID, *repoIDPtr, repoBranch, patternEntry)
		} else {
			pid := productUUID
			patternEntry.ProductID = &pid
			patternErr = s.cx.UpsertPattern(ctx, patternEntry)
		}
		if patternErr != nil {
			log.Printf("onboarding: upsert pattern: %v", patternErr)
		}
		patternWritten = patternErr == nil
	}

	// Create a codex version, scoped to this product and its default branch
	// so the portal's guards.CheckOnboardingComplete(productID) can see it
	// and stop rejecting work-item submissions with not_onboarded. Its
	// codex.indexed event commits with it (migration 060), so there is no
	// separate notification to lose; re-approving is safe (the upserts are
	// idempotent; it records one more version).
	rec, err := s.cx.RecordVersion(ctx, codex.NewVersion{
		ProductID:       productUUID,
		Branch:          repoBranch,
		SectionsChanged: approvalSections(sectionKeys, moduleErr == nil, patternWritten),
		Summary:         "Initial onboarding — version 1 committed",
		Source:          models.CodexChangeSourceOnboarding,
		Diff:            json.RawMessage(`{}`),
		Trigger:         codex.TriggerOnboardingApproval,
	})
	if err != nil {
		log.Printf("onboarding: create codex version: %v", err)
		http.Error(w, "failed to commit codex version", http.StatusInternalServerError)
		return
	}

	// Codex zones (migration 063): every repository without zones gets the
	// codex-config defaults and the zones derived from the risk_zones answer
	// (zones.go), in a section:zones version of its own. A repository whose
	// zones were confirmed in the wizard or edited in the dashboard is left
	// alone. Zones decide what agents may change, so a failure fails the
	// approval (re-approving is safe).
	created, zoneErr := s.applyOnboardingZones(ctx, productUUID)
	if zoneErr != nil {
		log.Printf("onboarding: apply codex zones for product %s: %v", productUUID, zoneErr)
		http.Error(w, "codex version committed but the repository zones could not be written; approve again", http.StatusInternalServerError)
		return
	}

	sess.mu.Lock()
	sess.Approved = true
	sess.Phase = PhaseCodexApproval
	sess.Progress = 70
	sess.mu.Unlock()

	log.Printf("onboarding: codex approved, version %d (product v%d on %s) committed", rec.VersionNumber, rec.ProductVersion, rec.Branch)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "approved",
		"version_number":  rec.VersionNumber,
		"product_version": rec.ProductVersion,
		"branch":          rec.Branch,
		"zones_created":   created,
	})
}

// approvalSections is the approved codex's typed sections_changed
// (migration 060): section:<key> for every onboarding answer written, then
// module:main and patterns when the approval wrote them.
func approvalSections(keys []string, moduleWritten, patternsWritten bool) []string {
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)
	out := make([]string, 0, len(sorted)+2)
	for i, k := range sorted {
		if i > 0 && sorted[i-1] == k {
			continue
		}
		out = append(out, codex.SectionChange(k))
	}
	if moduleWritten {
		out = append(out, codex.ModuleChange("main"))
	}
	if patternsWritten {
		out = append(out, string(codex.ChangePatterns))
	}
	return out
}

// startValidationHandler accepts 3-5 test work items and runs them through the pipeline.
// POST /api/onboarding/start-validation
func (s *server) startValidationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()
	if sess == nil {
		http.Error(w, "no active session", http.StatusBadRequest)
		return
	}

	var req validationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if len(req.WorkItems) < 3 || len(req.WorkItems) > 5 {
		http.Error(w, "between 3 and 5 work items required", http.StatusBadRequest)
		return
	}

	// ProductID must have been captured during approve-codex. Without it
	// the validation items would bypass every per-product guard downstream
	// (product-agent's CheckOnboardingComplete, per-product codex lookups,
	// dashboard filters) — the exact class of silent-bypass that audit
	// finding A.9 flagged. Refuse the request rather than fabricate.
	sess.mu.RLock()
	productID := sess.ProductID
	sess.mu.RUnlock()
	if productID == uuid.Nil {
		http.Error(w, "approve-codex has not been called for this session (product_id missing)", http.StatusPreconditionFailed)
		return
	}

	ctx := r.Context()
	workIDs := make([]string, 0, len(req.WorkItems))

	for _, wi := range req.WorkItems {
		workID := uuid.New()

		wiType := models.WorkItemTypeFeature
		if strings.ToLower(wi.Type) == "bug" {
			wiType = models.WorkItemTypeBug
		}

		pid := productID
		item := &models.WorkItem{
			ID:          workID,
			Type:        wiType,
			State:       models.WorkItemStateIntake,
			Title:       wi.Title,
			Description: wi.Description,
			Priority:    models.PriorityMedium,
			ProductID:   &pid,
		}

		// Enqueue intake with the work item so the full pipeline
		// processes it. Payload shape matches product-agent's
		// intakeMessage struct — work_id, product_id, description, source,
		// metadata. Omitting product_id (the old shape) made product-agent
		// read intake.ProductID == "" and skip every product-scoped check.
		payload := map[string]interface{}{
			"work_id":     workID.String(),
			"product_id":  productID.String(),
			"description": wi.Description,
			"source":      "onboarding_validation",
			"metadata": map[string]string{
				"session_id": sess.SessionID,
				"item_title": wi.Title,
			},
		}
		if err := createValidationItem(ctx, s.pool, s.wg, item, payload); err != nil {
			log.Printf("onboarding: create validation work item: %v", err)
			continue
		}

		workIDs = append(workIDs, workID.String())
	}

	sess.mu.Lock()
	sess.Phase = PhaseValidation
	sess.Progress = 85
	sess.ValidationWI = workIDs
	sess.mu.Unlock()

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":   "running",
		"work_ids": workIDs,
		"message":  "Validation items submitted. Monitor /api/onboarding/status for progress.",
	})
}

// ─── WebSocket conversation handler ─────────────────────────────────────────

// wsHandler manages the interactive onboarding conversation over WebSocket.
// GET /ws/onboarding
func (s *server) wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("onboarding: ws upgrade error: %v", err)
		return
	}
	defer conn.Close()

	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()

	if sess == nil {
		_ = conn.WriteJSON(wsOutgoing{
			Type:    "error",
			Payload: "No active session. Call POST /api/onboarding/start first.",
		})
		return
	}

	sess.mu.Lock()
	sess.Phase = PhaseConversation
	sess.Progress = 10
	sess.mu.Unlock()

	sendMsg := func(out wsOutgoing) bool {
		if err := conn.WriteJSON(out); err != nil {
			log.Printf("onboarding: ws write error: %v", err)
			return false
		}
		return true
	}

	ctx := r.Context()
	draft := make(map[string]string)

	for i, q := range orderedQuestions {
		// Optionally use LLM to personalise the question based on previous answers.
		questionText := q.text
		if s.llmSvc != nil && len(draft) > 0 {
			draftJSON, _ := json.Marshal(draft)
			intent := strategy.Intent{
				Type: strategy.IntentOnboardingQuestion,
				Context: map[string]string{
					"question_topic":   q.topic,
					"question_text":    q.text,
					"collected_so_far": string(draftJSON),
				},
				MaxTokens: 200,
			}
			result, err := s.llmSvc.Execute(ctx, intent)
			if err == nil && result.Content != "" {
				questionText = result.Content
			}
		}

		// Send question to client.
		if !sendMsg(wsOutgoing{
			Type:    "question",
			Payload: questionText,
			Phase:   string(PhaseConversation),
		}) {
			return
		}

		// Wait for client answer.
		var incoming wsIncoming
		if err := conn.ReadJSON(&incoming); err != nil {
			log.Printf("onboarding: ws read error: %v", err)
			return
		}

		if incoming.Action == "skip" {
			continue
		}

		answer := strings.TrimSpace(incoming.Payload)
		if answer != "" {
			draft[q.key] = answer
		}

		// Update progress proportionally through questions.
		progress := 10 + int(float64(i+1)/float64(len(orderedQuestions))*55)
		sess.mu.Lock()
		sess.CodexDraft[q.key] = answer
		sess.Progress = progress
		sess.mu.Unlock()

		// Send incremental draft update.
		if !sendMsg(wsOutgoing{
			Type:  "update",
			Phase: string(PhaseConversation),
			Draft: draft,
		}) {
			return
		}
	}

	// Conversation complete — send final state.
	sess.mu.Lock()
	sess.Progress = 65
	sess.mu.Unlock()

	sendMsg(wsOutgoing{
		Type:    "complete",
		Payload: "Onboarding conversation complete. Review the draft Codex and call POST /api/onboarding/approve-codex to commit.",
		Phase:   string(PhaseCodexApproval),
		Draft:   draft,
	})
}

// conversationHandler handles a single question-answer exchange via REST.
// POST /api/onboarding/conversation
// Body: {"product_id":"...", "history": [{"role":"user","content":"..."},{"role":"assistant","content":"..."},...]}
func (s *server) conversationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID string              `json:"product_id"`
		History   []map[string]string `json:"history"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Figure out which question we're on based on history length
	// Count user messages to determine progress
	userMsgCount := 0
	for _, msg := range req.History {
		if msg["role"] == "user" {
			userMsgCount++
		}
	}

	// The latest user message is the answer to the current question
	latestAnswer := ""
	if len(req.History) > 0 {
		last := req.History[len(req.History)-1]
		if last["role"] == "user" {
			latestAnswer = last["content"]
		}
	}

	// Store the answer in the session if we have one
	s.sessionMu.RLock()
	sess := s.session
	s.sessionMu.RUnlock()

	currentQuestionIdx := userMsgCount - 1 // 0-based: first user answer corresponds to first question
	if currentQuestionIdx >= 0 && currentQuestionIdx < len(orderedQuestions) && latestAnswer != "" {
		q := orderedQuestions[currentQuestionIdx]
		if sess != nil {
			sess.mu.Lock()
			sess.CodexDraft[q.key] = latestAnswer
			sess.Phase = PhaseConversation
			sess.mu.Unlock()
		}
	}

	// Determine the next question to ask
	nextQuestionIdx := userMsgCount // next question index = number of answers given
	if nextQuestionIdx >= len(orderedQuestions) {
		// Conversation complete
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"message":  "Onboarding conversation complete. You can now review and approve the Codex.",
			"complete": true,
			"progress": 100,
		})
		return
	}

	nextQ := orderedQuestions[nextQuestionIdx]
	questionText := nextQ.text

	// Load the pre-fill candidate (if any) so the prompt asks for confirmation
	// instead of asking cold. Spec: Inject confirmed candidates into
	// IntentOnboardingQuestion LLM prompts as candidate_answer + evidence.
	// Degrade gracefully: lookup failures must not block the conversation flow.
	var candidateAnswer, candidateEvidence, candidateConfidence, candidateStatus, candidateExtractor string
	if s.onboardingStore != nil && req.ProductID != "" {
		if pid, err := uuid.Parse(req.ProductID); err == nil {
			cand, lookupErr := s.onboardingStore.Get(r.Context(), pid, nextQ.key)
			if lookupErr != nil {
				log.Printf("onboarding: candidate lookup for %s failed (continuing without prefill): %v", nextQ.key, lookupErr)
			} else if cand != nil {
				candidateAnswer = cand.EffectiveValue()
				candidateConfidence = string(cand.Confidence)
				candidateStatus = string(cand.UserStatus)
				candidateExtractor = cand.Extractor
				if evBytes, mErr := json.Marshal(cand.Evidence); mErr == nil {
					candidateEvidence = string(evBytes)
				}
			}
		}
	}

	// Optionally personalize with LLM
	if s.llmSvc != nil && sess != nil {
		sess.mu.RLock()
		draftJSON, _ := json.Marshal(sess.CodexDraft)
		sess.mu.RUnlock()

		ctxMap := map[string]string{
			"question_topic":   nextQ.topic,
			"question_text":    nextQ.text,
			"collected_so_far": string(draftJSON),
		}
		if candidateAnswer != "" {
			ctxMap["candidate_answer"] = candidateAnswer
			ctxMap["candidate_evidence"] = candidateEvidence
			ctxMap["candidate_confidence"] = candidateConfidence
			ctxMap["candidate_user_status"] = candidateStatus
			ctxMap["candidate_extractor"] = candidateExtractor
			ctxMap["candidate_instruction"] = "The system detected the candidate_answer above from the codebase. Ask the user to confirm, edit, or reject it; cite the evidence by path. Do not ask from scratch."
		}
		intent := strategy.Intent{
			Type:      strategy.IntentOnboardingQuestion,
			Context:   ctxMap,
			MaxTokens: 300,
		}
		result, err := s.llmSvc.Execute(r.Context(), intent)
		if err == nil && result.Content != "" {
			questionText = result.Content
		}
	}

	progress := int(float64(nextQuestionIdx) / float64(len(orderedQuestions)) * 100)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":         questionText,
		"complete":        false,
		"progress":        progress,
		"question_number": nextQuestionIdx + 1,
		"total_questions": len(orderedQuestions),
		"topic":           nextQ.topic,
	})
}

// discoverJiraHandler triggers Jira project auto-discovery and returns the
// project structure plus an LLM-proposed workflow mapping.
// POST /api/onboarding/discover-jira
func (s *server) discoverJiraHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProjectKey string `json:"project_key"`
		// Optional credential overrides — used when credentials are not yet in
		// the config store (i.e., during initial onboarding setup).
		JiraURL   string `json:"jira_url,omitempty"`
		JiraEmail string `json:"jira_email,omitempty"`
		JiraToken string `json:"jira_token,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ProjectKey == "" {
		http.Error(w, "project_key is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Resolve Jira credentials: prefer values from the request body (for initial
	// setup), fall back to the config store.
	jiraURL := req.JiraURL
	jiraEmail := req.JiraEmail
	jiraToken := req.JiraToken

	if s.configStore != nil {
		if jiraURL == "" {
			jiraURL, _ = s.configStore.ResolveCredential(ctx, "jira", "url", "", "")
		}
		if jiraEmail == "" {
			jiraEmail, _ = s.configStore.ResolveCredential(ctx, "jira", "email", "", "")
		}
		if jiraToken == "" {
			jiraToken, _ = s.configStore.ResolveCredential(ctx, "jira", "api_token", "", "")
		}
	}

	// Fall back to static config values if credential store had nothing.
	if jiraURL == "" {
		jiraURL = s.cfg.Jira.URL
	}
	if jiraToken == "" {
		jiraToken = s.cfg.Jira.APIToken
	}

	if jiraURL == "" || jiraToken == "" {
		http.Error(w, "Jira credentials are not configured", http.StatusBadRequest)
		return
	}

	client := jirapkg.NewClient(jiraURL, jiraEmail, jiraToken)

	project, err := client.DiscoverProject(ctx, req.ProjectKey)
	if err != nil {
		log.Printf("onboarding: discover jira project %q: %v", req.ProjectKey, err)
		http.Error(w, fmt.Sprintf("Jira discovery failed: %v", err), http.StatusBadGateway)
		return
	}

	// Propose a mapping via the LLM; fall back to a heuristic default if the
	// LLM service is unavailable.
	var mapping *jirapkg.WorkflowMapping
	if s.llmSvc != nil {
		mapping, err = jirapkg.ProposeMapping(ctx, s.llmSvc, project)
		if err != nil {
			log.Printf("onboarding: propose jira mapping: %v — using default mapping", err)
			mapping = jirapkg.DefaultMapping(project)
		}
	} else {
		mapping = jirapkg.DefaultMapping(project)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"project":          project,
		"proposed_mapping": mapping,
	})
}

// saveJiraMappingHandler persists a confirmed (possibly user-modified) Jira
// workflow mapping to the config store.
// POST /api/onboarding/save-jira-mapping
func (s *server) saveJiraMappingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var mapping jirapkg.WorkflowMapping
	if err := json.NewDecoder(r.Body).Decode(&mapping); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if s.configStore == nil {
		http.Error(w, "config store not available", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	if err := jirapkg.SaveMapping(ctx, s.configStore, &mapping); err != nil {
		log.Printf("onboarding: save jira mapping: %v", err)
		http.Error(w, fmt.Sprintf("failed to save mapping: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("onboarding: jira mapping saved")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// ─── Product management handlers ──────────────────────────────────────────────

// createProductHandler creates a new product and returns its ID.
// POST /api/onboarding/create-product
// Body: {"name":"...","description":"..."}
func (s *server) createProductHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if s.productStore == nil {
		http.Error(w, "product store not available", http.StatusServiceUnavailable)
		return
	}

	product, err := s.productStore.CreateProduct(r.Context(), req.Name, req.Description)
	if err != nil {
		log.Printf("onboarding: create product %q: %v", req.Name, err)
		http.Error(w, fmt.Sprintf("failed to create product: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("onboarding: created product %s (%s)", product.Name, product.ID)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"product_id":  product.ID.String(),
		"name":        product.Name,
		"description": product.Description,
	})
}

// addRepoHandler validates GitHub access and adds a repository to a product.
// POST /api/onboarding/add-repo
// Body: {"product_id":"...","github_url":"...","name":"...","base_branch":"...","merge_strategy":"...","branch_prefix":"..."}
func (s *server) addRepoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID     string `json:"product_id"`
		GitHubURL     string `json:"github_url"`
		Name          string `json:"name"`
		BaseBranch    string `json:"base_branch"`
		MergeStrategy string `json:"merge_strategy"`
		BranchPrefix  string `json:"branch_prefix"`

		// VerificationPolicy, when present, is written in the same
		// transaction as the repository row (M3-03).
		VerificationPolicy *verifypolicy.Input `json:"verification_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ProductID == "" {
		http.Error(w, "product_id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.GitHubURL) == "" {
		http.Error(w, "github_url is required", http.StatusBadRequest)
		return
	}
	// Classify before anything runs git against the value: an invalid one
	// (leading "-", whitespace, transport::, ...) is a 400, never an
	// ls-remote argument.
	githubURL, remoteKind, err := products.ClassifyRemote(req.GitHubURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.GitHubURL = githubURL
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if s.productStore == nil {
		http.Error(w, "product store not available", http.StatusServiceUnavailable)
		return
	}

	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	// Validate GitHub access using org token. A remote that is not on
	// github.com (local or bare mirror, file://, another host) never gets
	// the token, so it does not need one.
	if remoteKind.IsGitHub() && s.cfg.GitHub.Token == "" {
		http.Error(w, "GitHub token not configured", http.StatusPreconditionFailed)
		return
	}
	if !remoteKind.IsGitHub() {
		log.Printf("onboarding: repository %q has a %s remote: no GitHub token is attached and its PRs go through the manual PR gate", req.Name, remoteKind)
	}

	// Resolve the actual default branch from the git remote. This is the
	// source of truth — never hardcode "main" because real repos on master,
	// develop, or trunk silently end up indexed under the wrong partition.
	// If the caller explicitly provided a base_branch we honour it (the UI
	// may want to override), otherwise we consult the remote.
	baseBranch := strings.TrimSpace(req.BaseBranch)
	if baseBranch == "" {
		resolvedBranch, err := resolveRemoteDefaultBranch(r.Context(), req.GitHubURL, s.cfg.GitHub.Token)
		if err != nil {
			log.Printf("onboarding: resolve default branch for %s: %v", req.GitHubURL, err)
			http.Error(w, fmt.Sprintf("could not determine default branch for %s — check repo access: %v", req.GitHubURL, err), http.StatusBadGateway)
			return
		}
		baseBranch = resolvedBranch
	}

	mergeStrategy := req.MergeStrategy
	if mergeStrategy == "" {
		mergeStrategy = "squash"
	}
	branchPrefix := req.BranchPrefix
	if branchPrefix == "" {
		branchPrefix = "ae/"
	}

	repo, err := s.addRepositoryWithPolicy(r.Context(), req.VerificationPolicy, productID, req.Name, req.GitHubURL, baseBranch, mergeStrategy, branchPrefix)
	var inputErr *policyInputError
	var remoteErr *products.InvalidRemoteError
	if errors.As(err, &inputErr) {
		http.Error(w, inputErr.Error(), http.StatusBadRequest)
		return
	}
	if errors.As(err, &remoteErr) {
		http.Error(w, remoteErr.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("onboarding: add repo %q to product %s: %v", req.Name, req.ProductID, err)
		http.Error(w, fmt.Sprintf("failed to add repository: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("onboarding: added repo %s (%s) to product %s", repo.Name, repo.ID, productID)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"repository_id":  repo.ID.String(),
		"product_id":     productID.String(),
		"name":           repo.Name,
		"github_url":     repo.GitHubURL,
		"base_branch":    repo.BaseBranch,
		"merge_strategy": repo.MergeStrategy,
		"branch_prefix":  repo.BranchPrefix,
		"clone_path":     repo.ClonePath,
		"remote_kind":    repo.RemoteKind,
	})
}

// removeRepoHandler removes a repository from a product.
// POST /api/onboarding/remove-repo
// Body: {"repository_id":"..."}
func (s *server) removeRepoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RepositoryID string `json:"repository_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.RepositoryID == "" {
		http.Error(w, "repository_id is required", http.StatusBadRequest)
		return
	}
	if s.productStore == nil {
		http.Error(w, "product store not available", http.StatusServiceUnavailable)
		return
	}

	repoID, err := uuid.Parse(req.RepositoryID)
	if err != nil {
		http.Error(w, "invalid repository_id", http.StatusBadRequest)
		return
	}

	if err := s.productStore.DeleteRepository(r.Context(), repoID); err != nil {
		log.Printf("onboarding: remove repo %s: %v", req.RepositoryID, err)
		http.Error(w, fmt.Sprintf("failed to remove repository: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("onboarding: removed repo %s", req.RepositoryID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "repository_id": req.RepositoryID})
}

// addDependencyHandler maps a cross-repo dependency between two repositories.
// POST /api/onboarding/add-dependency
// Body: {"from_repository_id":"...","to_repository_id":"...","dependency_type":"...","description":"..."}
func (s *server) addDependencyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		FromRepositoryID string `json:"from_repository_id"`
		ToRepositoryID   string `json:"to_repository_id"`
		DependencyType   string `json:"dependency_type"`
		Description      string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.FromRepositoryID == "" || req.ToRepositoryID == "" {
		http.Error(w, "from_repository_id and to_repository_id are required", http.StatusBadRequest)
		return
	}
	if s.productStore == nil {
		http.Error(w, "product store not available", http.StatusServiceUnavailable)
		return
	}

	fromID, err := uuid.Parse(req.FromRepositoryID)
	if err != nil {
		http.Error(w, "invalid from_repository_id", http.StatusBadRequest)
		return
	}
	toID, err := uuid.Parse(req.ToRepositoryID)
	if err != nil {
		http.Error(w, "invalid to_repository_id", http.StatusBadRequest)
		return
	}

	depType := req.DependencyType
	if depType == "" {
		depType = "uses"
	}

	if err := s.productStore.AddDependency(r.Context(), fromID, toID, depType, req.Description, "onboarding"); err != nil {
		log.Printf("onboarding: add dependency %s -> %s: %v", req.FromRepositoryID, req.ToRepositoryID, err)
		http.Error(w, fmt.Sprintf("failed to add dependency: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("onboarding: added dependency %s -> %s (%s)", req.FromRepositoryID, req.ToRepositoryID, depType)
	writeJSON(w, http.StatusCreated, map[string]string{
		"status":             "created",
		"from_repository_id": req.FromRepositoryID,
		"to_repository_id":   req.ToRepositoryID,
		"dependency_type":    depType,
	})
}

// cloneReposHandler triggers cloning of all repositories for a product via
// github-service /clone-all.
// POST /api/onboarding/clone-repos
// Body: {"product_id":"..."}
func (s *server) cloneReposHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ProductID string `json:"product_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ProductID == "" {
		http.Error(w, "product_id is required", http.StatusBadRequest)
		return
	}

	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	// Hard gate: product must have at least one repository.
	if s.productStore != nil {
		repos, listErr := s.productStore.GetRepositoriesForProduct(r.Context(), productID)
		if listErr != nil || len(repos) == 0 {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{
				"error":         "no repositories configured for this product",
				"action_needed": "Add at least one repository using POST /api/onboarding/add-repo before cloning.",
			})
			return
		}
	}

	if s.githubServiceURL == "" {
		http.Error(w, "github-service URL not configured", http.StatusServiceUnavailable)
		return
	}

	// Call github-service /clone-all.
	payload, _ := json.Marshal(map[string]string{
		"product_id": req.ProductID,
	})
	cloneURL := strings.TrimRight(s.githubServiceURL, "/") + "/clone-all"
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cloneURL, bytes.NewReader(payload))
	if err != nil {
		log.Printf("onboarding: build clone-all request: %v", err)
		http.Error(w, "failed to build clone request", http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	cloneCtx, cloneCancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cloneCancel()
	httpReq = httpReq.WithContext(cloneCtx)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("onboarding: clone-all via github-service: %v", err)
		http.Error(w, fmt.Sprintf("clone-all failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		http.Error(w, fmt.Sprintf("github-service clone-all returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	var cloneResult interface{}
	_ = json.NewDecoder(resp.Body).Decode(&cloneResult)

	log.Printf("onboarding: clone-all triggered for product %s", req.ProductID)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":     "triggered",
		"product_id": req.ProductID,
		"result":     cloneResult,
	})
}

// ─── Onboarding candidate handlers ────────────────────────────────────────────

// candidatesListHandler returns every pre-fill candidate for a product.
// GET /api/onboarding/candidates?product_id=<uuid>
func (s *server) candidatesListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "product_id is required"})
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid product_id"})
		return
	}
	if s.onboardingStore == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"candidates": []onboarding.Candidate{}})
		return
	}
	cands, err := s.onboardingStore.List(r.Context(), productID)
	if err != nil {
		log.Printf("onboarding: list candidates: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list candidates: " + err.Error()})
		return
	}
	if cands == nil {
		cands = []onboarding.Candidate{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"candidates": cands})
}

// candidatesReviewHandler records the user's decision on a single candidate.
// POST /api/onboarding/candidates/review
// Body: {"product_id":"...","question_key":"...","status":"confirmed|edited|rejected","user_value":"..."}
func (s *server) candidatesReviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		ProductID   string  `json:"product_id"`
		QuestionKey string  `json:"question_key"`
		Status      string  `json:"status"`
		UserValue   *string `json:"user_value,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.ProductID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "product_id is required"})
		return
	}
	productID, err := uuid.Parse(req.ProductID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid product_id"})
		return
	}
	if req.QuestionKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question_key is required"})
		return
	}
	status := onboarding.UserStatus(req.Status)
	switch status {
	case onboarding.StatusConfirmed, onboarding.StatusEdited, onboarding.StatusRejected:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be one of confirmed|edited|rejected"})
		return
	}
	if s.onboardingStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "onboarding store not configured"})
		return
	}
	if err := s.onboardingStore.Review(r.Context(), productID, req.QuestionKey, status, req.UserValue); err != nil {
		log.Printf("onboarding: review candidate %s/%s: %v", req.ProductID, req.QuestionKey, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "review: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// candidatesRegenerateHandler asks the indexer to re-run pre-fill for a
// product. The work happens asynchronously on the indexer side; this endpoint
// only publishes the trigger and returns 202.
// POST /api/onboarding/candidates/regenerate?product_id=<uuid>
// or Body: {"product_id":"..."}
func (s *server) candidatesRegenerateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// Accept product_id from query string or JSON body.
	productIDStr := r.URL.Query().Get("product_id")
	if productIDStr == "" {
		var req struct {
			ProductID string `json:"product_id"`
		}
		// Body is optional when query parameter is present; tolerate an empty body.
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
				return
			}
		}
		productIDStr = req.ProductID
	}
	if productIDStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "product_id is required"})
		return
	}
	productID, err := uuid.Parse(productIDStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid product_id"})
		return
	}

	payload, err := json.Marshal(map[string]string{
		"product_id": productID.String(),
		"trigger":    "user_regenerate",
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "marshal payload: " + err.Error()})
		return
	}
	if err := s.kb.Publish(r.Context(), bus.TopicOnboardingPrefillProgress, productID.String(), payload); err != nil {
		log.Printf("onboarding: publish regenerate trigger for %s: %v", productID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "publish: " + err.Error()})
		return
	}

	log.Printf("onboarding: regenerate prefill triggered for product %s", productID)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":     "triggered",
		"product_id": productID.String(),
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ─── Main ────────────────────────────────────────────────────────────────────

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── Configuration ─────────────────────────────────────────────────────────

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("onboarding: load config: %v", err)
	}

	// ── Database ──────────────────────────────────────────────────────────────

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("onboarding: connect db: %v", err)
	}
	defer pool.Close()

	// Wire prompt store so strategy layer resolves prompts from DB.
	strategy.SetPromptStore(prompts.NewStore(pool))

	// ── Kafka ─────────────────────────────────────────────────────────────────

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("onboarding: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// ── Master encryption key ─────────────────────────────────────────────────

	var configStore *configstore.ConfigStore
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr == "" {
		_, encoded, _ := crypto.GenerateMasterKey()
		log.Printf("onboarding: WARNING: No MASTER_ENCRYPTION_KEY set. Generated: %s", encoded)
		log.Printf("onboarding: Jira discovery endpoints will use static config credentials only")
	} else {
		masterKey, mkErr := crypto.LoadMasterKey(masterKeyStr)
		if mkErr != nil {
			log.Fatalf("onboarding: load master key: %v", mkErr)
		}
		configStore = configstore.New(pool, masterKey)
		configStore.StartWatcher(ctx)
	}

	// ── Domain services ───────────────────────────────────────────────────────

	wg := workgraph.New(pool)
	cx := codex.New(pool)
	vs := vector.New(cfg.Qdrant.URL, "onboarding")
	ps := products.NewStore(pool)
	obStore := onboarding.NewStore(pool)

	// Build the LLM service from the credential registry. If no provider is
	// configured yet llmSvc will be nil; the onboarding wizard itself is the
	// primary path for adding an LLM provider (Admin → Models or step 1 of
	// the wizard).
	var llmSvc *llm.Service
	if configStore != nil {
		llmSvc, err = llm.NewServiceFromRegistry(configStore, "onboarding", "instance-1", pool)
		if err != nil {
			log.Println("onboarding: LLM not configured. Configure a model provider in the admin dashboard (Admin → Models).")
		}
	} else {
		log.Println("onboarding: LLM not configured. Configure a model provider in the admin dashboard (Admin → Models).")
	}

	// ── Health ────────────────────────────────────────────────────────────────

	checker := health.New(serviceName)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Resolve github-service URL from env (fallback to empty).
	githubServiceURL := os.Getenv("GITHUB_SERVICE_URL")

	// ── HTTP server ───────────────────────────────────────────────────────────

	// Validation work items and codex.updated are enqueued in the outbox;
	// this dispatcher delivers them. A start failure is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "onboarding")
	if err != nil {
		log.Fatalf("onboarding: start outbox dispatcher: %v", err)
	}

	svc := newServer(cfg, pool, configStore, kb, wg, cx, vs, llmSvc, ps, githubServiceURL, obStore)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/api/onboarding/start", svc.startHandler)
	mux.HandleFunc("/api/onboarding/status", svc.statusHandler)
	mux.HandleFunc("/api/onboarding/verify-connectivity", svc.verifyConnectivityHandler)
	mux.HandleFunc("/api/onboarding/run-analysis", svc.runAnalysisHandler)
	mux.HandleFunc("/api/onboarding/evaluate-model", svc.evaluateModelHandler)
	mux.HandleFunc("/api/onboarding/approve-codex", svc.approveCodexHandler)
	mux.HandleFunc("/api/onboarding/start-validation", svc.startValidationHandler)
	mux.HandleFunc("/api/onboarding/discover-jira", svc.discoverJiraHandler)
	mux.HandleFunc("/api/onboarding/save-jira-mapping", svc.saveJiraMappingHandler)
	mux.HandleFunc("/api/onboarding/create-product", svc.createProductHandler)
	mux.HandleFunc("/api/onboarding/add-repo", svc.addRepoHandler)
	mux.HandleFunc("/api/onboarding/verification-policy", svc.verificationPolicyHandler)
	mux.HandleFunc("/api/onboarding/remove-repo", svc.removeRepoHandler)
	mux.HandleFunc("/api/onboarding/add-dependency", svc.addDependencyHandler)
	mux.HandleFunc("/api/onboarding/clone-repos", svc.cloneReposHandler)
	mux.HandleFunc("/api/onboarding/conversation", svc.conversationHandler)
	mux.HandleFunc("/api/onboarding/candidates", svc.candidatesListHandler)
	mux.HandleFunc("/api/onboarding/candidates/review", svc.candidatesReviewHandler)
	mux.HandleFunc("/api/onboarding/candidates/regenerate", svc.candidatesRegenerateHandler)
	mux.HandleFunc("/api/onboarding/zones/proposed", svc.proposedZonesHandler)
	mux.HandleFunc("/api/onboarding/zones", svc.confirmZonesHandler)
	mux.HandleFunc("/ws/onboarding", svc.wsHandler)

	addr := fmt.Sprintf(":%d", onboardingPort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("onboarding: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("onboarding: server error: %v", err)
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("onboarding: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("onboarding: shutdown error: %v", err)
	}
	dispatcher.Wait()
	log.Println("onboarding: stopped")
}

// resolveRemoteDefaultBranch asks the git remote for its HEAD and parses the
// `ref: refs/heads/<name>` line. No "main" default — a repo on master,
// develop, or trunk is common enough that hardcoding main silently indexes
// rows onto the wrong branch partition and hides the failure.
//
// githubURL is any remote gitremote.Classify accepts: "owner/repo", a
// GitHub https or ssh URL, a local path, a file:// URL or another host. The
// URL and the token are resolved by gitremote, so the token is embedded only
// into an https://github.com URL and never sent to any other host.
func resolveRemoteDefaultBranch(ctx context.Context, githubURL, token string) (string, error) {
	githubURL = strings.TrimSpace(githubURL)
	if githubURL == "" {
		return "", fmt.Errorf("empty repo URL")
	}
	if k, _ := gitremote.Classify(githubURL); k == gitremote.KindInvalid {
		return "", fmt.Errorf("invalid repository remote %q", gitremote.Redact(githubURL, token))
	}
	url := gitremote.AuthenticatedURL(githubURL, token)

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "ls-remote", "--symref", "--", url, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote --symref: %w", err)
	}
	// Expected output:
	//   ref: refs/heads/master\tHEAD
	//   <sha>\tHEAD
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref:") {
			continue
		}
		// Strip "ref:" prefix and trailing \tHEAD.
		rest := strings.TrimSpace(strings.TrimPrefix(line, "ref:"))
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		ref := fields[0]
		if name := strings.TrimPrefix(ref, "refs/heads/"); name != ref && name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not parse default branch from `git ls-remote --symref` output for %s", githubURL)
}
