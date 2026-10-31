package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID           = "research-coordinator"
	consumerGroup     = "research-coordinator"
	healthPort        = 8092
	defaultTimeBudget = 30 * time.Minute
)

// prdReadyMessage is received from prd.ready.
type prdReadyMessage struct {
	WorkID       string `json:"work_id"`
	ProductID    string `json:"product_id"`
	WorkType     string `json:"work_type"`
	PRDContent   string `json:"prd_content"`
	UserGuidance string `json:"user_guidance"`
	Reason       string `json:"reason"`
}

// Status values mirrored from research-agent. A result with Status != "ok"
// (including the empty/missing legacy case) means the task did not complete
// honestly and must not be treated as successful research.
const (
	ResearchStatusOK       = "ok"
	ResearchStatusLimitHit = "limit_hit"
	ResearchStatusTimedOut = "timed_out"
	ResearchStatusError    = "error"
)

// researchResultMessage is received from research.results. The Status field
// is the primary success/failure signal; evidence MUST NOT be passed to
// synthesis unless Status == "ok". Legacy messages without a Status field
// decode as empty string and are treated as failures (the old code dropped
// tool-loop-safety-limit errors as evidence-looking payloads with confidence
// 0.1 and they got counted toward convergence).
//
// Affected*/Relevant* are the structured sidecar produced by research-agent
// from the tool-call history. We union-dedupe them across all 5 tasks into
// the technical brief so downstream agents inherit concrete identifiers
// instead of re-extracting file paths from prose. Empty slices from older
// agent builds (before structured capture landed) are tolerated.
type researchResultMessage struct {
	TaskID              string          `json:"task_id"`
	WorkID              string          `json:"work_id"`
	Status              string          `json:"status"` // "ok" | "limit_hit" | "timed_out" | "error"
	FindingType         string          `json:"finding_type"`
	Evidence            string          `json:"evidence"`
	Confidence          json.RawMessage `json:"confidence"` // accepts string or number
	FollowupSuggestions string          `json:"followup_suggestions"`
	ErrorDetail         string          `json:"error_detail,omitempty"`

	// Criteria are the task's acceptance criteria (M2-04); merged into the
	// brief by mergeBriefCriteria (criteria.go).
	Criteria *plan.BriefCriteria `json:"criteria,omitempty"`

	AffectedFiles        []string `json:"affected_files,omitempty"`
	AffectedModules      []string `json:"affected_modules,omitempty"`
	AffectedZones        []string `json:"affected_zones,omitempty"`
	CrossRepoTouchpoints []string `json:"cross_repo_touchpoints,omitempty"`
	RelevantPatterns     []string `json:"relevant_patterns,omitempty"`
	RelevantAntiPatterns []string `json:"relevant_anti_patterns,omitempty"`

	// BaseSHAs is repository id → the pinned commit the task read (its
	// snapshot worktree, M2-03); CodexVersion the Codex version it was
	// grounded on: the product_version of CodexBranch (migration 060).
	// All absent from older research-agent builds; a CodexVersion without a
	// CodexBranch is a pre-060 global version_number (briefCodexPin).
	BaseSHAs     map[string]string `json:"base_shas,omitempty"`
	CodexVersion int               `json:"codex_version,omitempty"`
	CodexBranch  string            `json:"codex_branch,omitempty"`
}

// technicalBrief is the artifact content we save to work_item_artifacts
// and whose prose half is also published on technical-brief.ready. The
// `brief` / `narrative` prose field is kept under both "brief" (legacy
// key, consumed by planner) and "narrative" (new canonical key) so
// Agent C's planner changes can migrate without breaking this pipeline.
type technicalBrief struct {
	// Prose synthesis from the LLM. Kept under two keys for backwards
	// compatibility — see the artifact-save site for rationale.
	Brief     string `json:"brief"`
	Narrative string `json:"narrative"`
	WorkID    string `json:"work_id"`

	AffectedFiles        []string `json:"affected_files"`
	AffectedModules      []string `json:"affected_modules"`
	AffectedZones        []string `json:"affected_zones"`
	CrossRepoTouchpoints []string `json:"cross_repo_touchpoints"`
	RelevantPatterns     []string `json:"relevant_patterns"`
	RelevantAntiPatterns []string `json:"relevant_anti_patterns"`

	// Criteria are the merged acceptance criteria and file evidence map
	// (M2-04). Omitted when no research result carried criteria.
	Criteria *plan.BriefCriteria `json:"criteria,omitempty"`

	// BaseSHAs maps repository id → the pinned base commit the research was
	// dispatched against (M2-02). Omitted for a legacy-unpinned item.
	BaseSHAs map[string]string `json:"base_shas,omitempty"`
	// CodexVersion / CodexBranch are the codex the brief rests on: the
	// product_version of CodexBranch (migration 060), as pinned in
	// work_item_codex_pins (briefCodexPin). Omitted when nothing was pinned.
	CodexVersion int    `json:"codex_version,omitempty"`
	CodexBranch  string `json:"codex_branch,omitempty"`
	// LineSeeds are the places to edit research found, with the lines
	// quoted (R10 W5, line_seeds.go; at most 8). Additive.
	LineSeeds []lineSeed `json:"line_seeds,omitempty"`
	// PRRequirements are the intake's sentences (work item description +
	// PRD) that ask for something to be stated in the PR text
	// (plan.PRTextRequirements, R15 T2), verbatim; the planner copies them
	// onto every subtask and pr-coordinator lists them in the PR
	// description. Additive.
	PRRequirements []string `json:"pr_requirements,omitempty"`
}

// researchTaskMessage is published to research.tasks.
type researchTaskMessage struct {
	TaskID          string    `json:"task_id"`
	WorkID          string    `json:"work_id"`
	ProductID       string    `json:"product_id"`
	RepositoryID    string    `json:"repository_id,omitempty"`
	ResearchType    string    `json:"research_type"`
	StartingContext string    `json:"starting_context"`
	CodexSections   string    `json:"codex_sections"`
	RepoList        []repoRef `json:"repo_list,omitempty"`

	// SeedFiles are repository-relative files the intake or the PRD names
	// (R2, seeds.go), with SeedLines the line numbers given next to them.
	// SeedsUnverified: no clone or Codex could confirm them. Additive.
	SeedFiles       []string         `json:"seed_files,omitempty"`
	SeedLines       map[string][]int `json:"seed_lines,omitempty"`
	SeedsUnverified bool             `json:"seeds_unverified,omitempty"`
	// BudgetHintTokens is this task's share of what is left of the work
	// item's token budget when it was dispatched: (WORK_ITEM_TOKEN_BUDGET -
	// spent) / tasks dispatched. Absent when the spend could not be read.
	BudgetHintTokens *int64 `json:"budget_hint_tokens,omitempty"`
}

// repoRef is a minimal reference to a repository for research tasks.
type repoRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ClonePath string `json:"clone_path"`
	// BaseSHA is the repository's pinned base commit for this work item
	// (M2-02); empty when the item is legacy-unpinned. Additive.
	BaseSHA string `json:"base_sha,omitempty"`
}

// technicalBriefReadyMessage is published to technical-brief.ready.
type technicalBriefReadyMessage struct {
	WorkID       string `json:"work_id"`
	ProductID    string `json:"product_id"`
	BriefContent string `json:"brief_content"`
	Timestamp    string `json:"timestamp"`
	// Criteria mirror the artifact's (M2-04); additive.
	Criteria *plan.BriefCriteria `json:"criteria,omitempty"`
}

// researchSession tracks the state of research for a single work item.
type researchSession struct {
	workID      uuid.UUID
	aggregateID uuid.UUID // outbox aggregate: the parent work item, or workID itself
	productID   string
	workType    string
	prdContent  string
	repoList    []repoRef
	// seeds and intakeText are the intake's seed set (R2) and its
	// description: the line-seed fallback and hints (R11 S1).
	seeds        seedSet
	intakeText   string
	taskIDs      map[string]bool
	results      []researchResultMessage
	startedAt    time.Time
	assignmentID uuid.UUID // ID of the agent_assignments row for this session
	dispatched   bool      // set after tasks are published — prevents duplicate dispatch on redelivery
	mu           sync.Mutex
}

// coordinator manages active research sessions.
type coordinator struct {
	sessions map[string]*researchSession
	mu       sync.Mutex
}

func newCoordinator() *coordinator {
	return &coordinator{sessions: make(map[string]*researchSession)}
}

func (c *coordinator) getOrCreate(workID uuid.UUID) *researchSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := workID.String()
	if s, ok := c.sessions[key]; ok {
		return s
	}
	s := &researchSession{
		workID:    workID,
		taskIDs:   make(map[string]bool),
		startedAt: time.Now(),
	}
	c.sessions[key] = s
	return s
}

func (c *coordinator) remove(workID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, workID.String())
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("research-coordinator: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("research-coordinator: connect db: %v", err)
	}
	defer pool.Close()

	// Wire prompt store so strategy layer resolves prompts from DB.
	strategy.SetPromptStore(prompts.NewStore(pool))

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("research-coordinator: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
	cx := codex.New(pool)
	ps := products.NewStore(pool)
	registry := agentregistry.New(pool)

	var cs *configstore.ConfigStore
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr != "" {
		mk, err := crypto.LoadMasterKey(masterKeyStr)
		if err == nil {
			cs = configstore.New(pool, mk)
			// Listen on pg_notify('config_changed') so UI updates to credentials/
			// endpoints flush the in-memory cache without a restart.
			cs.StartWatcher(ctx)
		}
	}

	coord := newCoordinator()

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04). research.tasks, research.additional,
	// technical-brief.ready and work.blocked are committed to outbox_events
	// with the state they announce; this loop carries them to Kafka, so a
	// failure to start is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), agentID)
	if err != nil {
		log.Fatalf("research-coordinator: start outbox dispatcher: %v", err)
	}
	log.Printf("research-coordinator: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	topics := []string{bus.TopicPRDReady, bus.TopicResearchResults}
	// M1-05: deduplicate on the processed-event ledger (see the spec).
	handler := idempotency.NewLedger(pool).Handler("research-coordinator", buildHandler(ctx, pool, cfg.Qdrant.URL, wg, cx, ps, registry, cs, kb, coord))
	if err := kb.SubscribeMultiple(topics, consumerGroup, handler); err != nil {
		log.Fatalf("research-coordinator: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("research-coordinator: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("research-coordinator: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("research-coordinator: shutting down...")
	cancel()
	dispatcher.Wait()
	log.Println("research-coordinator: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("research-coordinator: health server shutdown error: %v", err)
	}
	log.Println("research-coordinator: stopped")
}

func buildHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	qdrantURL string,
	wg *workgraph.WorkGraph,
	cx *codex.Codex,
	ps *products.ProductStore,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	coord *coordinator,
) bus.Handler {
	rounds := &pgRoundStore{pool: pool}
	return func(msgCtx context.Context, msg *bus.Message) error {
		switch msg.Topic {
		case bus.TopicPRDReady:
			return handlePRDReady(ctx, msgCtx, pool, qdrantURL, wg, cx, ps, registry, cs, kb, coord, rounds, msg)
		case bus.TopicResearchResults:
			return handleResearchResult(ctx, msgCtx, pool, wg, registry, cs, kb, coord, rounds, msg)
		default:
			log.Printf("research-coordinator: unhandled topic %s", msg.Topic)
			return nil
		}
	}
}

func handlePRDReady(
	ctx context.Context,
	msgCtx context.Context,
	pool *pgxpool.Pool,
	qdrantURL string,
	wg *workgraph.WorkGraph,
	cx *codex.Codex,
	ps *products.ProductStore,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	coord *coordinator,
	rounds roundStore,
	msg *bus.Message,
) error {
	var payload prdReadyMessage
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("research-coordinator: prd.ready: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		log.Printf("research-coordinator: prd.ready: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}

	log.Printf("research-coordinator: received prd.ready for work_id=%s product_id=%s", workID, payload.ProductID)

	// Record agent assignment for this work item.
	assignment := &models.AgentAssignment{
		WorkItemID:      workID,
		AgentType:       models.AgentTypeResearchCoordinator,
		AgentInstanceID: agentID,
		Status:          models.AssignmentStatusActive,
	}
	if err := wg.AssignAgent(msgCtx, assignment); err != nil {
		log.Printf("research-coordinator: assign agent: %v", err)
	}

	// Collect repos for this product so research agents know what to search.
	var repoList []repoRef
	var productID *uuid.UUID
	if payload.ProductID != "" {
		if pid, parseErr := uuid.Parse(payload.ProductID); parseErr == nil {
			productID = &pid
			repos, listErr := ps.GetRepositoriesForProduct(msgCtx, pid)
			if listErr != nil {
				log.Printf("research-coordinator: list repos for product %s: %v", payload.ProductID, listErr)
			} else {
				for _, r := range repos {
					if r.Enabled {
						repoList = append(repoList, repoRef{
							ID:        r.ID.String(),
							Name:      r.Name,
							ClonePath: r.ClonePath,
						})
					}
				}
			}
		}
	}

	// Pick the repo path to use for availability checks.
	// When product repos aren't available (e.g. QA bug loop re-entry without
	// product_id), fall back to the work item's repository_id clone path.
	repoPath := ""
	if len(repoList) > 0 {
		repoPath = repoList[0].ClonePath
	}
	if repoPath == "" {
		// Try resolving from the work item's repository_id directly.
		var clonePath string
		err := pool.QueryRow(msgCtx, `
			SELECT r.clone_path
			FROM repositories r
			JOIN work_items w ON w.repository_id = r.id
			WHERE w.id = $1
		`, workID).Scan(&clonePath)
		if err == nil && clonePath != "" {
			repoPath = clonePath
			// Also populate repoList so research agents have repo context.
			var repoID, repoName string
			_ = pool.QueryRow(msgCtx, `
				SELECT r.id::text, r.name
				FROM repositories r
				JOIN work_items w ON w.repository_id = r.id
				WHERE w.id = $1
			`, workID).Scan(&repoID, &repoName)
			if repoID != "" {
				repoList = append(repoList, repoRef{
					ID:        repoID,
					Name:      repoName,
					ClonePath: clonePath,
				})
			}
		} else {
			log.Printf("research-coordinator: no repo found for work_id=%s via product or work item", workID)
		}
	}

	// M2-02: each repository's pinned base commit rides on repo_list
	// (additive; M2-03 decides how research uses it).
	applyRepoPins(msgCtx, pool, workID, repoList)

	// Validate context before dispatching research tasks (contextGate). The
	// codex check is product-scoped and uses the one grounding gate of every
	// stage (guards.CheckCodexGrounded, codex coherence Part 3): at least one
	// module; patterns or anti-patterns alone are not grounding. If
	// productID is missing we block (rather than fall back to a
	// cross-product global query). The work item's target branch scopes the
	// gate and the context payload; empty means the product's default
	// branch.
	branch := ""
	wi, wiErr := wg.GetWorkItem(msgCtx, workID)
	if wiErr != nil {
		wi = nil
	}
	if wi != nil && wi.Branch != nil {
		branch = *wi.Branch
	}
	aggregateID := aggregateFor(workID, wi)

	// Codex coherence Part 3: the one grounding gate (CheckCodexGrounded)
	// inside contextGate; every failure escalates through work.blocked.
	others := guards.RunAll(
		guards.CheckRepoAvailable(repoPath),
		guards.CheckVectorStorePopulated(msgCtx, qdrantURL, "codebase"),
	)
	if refused, gerr := contextGate(msgCtx, pool, aggregateID, workID, productID, branch, msg, others); refused || gerr != nil {
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		if gerr != nil && !idempotency.IsDuplicate(gerr) {
			return fmt.Errorf("refuse work item %s at context validation: %w", workID, gerr)
		}
		return nil
	}

	// Guard: refuse to run research with no PRD. Without a PRD the
	// research agents have no direction and produce generic codebase
	// analysis that wastes LLM time and yields garbage plans downstream.
	if strings.TrimSpace(payload.PRDContent) == "" {
		reason := "Research cannot proceed — no PRD was generated for this work item. " +
			"The retry path may have skipped PRD generation. Please retry the work item so " +
			"the product-agent generates a PRD first."
		log.Printf("research-coordinator: blocking work_id=%s — empty PRD content", workID)
		if err := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); err != nil {
			log.Printf("research-coordinator: block work item: %v", err)
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}

	session := coord.getOrCreate(workID)
	session.mu.Lock()
	if session.dispatched {
		session.mu.Unlock()
		log.Printf("research-coordinator: ignoring duplicate prd.ready for work_id=%s (already dispatched)", workID)
		return nil
	}
	session.mu.Unlock()

	// Build Codex context — product-scoped. Skip grounding when productID
	// is absent so research agents don't receive cross-product modules.
	codexSections := ""
	if productID != nil {
		codexSections, err = cx.BuildContextForIntentBranch(msgCtx, *productID, branch)
		if err != nil {
			// The stage stops (escalation) rather than research without
			// the codex.
			log.Printf("research-coordinator: build codex context for work_id=%s: %v — refusing", workID, err)
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			if rerr := refuseCodex(msgCtx, pool, aggregateID, workID, *productID, branch, guards.ContextErrorReason(*productID, err), msg); rerr != nil && !idempotency.IsDuplicate(rerr) {
				return fmt.Errorf("refuse work item %s after a codex context error: %w", workID, rerr)
			}
			return nil
		}
	} else {
		log.Printf("research-coordinator: work_id=%s has no product_id — skipping codex grounding", workID)
	}

	// Seeds (R2): files the intake description or the PRD names, checked
	// against the repository clone (then the Codex), so research opens them
	// first instead of rediscovering them.
	description := ""
	if wi != nil {
		description = wi.Description
	}
	seeds := resolveSeeds(msgCtx, pool, productID, repoList, description, payload.PRDContent)
	if len(seeds.Files) > 0 {
		log.Printf("research-coordinator: work_id=%s seeds (%s): %v lines=%v", workID, seeds.Source, seeds.Files, seeds.Lines)
	}

	// Fan-out scaled to the work (R2, fanout.go).
	criteriaCount := countAcceptanceCriteria(payload.PRDContent)
	taskTypes := selectResearchTypes(payload.WorkType, seeds.Files, !seeds.Unverified, criteriaCount, os.Getenv)
	log.Printf("research-coordinator: work_id=%s work_type=%s seeds=%d acceptance_criteria=%d → research types %v",
		workID, payload.WorkType, len(seeds.Files), criteriaCount, taskTypes)

	// Set the primary repository_id so research agents know which repo to focus on.
	primaryRepoID := ""
	if len(repoList) > 0 {
		primaryRepoID = repoList[0].ID
	}

	// Build starting context: PRD + optional user guidance from the approval.
	startingContext := payload.PRDContent
	if payload.UserGuidance != "" {
		startingContext = payload.PRDContent +
			"\n\n---\n## User Guidance (from PRD approval)\n" +
			payload.UserGuidance
	}

	return dispatchResearchRound(msgCtx, pool, wg, registry, cs, kb, coord, rounds, dispatchInput{
		workID:          workID,
		aggregateID:     aggregateID,
		assignmentID:    assignment.ID,
		payload:         payload,
		startingContext: startingContext,
		primaryRepoID:   primaryRepoID,
		codexSections:   codexSections,
		repoList:        repoList,
		seeds:           seeds,
		description:     description,
		taskTypes:       taskTypes,
	})
}

// dispatchInput is what handlePRDReady has resolved before a round is
// planned and dispatched.
type dispatchInput struct {
	workID          uuid.UUID
	aggregateID     uuid.UUID
	assignmentID    uuid.UUID
	payload         prdReadyMessage
	startingContext string
	primaryRepoID   string
	codexSections   string
	repoList        []repoRef
	seeds           seedSet
	// description is the work item's intake text (R11 S1 hints).
	description string
	taskTypes   []string
}

// dispatchResearchRound plans the round (reusing earlier ok results for the
// same PRD hash), commits it with the transition and its research.tasks
// events, and — when every type was reused — synthesizes straight away.
func dispatchResearchRound(
	msgCtx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	coord *coordinator,
	rounds roundStore,
	in dispatchInput,
) error {
	workID, aggregateID, payload := in.workID, in.aggregateID, in.payload
	seeds, repoList := in.seeds, in.repoList

	// Reuse on retry (R2): an earlier round's ok results for the same work
	// item and the same PRD hash are carried into this round; only the
	// failed or missing research types are dispatched.
	hash := prdHash(payload.PRDContent)
	roundPlan, planErr := prepareRound(msgCtx, rounds, workID, hash, in.taskTypes)
	if planErr != nil {
		log.Printf("research-coordinator: work_id=%s: read reusable research results: %v — dispatching every type", workID, planErr)
	}
	for _, reused := range roundPlan.Reused {
		log.Printf("research-coordinator: work_id=%s reusing ok %s result of task %s (same PRD hash)", workID, reused.ResearchType, reused.ReusedFrom)
	}

	// Budget visibility (R2): each task's share of what is left of the work
	// item's budget. The budget itself is unchanged.
	var budgetHint *int64
	if len(roundPlan.Dispatch) > 0 {
		if spent, err := workItemTokensSpent(msgCtx, pool, workID); err != nil {
			log.Printf("research-coordinator: work_id=%s: read token spend: %v — no budget hint", workID, err)
		} else {
			h := budgetHintTokens(tools.TokenBudget(), spent, len(roundPlan.Dispatch))
			budgetHint = &h
		}
	}

	// Build every research task first — include repo list for multi-repo
	// awareness. Nothing is written yet.
	tasks := make([]researchTaskMessage, 0, len(roundPlan.Dispatch))
	for _, taskType := range roundPlan.Dispatch {
		tasks = append(tasks, researchTaskMessage{
			TaskID:           uuid.New().String(),
			WorkID:           workID.String(),
			ProductID:        payload.ProductID,
			RepositoryID:     in.primaryRepoID,
			ResearchType:     taskType,
			StartingContext:  in.startingContext,
			CodexSections:    in.codexSections,
			RepoList:         repoList,
			SeedFiles:        seeds.Files,
			SeedLines:        seeds.Lines,
			SeedsUnverified:  seeds.Unverified,
			BudgetHintTokens: budgetHint,
		})
	}

	// The round is persisted with the dispatch: its PRD hash and its whole
	// task set (dispatched and reused), so convergence survives a restart.
	round := &researchRound{
		ID:           uuid.New(),
		WorkID:       workID,
		PRDHash:      hash,
		WorkType:     payload.WorkType,
		ProductID:    payload.ProductID,
		PRDContent:   payload.PRDContent,
		AggregateID:  aggregateID,
		AssignmentID: in.assignmentID,
		RepoList:     repoList,
		Status:       roundOpen,
		CreatedAt:    time.Now(),
	}
	for _, task := range tasks {
		round.Tasks = append(round.Tasks, roundTask{TaskID: task.TaskID, ResearchType: task.ResearchType, Status: taskPending})
	}
	round.Tasks = append(round.Tasks, roundPlan.Reused...)
	allReused := len(tasks) == 0 && len(round.Tasks) > 0
	if allReused {
		// Nothing to wait for: this handler synthesizes right after the commit.
		round.Status = roundSynthesizing
	}

	// M1-05: the dispatch transaction carries the processed-event guard row
	// — except on the all-reused path. There the dispatch commits nothing to
	// wait for and synthesis follows in this handler, so the guard row goes
	// into the technical brief's transaction (or the block's) instead: a
	// crash during synthesis leaves prd.ready unrecorded, and its redelivery
	// dispatches again, superseding the stranded "synthesizing" round.
	dispatchDB := idempotency.Wrap(msgCtx, pool)
	if allReused {
		dispatchDB = idempotency.Wrap(idempotency.Unguarded(msgCtx), pool)
	}

	// Commit the transition to research and every research.tasks event
	// together, then register the tasks in the session. The session lock is
	// held across the commit so a duplicate prd.ready cannot dispatch a
	// second set in the meantime, and a result for this work item cannot be
	// counted against a session that does not know its tasks yet.
	session := coord.getOrCreate(workID)
	session.mu.Lock()
	if session.dispatched {
		session.mu.Unlock()
		log.Printf("research-coordinator: ignoring duplicate prd.ready for work_id=%s (already dispatched)", workID)
		return nil
	}
	if err := commitResearchDispatch(msgCtx, dispatchDB, wg, aggregateID, workID, tasks, round); err != nil {
		session.mu.Unlock()
		coord.remove(workID)
		if idempotency.IsDuplicate(err) {
			log.Printf("research-coordinator: prd.ready for work_id=%s already dispatched — duplicate delivery", workID)
			return nil
		}
		reason := fmt.Sprintf("Research dispatch could not be recorded: %v. No research tasks were sent; retry the work item.", err)
		log.Printf("research-coordinator: commit research dispatch for work_id=%s: %v", workID, err)
		if bErr := blockInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason); bErr != nil {
			log.Printf("research-coordinator: block work item: %v", bErr)
		}
		wg.CompleteAssignment(msgCtx, in.assignmentID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	session.aggregateID = aggregateID
	session.workType = payload.WorkType
	session.prdContent = payload.PRDContent
	session.productID = payload.ProductID
	session.repoList = repoList
	session.seeds = seeds
	session.intakeText = in.description
	session.assignmentID = in.assignmentID
	for _, task := range tasks {
		session.taskIDs[task.TaskID] = false // false = pending
		log.Printf("research-coordinator: dispatched task %s type=%s for work_id=%s", task.TaskID, task.ResearchType, workID)
	}
	session.dispatched = true
	session.mu.Unlock()

	if allReused {
		log.Printf("research-coordinator: work_id=%s: every research type reused from an earlier round — synthesizing without dispatch", workID)
		return finishAllReused(msgCtx, pool, wg, registry, cs, kb, coord, rounds, round, false)
	}
	return nil
}

// finishAllReused is finishRound; a var only so the integration test can
// simulate a crash between the all-reused dispatch and its synthesis.
var finishAllReused = finishRound

// resolveSeeds extracts the seed files from texts and validates them
// against the repository clones, falling back to the Codex (read only when
// no clone is on disk).
func resolveSeeds(ctx context.Context, pool *pgxpool.Pool, productID *uuid.UUID, repoList []repoRef, texts ...string) seedSet {
	cands := extractSeedCandidates(texts...)
	if len(cands) == 0 {
		return seedSet{}
	}
	clones := make([]string, 0, len(repoList))
	for _, r := range repoList {
		clones = append(clones, r.ClonePath)
	}
	var cx seedCodex
	if productID != nil && pool != nil {
		pid := *productID
		cx = &lazySeedCodex{load: func() seedCodex { return loadCodexSeedIndex(ctx, pool, pid) }}
	}
	return validateSeeds(cands, clones, cx)
}

// lazySeedCodex loads the Codex index on first use.
type lazySeedCodex struct {
	load   func() seedCodex
	once   sync.Once
	loaded seedCodex
}

func (l *lazySeedCodex) Knows(p string) bool {
	l.once.Do(func() { l.loaded = l.load() })
	return l.loaded != nil && l.loaded.Knows(p)
}

// researchTaskEvents builds one research.tasks outbox event per task, keyed by
// task id as the direct publish was, all under the work item's aggregate so
// they leave the outbox in dispatch order.
func researchTaskEvents(aggregateID uuid.UUID, tasks []researchTaskMessage) ([]outbox.Event, error) {
	events := make([]outbox.Event, 0, len(tasks))
	for _, task := range tasks {
		ev, err := outbox.NewEvent(outbox.AggregateTypeWorkItem, aggregateID, bus.TopicResearchTasks, task.TaskID, task)
		if err != nil {
			return nil, fmt.Errorf("build research.tasks event (type=%s): %w", task.ResearchType, err)
		}
		events = append(events, ev)
	}
	return events, nil
}

// commitResearchDispatch moves the work item to research and enqueues every
// research.tasks event in one transaction: either the item is in research
// with all of its tasks on their way, or nothing changed.
//
// The PRD approval paths in dashboard-backend transition the item to research
// themselves before publishing prd.ready, so finding it already in research
// is the normal case and not an error: the row lock is still taken, nothing
// is rewritten, and the tasks are enqueued. Any other invalid transition
// (the item was cancelled or blocked meanwhile) rolls the dispatch back.
//
// round (R2), when given, is persisted in the same transaction: the round
// exists exactly when its tasks were sent.
func commitResearchDispatch(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, aggregateID, workID uuid.UUID, tasks []researchTaskMessage, round *researchRound) error {
	events, err := researchTaskEvents(aggregateID, tasks)
	if err != nil {
		return err
	}
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		from, err := wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStateResearch, agentID, "research-coordinator dispatching tasks")
		if err != nil && !(errors.Is(err, workgraph.ErrInvalidTransition) && from == models.WorkItemStateResearch) {
			return fmt.Errorf("transition to research: %w", err)
		}
		if err := insertRoundTx(ctx, tx, round); err != nil {
			return err
		}
		return outboxEnqueueAll(ctx, tx, events...)
	})
}

// outboxEnqueueAll is the fan-out's enqueue. It is a var only so the
// integration test can fail it after the transition has been written and
// prove the transition rolls back with it.
var outboxEnqueueAll = outbox.EnqueueAll

// commitTechnicalBrief saves the technical brief artifact, enqueues
// technical-brief.ready and completes the coordinator's assignment in one
// transaction. There is no state transition here: research → planning is the
// implementation-planner's, on receipt of this event. The research round
// (R2) is closed as completed in the same transaction, so a crash after the
// commit cannot leave it claimable for a second synthesis. codexPin, when
// set, is the work item's codex pin (migration 060), written (replacing an
// earlier brief's) in the same transaction; nil leaves the item unpinned.
func commitTechnicalBrief(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, aggregateID uuid.UUID, artifact *models.WorkItemArtifact, msg technicalBriefReadyMessage, assignmentID, roundID uuid.UUID, codexPin *codex.Pin) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if codexPin != nil {
			if err := codexPins.PinTx(ctx, tx, *codexPin); err != nil {
				return err
			}
		}
		if err := wg.AddArtifactTx(ctx, tx, artifact); err != nil {
			return err
		}
		if err := outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicTechnicalBriefReady, msg.WorkID, msg); err != nil {
			return err
		}
		if err := closeRoundTx(ctx, tx, roundID, roundCompleted); err != nil {
			return err
		}
		if assignmentID == uuid.Nil {
			return nil
		}
		return wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusCompleted)
	})
}

// enqueueFollowup puts a research.additional task on the outbox in a short
// transaction of its own. There is no row write to share it with, but going
// through the outbox gives it the same retries and dead-lettering as the
// fan-out, and keeps it ordered behind the work item's earlier events. The
// follow-up joins its round's persisted task set in the same transaction, so
// the round waits for it. The round row is locked first and the follow-up is
// skipped (errRoundNotOpen) unless the round is still open: a round already
// claimed for synthesis, closed or superseded must not gain a task.
func enqueueFollowup(ctx context.Context, db workgraph.TxBeginner, aggregateID, roundID uuid.UUID, task researchTaskMessage) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := lockOpenRoundTx(ctx, tx, roundID); err != nil {
			return err
		}
		if err := insertRoundTaskTx(ctx, tx, roundID, roundTask{TaskID: task.TaskID, ResearchType: task.ResearchType, Status: taskPending, Followup: true}); err != nil {
			return err
		}
		return outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicResearchAdditional, task.TaskID, task)
	})
}

// blockInOwnTx blocks the work item in a short transaction of its own: the
// transition to blocked and its work.blocked event commit together.
func blockInOwnTx(ctx context.Context, db workgraph.TxBeginner, aggregateID, workID uuid.UUID, reason string) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, aggregateID, workID, agentID, reason)
	})
}

// aggregateFor is the outbox aggregate for a work item's events: its parent
// when it has one, otherwise the item itself (common3 outbox rule).
func aggregateFor(workID uuid.UUID, wi *models.WorkItem) uuid.UUID {
	if wi != nil && wi.ParentID != nil && *wi.ParentID != uuid.Nil {
		return *wi.ParentID
	}
	return workID
}

// roundAggregate is the round's outbox aggregate, resolved from the work
// item when the round has none recorded.
func roundAggregate(ctx context.Context, wg *workgraph.WorkGraph, round *researchRound) uuid.UUID {
	if round.AggregateID != uuid.Nil {
		return round.AggregateID
	}
	wi, err := wg.GetWorkItem(ctx, round.WorkID)
	if err != nil {
		wi = nil
	}
	round.AggregateID = aggregateFor(round.WorkID, wi)
	return round.AggregateID
}

func handleResearchResult(
	ctx context.Context,
	msgCtx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	coord *coordinator,
	rounds roundStore,
	msg *bus.Message,
) error {
	var result researchResultMessage
	if err := json.Unmarshal(msg.Value, &result); err != nil {
		log.Printf("research-coordinator: research.results: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(result.WorkID)
	if err != nil {
		log.Printf("research-coordinator: research.results: parse work_id %q: %v", result.WorkID, err)
		return nil
	}

	// B2 (R2): the result is recorded on its persisted round and the round's
	// completion is judged against the task set dispatched for it — never
	// against what this process happens to have seen since it started.
	//
	// A failed record is retried in process and then blocks the item (the
	// F2 rule: the bus does not redeliver a failed message in process, so
	// returning the error would bury it and leave the round unconverged).
	// The error is returned only when the block failed too.
	decision, round, handled, err := recordOrBlock(msgCtx, rounds, result, func(reason string) error {
		wi, gErr := wg.GetWorkItem(msgCtx, workID)
		if gErr != nil {
			wi = nil
		}
		return blockInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateFor(workID, wi), workID, reason)
	})
	if handled {
		return err
	}
	if !decision.Known {
		log.Printf("research-coordinator: result task=%s work_id=%s belongs to no persisted research round — dropped (not counted toward any round)", result.TaskID, workID)
		return nil
	}
	if round.WorkID != workID {
		log.Printf("research-coordinator: result task=%s claims work_id=%s but its round is for %s — using the round's", result.TaskID, workID, round.WorkID)
		workID = round.WorkID
	}

	log.Printf("research-coordinator: result received task=%s work_id=%s status=%s pending=%d/%d time_exceeded=%v",
		result.TaskID, workID, result.Status, decision.Pending, decision.Total, decision.TimeExceeded)

	if !decision.Claimed {
		if decision.Converged || decision.TimeExceeded || round.Status != roundOpen {
			// Another result completed (or the time budget closed) this
			// round, or it was superseded by a newer one; the result is
			// recorded and stays reusable.
			return nil
		}
		// Evaluate whether follow-up is needed.
		if result.FollowupSuggestions != "" {
			llmSvc, err := llm.NewServiceForWorkItem(msgCtx, registry, workID, cs, "research-coordinator", pool)
			if err != nil {
				log.Printf("research-coordinator: resolve LLM service for work_id=%s: %v — no follow-up evaluation", workID, err)
				return nil
			}
			evalIntent := strategy.Intent{
				Type: strategy.IntentEvaluateFindings,
				Context: map[string]string{
					"finding":              result.Evidence,
					"followup_suggestions": result.FollowupSuggestions,
					"confidence":           string(result.Confidence),
					"work_item_id":         workID.String(),
				},
			}
			evalResult, err := llmSvc.Execute(msgCtx, evalIntent)
			if err != nil {
				log.Printf("research-coordinator: evaluate findings: %v", err)
			} else if evalResult != nil && len(evalResult.Content) > 0 {
				// Spawn follow-up task if warranted.
				followupID := uuid.New().String()

				// Determine primary repo ID for the follow-up task.
				followupPrimaryRepoID := ""
				if len(round.RepoList) > 0 {
					followupPrimaryRepoID = round.RepoList[0].ID
				}

				followup := researchTaskMessage{
					TaskID:          followupID,
					WorkID:          workID.String(),
					ProductID:       round.ProductID,
					RepositoryID:    followupPrimaryRepoID,
					ResearchType:    result.FindingType,
					StartingContext: result.FollowupSuggestions,
					CodexSections:   "",
					RepoList:        round.RepoList,
				}
				// The follow-up joins the round's persisted task set only if
				// its event commits — a task that was never sent must not
				// hold convergence open.
				aggregateID := roundAggregate(msgCtx, wg, round)
				if err := enqueueFollowup(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, round.ID, followup); errors.Is(err, errRoundNotOpen) {
					log.Printf("research-coordinator: work_id=%s: follow-up skipped — %v", workID, err)
				} else if err != nil {
					log.Printf("research-coordinator: enqueue research.additional for work_id=%s: %v", workID, err)
				}
			}
		}
		return nil
	}

	return finishRound(msgCtx, pool, wg, registry, cs, kb, coord, rounds, round, decision.TimeExceeded)
}

// judgeResearchResults is the honest-failure gate's count: every result
// whose status is not "ok" (limit_hit, timed_out, error, loop_stuck,
// no_evidence, pending, missing — no status is special) is a failed task,
// summarised with its error detail; transient counts the failures whose
// detail looks like a provider hiccup.
func judgeResearchResults(results []researchResultMessage) (failed []string, transient, ok int) {
	for _, r := range results {
		if r.Status == ResearchStatusOK {
			ok++
			continue
		}
		detail := r.ErrorDetail
		if detail == "" {
			detail = "(no error detail)"
		}
		status := r.Status
		if status == "" {
			status = "missing"
		}
		failed = append(failed, fmt.Sprintf("task %s [%s] status=%s: %s", r.TaskID, r.FindingType, status, detail))
		if strings.Contains(detail, "status 429") || strings.Contains(detail, "unexpected EOF") || strings.Contains(detail, "context deadline") {
			transient++
		}
	}
	return failed, transient, ok
}

// finishRound synthesizes the technical brief from a round this handler
// claimed (every task recorded, or the time budget ran out), or blocks the
// work item when the research did not complete honestly. The round is closed
// either way.
func finishRound(
	msgCtx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	coord *coordinator,
	rounds roundStore,
	round *researchRound,
	timeBudgetExceeded bool,
) error {
	workID := round.WorkID
	outcome := roundBlocked
	defer func() {
		if err := rounds.Close(context.WithoutCancel(msgCtx), round.ID, outcome); err != nil {
			log.Printf("research-coordinator: work_id=%s: %v", workID, err)
		}
		// Clean up session.
		coord.remove(workID)
	}()

	results := round.results()
	// The gate also sees every task still pending (time budget) as failed.
	gateResults := round.gateResults()
	taskCount := len(round.Tasks)
	prdContent := round.PRDContent
	assignmentID := round.AssignmentID
	productID := round.ProductID

	// Synthesize technical brief.
	log.Printf("research-coordinator: converged for work_id=%s tasks=%d results=%d time_exceeded=%v", workID, taskCount, len(results), timeBudgetExceeded)

	// Resolve LLM service per work item for synthesis.
	llmSvc, err := resolveBriefLLM(msgCtx, registry, workID, cs, pool)
	if err != nil {
		log.Printf("research-coordinator: resolve LLM service for work_id=%s: %v — proceeding without LLM", workID, err)
		llmSvc = nil
	}

	// Honest-failure gate: if ANY result has Status != "ok" (or missing
	// status field — legacy messages are untrusted), block the work item
	// with a clear reason. Previously the coordinator would count
	// safety-limit-hit results toward convergence and synthesize a brief
	// from the partial/failed evidence, producing hallucinated PRDs. The
	// user explicitly chose "block on any failure" over "degrade on N-of-M"
	// — degraded success is the bug we're fixing.
	failedSummaries, transientFailures, okCount := judgeResearchResults(gateResults)
	if len(failedSummaries) > 0 {
		allTransient := transientFailures == len(failedSummaries)
		majorityOK := okCount > len(gateResults)/2

		if allTransient && majorityOK {
			log.Printf("research-coordinator: work_id=%s — %d/%d tasks failed (all transient), proceeding with %d successful results",
				workID, len(failedSummaries), len(gateResults), okCount)
		} else {
			reason := fmt.Sprintf(
				"Research did not complete honestly: %d of %d tasks failed. %s. "+
					"Not synthesizing a technical brief from partial/failed research — re-run after addressing the failures.",
				len(failedSummaries), len(gateResults), strings.Join(failedSummaries, "; "),
			)
			log.Printf("research-coordinator: blocking work_id=%s — %s", workID, reason)
			if err := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); err != nil {
				log.Printf("research-coordinator: block work item: %v", err)
			}
			if assignmentID != uuid.Nil {
				wg.CompleteAssignment(msgCtx, assignmentID, models.AssignmentStatusFailed) //nolint:errcheck
			}
			return nil
		}
	}

	briefContent := synthesizeBrief(msgCtx, llmSvc, wg, kb, workID, prdContent, results, assignmentID)
	if briefContent == "" {
		// synthesizeBrief already blocked the work item and marked the
		// assignment failed — nothing more to do. Do NOT publish
		// technical-brief.ready with fabricated content.
		return nil
	}

	// Union-dedupe structured fields across all task results. This is
	// the payoff for Approach A in the pivot-B plan: downstream planners
	// and implementation agents inherit a single merged view of every
	// file/module/zone/pattern touched during research, without having
	// to re-extract them from the prose narrative.
	agg := aggregateStructuredFields(results)
	criteria := mergeBriefCriteria(results)
	// The intake's text: the session's description + PRD, or the PRD alone
	// after a restart (seedContext).
	seedCtx := coord.seedContext(round)

	// Save technical brief artifact. Keep "brief" populated for the
	// planner's legacy lookup under that key (Agent C owns removing that
	// indirection); "narrative" is the new canonical key.
	brief := technicalBrief{
		Brief:                briefContent,
		Narrative:            briefContent,
		WorkID:               workID.String(),
		AffectedFiles:        agg.AffectedFiles,
		AffectedModules:      agg.AffectedModules,
		AffectedZones:        agg.AffectedZones,
		CrossRepoTouchpoints: agg.CrossRepoTouchpoints,
		RelevantPatterns:     agg.RelevantPatterns,
		RelevantAntiPatterns: agg.RelevantAntiPatterns,
		Criteria:             criteria,
		BaseSHAs:             briefBaseSHAs(msgCtx, pool, workID),
		LineSeeds:            extractLineSeedsWith(results, coord.roundClones(round), seedCtx),
		PRRequirements:       plan.PRTextRequirements(seedCtx.hintText),
	}
	// Migration 060: the codex the brief rests on, pinned with the brief.
	var codexPin *codex.Pin
	if pid, pErr := uuid.Parse(productID); pErr == nil && pid != uuid.Nil {
		cx := codex.New(pool)
		if pin, ok := briefCodexPin(workID, pid, results, agg.AffectedModules,
			codexBranchFor(msgCtx, pool, wg, workID, pid),
			func(branch string) (int, error) { return cx.GetCurrentVersion(msgCtx, pid, branch) },
		); ok {
			codexPin = &pin
			brief.CodexVersion, brief.CodexBranch = pin.ProductVersion, pin.Branch
		}
	}
	checkResultBases(workID, brief.BaseSHAs, results)
	artifactContent, mErr := json.Marshal(brief)
	if mErr != nil {
		log.Printf("research-coordinator: marshal technical brief: %v — falling back to legacy shape", mErr)
		artifactContent, _ = json.Marshal(map[string]string{
			"brief":   briefContent,
			"work_id": workID.String(),
		})
	}
	artifact := &models.WorkItemArtifact{
		WorkItemID: workID,
		Type:       models.ArtifactTypeTechnicalBrief,
		Content:    artifactContent,
		AgentID:    agentID,
	}

	// technical-brief.ready — include product_id.
	briefMsg := technicalBriefReadyMessage{
		WorkID:       workID.String(),
		ProductID:    productID,
		BriefContent: briefContent,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Criteria:     criteria,
	}

	// The artifact, its event and the assignment completion commit together.
	// Before the outbox a failed artifact save still published the event (the
	// planner then decomposed without the structured fields) and a failed
	// publish left a finished brief nobody would plan; now a failure leaves
	// neither, and the item is blocked with the cause.
	aggregateID := roundAggregate(msgCtx, wg, round)
	// M1-05: the brief's transaction carries the guard row of the
	// research.results event that completed the set.
	if err := commitTechnicalBrief(msgCtx, idempotency.Wrap(msgCtx, pool), wg, aggregateID, artifact, briefMsg, assignmentID, round.ID, codexPin); idempotency.IsDuplicate(err) {
		log.Printf("research-coordinator: technical brief for work_id=%s already recorded — duplicate delivery", workID)
		outcome = roundCompleted
	} else if err != nil {
		reason := fmt.Sprintf("Technical brief was synthesized but could not be recorded: %v. Retry the work item.", err)
		log.Printf("research-coordinator: commit technical brief for work_id=%s: %v", workID, err)
		if bErr := blockInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason); bErr != nil {
			log.Printf("research-coordinator: block work item: %v", bErr)
		}
		if assignmentID != uuid.Nil {
			wg.CompleteAssignment(msgCtx, assignmentID, models.AssignmentStatusFailed) //nolint:errcheck
		}
	} else {
		outcome = roundCompleted
	}

	return nil
}

// briefLLM is the part of *llm.Service synthesis uses.
type briefLLM interface {
	Execute(ctx context.Context, intent strategy.Intent) (*strategy.Result, error)
}

// resolveBriefLLM resolves the work item's LLM for synthesis. A var only so
// tests can drive finishRound without a provider (no live LLM in tests).
var resolveBriefLLM = func(ctx context.Context, registry *agentregistry.Registry, workID uuid.UUID, cs *configstore.ConfigStore, pool *pgxpool.Pool) (briefLLM, error) {
	svc, err := llm.NewServiceForWorkItem(ctx, registry, workID, cs, "research-coordinator", pool)
	if err != nil || svc == nil {
		return nil, err
	}
	return svc, nil
}

// synthesizeBrief asks the LLM to produce a technical brief from the PRD and
// research findings. This is a REQUIRED LLM call — there is no acceptable
// placeholder. If the LLM is missing, errors, or returns empty content, the
// function blocks the work item (fail honestly) and returns an empty string;
// the caller must then skip publishing technical-brief.ready. Fabricating a
// "# Technical Brief" header with raw findings appended previously caused the
// implementation-planner to decompose junk into implementation subtasks,
// burning LLM budget on work against a non-existent plan.
func synthesizeBrief(
	ctx context.Context,
	llmSvc briefLLM,
	wg *workgraph.WorkGraph,
	kb bus.MessageBus,
	workID uuid.UUID,
	prdContent string,
	results []researchResultMessage,
	assignmentID uuid.UUID,
) string {
	if llmSvc == nil {
		reason := "No LLM configured for research-coordinator — cannot synthesize technical brief. Configure an LLM provider at Admin → Providers and retry."
		log.Printf("research-coordinator: %s (work_id=%s)", reason, workID)
		if err := guards.BlockWorkItem(ctx, wg, kb, workID, agentID, reason); err != nil {
			log.Printf("research-coordinator: block work item: %v", err)
		}
		if assignmentID != uuid.Nil {
			wg.CompleteAssignment(ctx, assignmentID, models.AssignmentStatusFailed) //nolint:errcheck
		}
		return ""
	}

	evidenceBuilder := make([]string, 0, len(results))
	for _, r := range results {
		evidenceBuilder = append(evidenceBuilder, fmt.Sprintf("- [%s] %s (confidence: %s)", r.FindingType, r.Evidence, string(r.Confidence)))
	}
	var evidenceSummary string
	for _, e := range evidenceBuilder {
		evidenceSummary += e + "\n"
	}

	synthIntent := strategy.Intent{
		Type: strategy.IntentSynthesizeBrief,
		Context: map[string]string{
			"prd":          prdContent,
			"findings":     evidenceSummary,
			"work_id":      workID.String(),
			"work_item_id": workID.String(),
		},
		OutputFormat: "Markdown with sections: Executive Summary, Key Findings, Technical Recommendations, Risk Assessment, Implementation Notes",
	}
	result, err := llmSvc.Execute(ctx, synthIntent)
	if err != nil {
		reason := fmt.Sprintf("Research synthesis failed: %v. Check LLM provider status and retry.", err)
		log.Printf("research-coordinator: synthesize brief for work_id=%s: %v", workID, err)
		if bErr := guards.BlockWorkItem(ctx, wg, kb, workID, agentID, reason); bErr != nil {
			log.Printf("research-coordinator: block work item: %v", bErr)
		}
		if assignmentID != uuid.Nil {
			wg.CompleteAssignment(ctx, assignmentID, models.AssignmentStatusFailed) //nolint:errcheck
		}
		return ""
	}

	if result == nil || strings.TrimSpace(result.Content) == "" {
		reason := "Research synthesis returned empty content from the LLM. Check rate limits, token budgets, and provider logs, then retry."
		log.Printf("research-coordinator: empty brief for work_id=%s", workID)
		if bErr := guards.BlockWorkItem(ctx, wg, kb, workID, agentID, reason); bErr != nil {
			log.Printf("research-coordinator: block work item: %v", bErr)
		}
		if assignmentID != uuid.Nil {
			wg.CompleteAssignment(ctx, assignmentID, models.AssignmentStatusFailed) //nolint:errcheck
		}
		return ""
	}

	return result.Content
}
