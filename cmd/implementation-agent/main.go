package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/verify"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/testdetect"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "implementation-agent"
	consumerGroup = "implementation-agent"
	healthPort    = 8095

	// vectorCollection must match knowledge-indexer so codex_similar_code
	// queries the same dataset the indexer populated.
	vectorCollection = "codebase"
)

// implementationTaskMessage is received from implementation.tasks.
//
// The AffectedModules / RelevantPatterns / RelevantAntiPatterns /
// CrossRepoTouchpoints fields are the structured scope hints forwarded by
// implementation-planner from the research brief. When the upstream brief
// was legacy (prose-only, pre-Agent-B) or the planner couldn't load the
// artifact, these slices are empty and the agent falls back to its own
// codex_* queries to discover scope.
type implementationTaskMessage struct {
	TaskID               string   `json:"task_id"`
	WorkID               string   `json:"work_id"`
	ProductID            string   `json:"product_id"`
	RepositoryID         string   `json:"repository_id,omitempty"`
	Title                string   `json:"title"`
	Description          string   `json:"description"`
	FilesToChange        []string `json:"files_to_change"`
	Dependencies         []string `json:"dependencies"`
	CodexVersion         int      `json:"codex_version"`
	TechnicalBrief       string   `json:"technical_brief"`
	CodexPatterns        string   `json:"codex_patterns"`
	AffectedModules      []string `json:"affected_modules,omitempty"`
	RelevantPatterns     []string `json:"relevant_patterns,omitempty"`
	RelevantAntiPatterns []string `json:"relevant_anti_patterns,omitempty"`
	CrossRepoTouchpoints []string `json:"cross_repo_touchpoints,omitempty"`
	ReviewFeedback       string   `json:"review_feedback,omitempty"`
	IsReviewFix          bool     `json:"is_review_fix,omitempty"`
	// TaskSpec / Criteria are the planner's task contract (M2-04). Logged
	// and recorded in the summary artifact only (criteria.go); M3-07
	// verifies against them.
	TaskSpec *plan.TaskSpec   `json:"task_spec,omitempty"`
	Criteria []plan.Criterion `json:"criteria,omitempty"`
	// BaseSHA / BaseRef are the repository's pinned base commit and its
	// branch (M2-02), set by the planner from the work item's pin; empty for
	// a legacy-unpinned item and for subtasks built outside the planner
	// (finalization then looks the subtask's pin up in the store). The
	// evidence base stays the merge-base that files_changed was diffed from
	// (D1); when that differs from BaseSHA, finalization records a
	// drift_detected event (finalize.go, applyPinnedBase).
	BaseSHA string `json:"base_sha,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`
	// LineSeeds are research's places to edit on this subtask's files
	// (R10 W5): the task message's "## Where to edit", and where read_file
	// opens those files by default.
	LineSeeds []lineSeed `json:"line_seeds,omitempty"`
}

// implementationCompleteMessage is published to implementation.complete.
type implementationCompleteMessage struct {
	TaskID       string   `json:"task_id"`
	WorkID       string   `json:"work_id"`
	FilesChanged []string `json:"files_changed"`
	TestResults  string   `json:"test_results"`
	Summary      string   `json:"summary,omitempty"`
	Success      bool     `json:"success"`
	Reason       string   `json:"reason,omitempty"`

	// Evidence fields (additive). HeadSHA is the commit the final gate
	// verified and, on success, the commit pushed. BaseSHA is the commit
	// the worktree branched from; FilesChanged is base...HEAD from git.
	// Verified is true only when the configured tests actually ran and
	// passed on HeadSHA — a skipped test run never sets it.
	HeadSHA  string `json:"head_sha,omitempty"`
	BaseSHA  string `json:"base_sha,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Verified bool   `json:"verified"`

	// Evidence is the v1 implementation evidence record (M0-07), set on
	// every finalization path and absent on generation-phase failures.
	// Consumers gate on it through evidence.ReleaseDecision; the legacy
	// fields above keep their meaning for consumers not yet migrated.
	Evidence *evidence.ImplementationEvidence `json:"evidence,omitempty"`
}

// implementationFailureMessage is published when a task cannot be completed.
type implementationFailureMessage struct {
	TaskID string `json:"task_id"`
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("implementation-agent: load config: %v", err)
	}

	// Reclaim worktrees abandoned by a previous process before taking work.
	if reconcileEnabled() {
		reconcileCtx, cancelReconcile := context.WithTimeout(ctx, worktreeReconcileTimeoutFromEnv())
		reconcileAbandonedWorktrees(reconcileCtx, logging.New(agentID), repoCacheRootFromEnv())
		cancelReconcile()
	} else {
		log.Printf("implementation-agent: startup worktree reconcile disabled via %s", envWorktreeReconcile)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("implementation-agent: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("implementation-agent: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// M2-03: reclaim unreferenced snapshot worktrees (background, bounded).
	go pruneSnapshotsOnStartup(ctx, pool)

	wg := workgraph.New(pool)
	ps := products.NewStore(pool)
	registry := agentregistry.New(pool)
	cx := codex.New(pool)

	var cs *configstore.ConfigStore
	masterKeyStr := os.Getenv("MASTER_ENCRYPTION_KEY")
	if masterKeyStr != "" {
		mk, mkErr := crypto.LoadMasterKey(masterKeyStr)
		if mkErr == nil {
			cs = configstore.New(pool, mk)
			// Listen on pg_notify('config_changed') so UI updates to credentials/
			// endpoints flush the in-memory cache without a restart.
			cs.StartWatcher(ctx)
		}
	}

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04). implementation.complete, work.blocked and
	// escalation prompts are committed to outbox_events with the state
	// writes they announce; this loop carries them to Kafka. Without it
	// they would sit in a table nobody drains, so a start failure is fatal.
	// One per process; its ae_outbox_* series are on /metrics below.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "implementation-agent")
	if err != nil {
		log.Fatalf("implementation-agent: start outbox dispatcher: %v", err)
	}
	log.Printf("implementation-agent: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M1-05: deduplicate on the processed-event ledger. The up-front Seen
	// check acknowledges a recorded duplicate before any worktree or LLM call.
	handler := idempotency.NewLedger(pool).Handler("implementation-agent", buildHandler(ctx, pool, wg, ps, registry, cs, kb, cx))
	if err := kb.Subscribe(bus.TopicImplementationTasks, consumerGroup, handler); err != nil {
		log.Fatalf("implementation-agent: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("implementation-agent: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("implementation-agent: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("implementation-agent: shutting down...")
	cancel()
	// Let an in-flight outbox publish finish being recorded before exit.
	dispatcher.Wait()
	log.Println("implementation-agent: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("implementation-agent: health server shutdown error: %v", err)
	}
	log.Println("implementation-agent: stopped")
}

func buildHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	ps *products.ProductStore,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	cx *codex.Codex,
) bus.Handler {
	logger := logging.New("implementation-agent")
	evidenceStore := evidence.NewStore(pool)
	policyStore := verifypolicy.NewStore(pool)
	zoneStore := newZoneSource(pool)
	var runStore *controller.Store
	if pool != nil {
		runStore = controller.NewStore(pool)
	}
	return func(msgCtx context.Context, msg *bus.Message) error {
		var task implementationTaskMessage
		if err := json.Unmarshal(msg.Value, &task); err != nil {
			log.Printf("implementation-agent: unmarshal task: %v", err)
			return nil
		}

		taskID, err := uuid.Parse(task.TaskID)
		if err != nil {
			log.Printf("implementation-agent: parse task_id %q: %v", task.TaskID, err)
			return nil
		}
		workID, err := uuid.Parse(task.WorkID)
		if err != nil {
			log.Printf("implementation-agent: parse work_id %q: %v", task.WorkID, err)
			return nil
		}

		// Attach work item ID to context for structured log correlation.
		msgCtx = logging.WithWorkItemID(msgCtx, workID.String())
		logTaskCriteria(task)

		// Ghost-task guard: skip implementation.tasks messages whose
		// subtask row no longer exists in work_items. Kafka can redeliver
		// messages after the agent restarts or the planner deletes a
		// subtask (e.g. when an operator clears tainted state and replays
		// technical-brief.ready). Running the tool loop on a deleted
		// subtask wastes inference budget and blocks the queue behind it.
		if sub, lookupErr := wg.GetWorkItem(msgCtx, taskID); lookupErr != nil || sub == nil {
			log.Printf("implementation-agent: skipping ghost task_id=%s (not in work_items) work_id=%s", taskID, workID)
			return nil
		}

		// M1-05: every outcome transaction (the sink below) carries the
		// processed-event guard row; Ledger.Handler has already
		// acknowledged a recorded duplicate before this handler ran.
		db := idempotency.Wrap(msgCtx, pool)

		// Roadmap 1.2: validate the subtask's state before creating a
		// worktree or calling the LLM. A subtask that is no longer in
		// implementation (cancelled, passed, blocked by a retry decision)
		// or whose parent was cancelled or blocked must not be worked on;
		// a mismatch blocks it with the observed and expected states (when
		// it can still be blocked), committed with the guard row.
		pre, perr := idempotency.CheckPrecondition(msgCtx, db, idempotency.Precondition{
			Action:      "create a worktree and run the implementation",
			AgentID:     agentID,
			AggregateID: workID,
			WorkID:      taskID,
			Expected:    []models.WorkItemState{models.WorkItemStateImplementation},
			ParentID:    workID,
			ParentNotIn: []models.WorkItemState{models.WorkItemStateCancelled, models.WorkItemStateBlocked},
		})
		if idempotency.IsDuplicate(perr) {
			return nil
		}
		if perr != nil {
			return fmt.Errorf("precondition check for task_id=%s: %w", taskID, perr)
		}
		if !pre.OK {
			log.Printf("implementation-agent: NOT implementing task_id=%s work_id=%s (blocked=%t): %s", taskID, workID, pre.Blocked, pre.Reason)
			return nil
		}

		logger.Info(msgCtx, "processing task", map[string]interface{}{
			"task_id": taskID.String(),
			"work_id": workID.String(),
			"repo_id": task.RepositoryID,
		})

		// Fallback: if FilesToChange is empty, try to extract file paths from
		// the description. The planner writes "Files: [path1 path2]" markers
		// into the description for each step, and the prose often references
		// paths directly. Without FilesToChange the continuation loop cannot
		// detect partial completions and silently skips.
		if len(task.FilesToChange) == 0 && task.Description != "" {
			task.FilesToChange = extractFilePathsFromDescription(task.Description)
			if len(task.FilesToChange) > 0 {
				logger.Info(msgCtx, "recovered FilesToChange from description text", map[string]interface{}{
					"task_id": taskID.String(),
					"files":   task.FilesToChange,
				})
			}
		}
		// The planned scope includes every repository file the steps cite
		// (a planner older than R5 sent a narrower files_to_change).
		if scope, _ := taskScope(task); len(scope) > len(task.FilesToChange) {
			logger.Info(msgCtx, "widened FilesToChange to the steps' files", map[string]interface{}{
				"task_id": taskID.String(),
				"from":    task.FilesToChange,
				"to":      scope,
			})
			task.FilesToChange = scope
		}

		// Record agent assignment for this work item.
		assignment := &models.AgentAssignment{
			WorkItemID:      workID,
			AgentType:       models.AgentTypeImplementation,
			AgentInstanceID: agentID,
			Status:          models.AssignmentStatusActive,
		}
		if err := wg.AssignAgent(msgCtx, assignment); err != nil {
			log.Printf("implementation-agent: assign agent: %v", err)
		}

		// Every exit from here on reports through sink: its state writes
		// (evidence, prompt, block, assignment) and implementation.complete
		// commit in one transaction, the event via the outbox (M1-04).
		// The process ctx is used so a cancelled message ctx cannot drop
		// the completion event.
		sink := &outboxCompletionSink{
			ctx:          ctx,
			db:           db, // guarded (M1-05)
			wg:           wg,
			prompts:      interactions.NewPublisher(nil),
			workID:       workID,
			assignmentID: assignment.ID,
			taskID:       task.TaskID,
			logger:       logger,
		}

		// M3-02: this delivery's controller run (attempt N+1). Its state
		// advances only from tool evidence; the sink commits its terminal
		// state with the outcome on every exit below.
		run := startControllerRun(ctx, runStore, logger, workID, taskID)
		sink.run = outcomeHookFor(run, logger)
		// D4 budgets for this attempt (budget.go, M3-06): the active-time
		// clock starts now and pauses while a user prompt is pending.
		budget := newAttemptBudget(time.Now, run, logger, taskID.String())
		budget.persist(ctx, true)
		defer budget.persist(context.Background(), true)

		// Resolve the clone path for this task's repository.
		clonePath := resolveClonePath(msgCtx, ps, task.RepositoryID)

		// Validate context before writing any code.
		repoCheck := guards.CheckRepoAvailable(clonePath)
		if !repoCheck.Valid {
			log.Printf("implementation-agent: context validation failed for task_id=%s: %s", taskID, repoCheck.Message)
			reportOutcome(sink, completionOutcome{
				blockReason: "Cannot implement: no repository available. " + repoCheck.Message,
				assignment:  models.AssignmentStatusFailed,
			})
			return nil
		}

		// Resolve LLM adapter per work item: checks agent_definition first, falls back to global.
		resolution, err := registry.Resolve(msgCtx, workID, cs, "implementation-agent", "instance-1")
		if err != nil {
			log.Printf("implementation-agent: resolve adapter for work_id=%s: %v", workID, err)
			reportOutcome(sink, completionOutcome{
				blockReason: "Cannot create LLM adapter: provider not configured or credentials missing. Configure model credentials in Admin → Credentials.",
				assignment:  models.AssignmentStatusFailed,
			})
			return nil
		}
		llmAdapter, agentDef := resolution.Adapter, resolution.Definition
		// M4-05: this attempt's configuration snapshot goes on the run, the
		// work item, the evidence record and every inference row.
		msgCtx = recordAttemptSnapshot(msgCtx, registry, runStore, run, workID, resolution)

		// Resolve productID once; reused by both the prompt-context builder
		// below AND by the codex-aware executor further down.
		var productID *uuid.UUID
		if task.ProductID != "" {
			if pid, pidErr := uuid.Parse(task.ProductID); pidErr == nil {
				productID = &pid
			}
		}

		// Resolve the work item's target branch so codex lookups match the
		// branch this task is implementing against. Empty = main.
		branch := ""
		if wi, wiErr := wg.GetWorkItem(msgCtx, workID); wiErr == nil && wi != nil && wi.Branch != nil {
			branch = *wi.Branch
		}

		// Build Codex context for the prompt.
		codexContext := ""
		if cx != nil {
			// The body only: assembleImplementationSystemPrompt adds the
			// heading when there is something under it, and bounds it.
			var sb strings.Builder

			if productID != nil {
				modules, _ := cx.GetModulesForProductBranch(msgCtx, *productID, branch)
				if len(modules) > 0 {
					sb.WriteString("### Modules:\n")
					for _, m := range modules {
						sb.WriteString(fmt.Sprintf("- %s: %s (risk: %s)\n", m.Name, m.Purpose, m.RiskLevel))
					}
					sb.WriteString("\n")
				}
			}

			// Patterns and anti-patterns are product-scoped so one product's
			// codex can't leak into another's prompt. If productID is missing
			// we deliberately skip grounding rather than fall back to the
			// global GetAntiPatterns / GetPatternsByCategory("") queries
			// that returned cross-product rows and zero rows respectively.
			if productID != nil {
				patterns, _ := cx.GetPatternsForProductBranch(msgCtx, *productID, branch)
				if len(patterns) > 0 {
					sb.WriteString("### Patterns to Follow:\n")
					for _, p := range patterns {
						sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", p.Name, p.Category, p.Rationale))
					}
					sb.WriteString("\n")
				}

				antiPatterns, _ := cx.GetAntiPatternsForProductBranch(msgCtx, *productID, branch)
				if len(antiPatterns) > 0 {
					sb.WriteString("### Anti-Patterns to AVOID:\n")
					for _, ap := range antiPatterns {
						sb.WriteString(fmt.Sprintf("- %s: %s → Instead: %s\n", ap.Name, ap.WhyWrong, ap.WhatInstead))
					}
				}
			}

			codexContext = sb.String()
		}

		// Detect the test command from the project structure.
		testCommand := policyTestCommand(msgCtx, logger, policyStore, task.RepositoryID, testdetect.DetectTestCommand(clonePath))
		run.SetTestCommand(testCommand)

		// Resolve the repository's actual base branch from the DB. The branch
		// name is repo-specific ("main" vs "master" vs "develop") and hardcoding
		// "main" caused silent worktree failures on repos that use master,
		// followed by a fallback to the shared clone that masked the error.
		// If RepositoryID is unset or the lookup fails, default to "main" but
		// log it — the operator needs to see when we're guessing.
		baseBranch := "main"
		if task.RepositoryID != "" {
			var repoBase string
			if repoErr := pool.QueryRow(msgCtx,
				`SELECT base_branch FROM repositories WHERE id = $1`, task.RepositoryID,
			).Scan(&repoBase); repoErr == nil && repoBase != "" {
				baseBranch = repoBase
			} else if repoErr != nil {
				logger.Warn(msgCtx, "base_branch lookup failed — defaulting to main", map[string]interface{}{
					"repo_id": task.RepositoryID,
					"err":     repoErr.Error(),
				})
			}
		}

		// ── Environment health check ─────────────────────────────────
		// Before creating a worktree, ensure the main clone has its
		// dependencies installed. A clone without node_modules (or
		// equivalent) will cascade into hundreds of false LSP errors
		// in every worktree derived from it.
		if envErr := ensureDepsInstalled(msgCtx, clonePath); envErr != "" {
			logger.Warn(msgCtx, "repo environment is broken", map[string]interface{}{
				"task_id": taskID.String(),
				"reason":  envErr,
			})
			prompt := &interactions.Prompt{
				ID:          uuid.New(),
				WorkItemID:  workID,
				AskingAgent: agentID,
				Trigger:     interactions.TriggerPreflightBrokenBuild,
				PromptType:  interactions.PromptTypeEscalation,
				Headline:    "Repository environment is broken — dependencies missing",
				Question: fmt.Sprintf(
					"I cannot implement this work item because the repository's dependencies are not installed.\n\n"+
						"**Problem:** %s\n\n"+
						"This is an environment issue, not a code error. The repository needs its dependencies installed "+
						"before any implementation work can proceed.",
					envErr,
				),
				ContextPayload: map[string]any{
					"clone_path": clonePath,
					"reason":     envErr,
					"task_id":    taskID.String(),
				},
				Actions: []interactions.Action{
					{ID: "retry", Label: "Retry (I've installed deps)", Semantic: interactions.SemanticRetry},
					{ID: "cancel", Label: "Cancel this work item", Semantic: interactions.SemanticCancel},
				},
				State:       interactions.StatePending,
				RequestedAt: time.Now().UTC(),
				PromptedVia: []string{"dashboard"},
			}
			reportOutcome(sink, completionOutcome{
				prompt:      escalationPrompt(prompt, productID),
				blockReason: fmt.Sprintf("repo_env_broken: %s", envErr),
				msg: &implementationCompleteMessage{
					TaskID:  task.TaskID,
					WorkID:  task.WorkID,
					Success: false,
					Reason:  fmt.Sprintf("repository environment broken: %s", envErr),
				},
				assignment: models.AssignmentStatusFailed,
			})
			return nil
		}

		// Create an isolated worktree for this subtask to avoid conflicts with parallel tasks.
		worktreePath := worktreePathFor(clonePath, taskID)
		subtaskBranch := fmt.Sprintf("agent/%s/%s", task.WorkID, taskID.String()[:8])

		// M2-03: a pinned subtask starts at its pinned base commit (fetch
		// origin <base_ref>, worktree add at the SHA, HEAD asserted), never
		// at the local base branch. Legacy-unpinned subtasks keep baseBranch.
		pinSHA, pinRef := taskPin(msgCtx, task, taskID, newPinLookup(pool), baseBranch)
		diffBaseRef := baseBranch
		startBranch := baseBranch
		if pinSHA != "" {
			diffBaseRef = pinSHA
			startBranch = pinSHA
		}
		fromParentBranch := false

		// For review-fix tasks, branch from the parent's implementation branch
		// instead of baseBranch. This preserves the original implementation so
		// the agent only needs to apply targeted fixes rather than re-implementing
		// everything from scratch.
		if task.IsReviewFix {
			if existing := findExistingAgentBranch(msgCtx, clonePath, task.WorkID); existing != "" {
				// Fetch the remote branch so git can resolve it locally.
				fetchCmd := exec.CommandContext(msgCtx, "git", "fetch", "origin", existing)
				fetchCmd.Dir = clonePath
				if fetchOut, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
					logger.Warn(msgCtx, "failed to fetch parent branch for review fix, falling back to baseBranch", map[string]interface{}{
						"parent_branch": existing,
						"err":           string(fetchOut),
					})
				} else {
					startBranch = "origin/" + existing
					fromParentBranch = true
					logger.Info(msgCtx, "review-fix task: branching from parent implementation branch", map[string]interface{}{
						"parent_branch": existing,
						"start_ref":     startBranch,
					})
				}
			}
		}

		// An agent branch left by the previous attempt (a stopped attempt's
		// unpushed work) is preserved at refs/ae/wip/<task8>/<attempt-1>
		// before the branch is recreated (M3-06).
		wipCtx := withWIPRef(msgCtx, wipRefName(shortID(taskID), runAttempt(run)-1))
		var wtErr error
		if pinSHA != "" && !fromParentBranch {
			wtErr = createPinnedWorktree(wipCtx, clonePath, worktreePath, subtaskBranch, pinRef, pinSHA)
			if wtErr == nil {
				logger.Info(msgCtx, "worktree created at the pinned base", map[string]interface{}{
					"task_id": taskID.String(), "path": worktreePath, "base_sha": pinSHA, "base_ref": pinRef,
				})
			}
		} else {
			wtErr = createIsolatedWorktree(wipCtx, clonePath, worktreePath, subtaskBranch, startBranch)
		}
		if err := wtErr; err != nil {
			reason := fmt.Sprintf("Cannot implement safely: failed to create isolated git worktree: %v", err)
			logger.Error(msgCtx, "isolated worktree creation failed", err, map[string]interface{}{
				"task_id": taskID.String(), "branch": subtaskBranch, "start_ref": startBranch,
			})
			reportOutcome(sink, completionOutcome{
				blockReason: reason,
				msg: &implementationCompleteMessage{
					TaskID: task.TaskID, WorkID: task.WorkID, Success: false, Reason: reason,
				},
				assignment: models.AssignmentStatusFailed,
			})
			return nil
		}
		if err := writeWorktreeMarker(worktreePath, taskID.String(), task.WorkID, clonePath); err != nil {
			// The worktree itself is usable; only crash recovery loses track of it.
			logger.Warn(msgCtx, "failed to write worktree marker; worktree will not be reclaimed after a crash", map[string]interface{}{
				"task_id": taskID.String(), "path": worktreePath, "err": err.Error(),
			})
		}
		defer func() {
			// Clean up worktree when done
			if err := removeWorktree(context.Background(), clonePath, worktreePath, repoCacheRootFromEnv()); err != nil {
				logger.Warn(context.Background(), "worktree cleanup did not remove worktree", map[string]interface{}{
					"task_id": taskID.String(), "path": worktreePath, "err": err.Error(),
				})
			}
		}()

		// Symlink untracked dependency directories from the main clone
		// into the worktree. git worktree only checks out tracked files;
		// node_modules, .next, etc. are gitignored and won't exist,
		// causing every import to fail under the LSP.
		if worktreePath != clonePath {
			symlinkDeps(clonePath, worktreePath)
		}

		// Record the integration base of this worktree (merge-base of the
		// pinned base — or, legacy-unpinned, the base branch — and HEAD).
		// Completion evidence (files changed, base_sha) is derived from
		// base...HEAD in git, never from the planner's expectation.
		baseSHA := resolveBaseSHA(msgCtx, logger, worktreePath, diffBaseRef, taskID.String())

		// T3: the task, de-duplicated (steps once, criteria once, files in
		// scope); the checkpoint keeps it verbatim on every call (B2).
		taskInstructions := buildTaskMessage(task)

		// Build a VectorStore for codex_similar_code. Query-time vectors
		// MUST come from the same embedding provider + model the knowledge-
		// indexer wrote with (any mismatch makes the dot product
		// meaningless). Resolve the default embedding_providers row via
		// the configstore; if the lookup fails (no provider configured, or
		// no master key), codex_similar_code is disabled and the LLM falls
		// back to search_code.
		var vectorStore *vector.VectorStore
		qdrantURL := os.Getenv("QDRANT_URL")
		if qdrantURL != "" && cs != nil {
			embSvc, _, embErr := vector.BuildEmbeddingService(msgCtx, pool, cs)
			if embErr != nil {
				log.Printf("implementation-agent: embedding provider unavailable — codex_similar_code disabled: %v", embErr)
				vectorStore = vector.New(qdrantURL, vectorCollection)
			} else {
				vectorStore = vector.NewWithEmbeddings(qdrantURL, vectorCollection, embSvc)
			}
		} else if qdrantURL != "" {
			vectorStore = vector.New(qdrantURL, vectorCollection)
		}

		// Create executor rooted at the isolated worktree (or shared clone on fallback).
		// Wire in the codex + vector store + product_id so codex_* tools serve real
		// data. kb + workID enable codex.gap event emission when the agent falls
		// back to grep after the codex returned empty.
		executor := tools.NewExecutorWithCodex(worktreePath, cx, vectorStore, productID, kb, &workID)
		executor.SetAskingAgent(agentID)
		// B5: start the workspace's language server now; the tool loops
		// offer the LSP tools only once it is ready (runImplementationLoop).
		go executor.WarmLSP(msgCtx)
		// T1: codex_read_file / codex_grep / codex_list_tree /
		// codex_verify_imports read this worktree, so they see the edits.
		executor.SetCodexReadsFromWorktree(true)
		// The worktree is the task's repository (not the product's first
		// one): repo=<that name> reads it too.
		executor.SetWorktreeRepo(taskRepoName(msgCtx, ps, task.RepositoryID))
		// D4: both budgets are checked before every LLM call; a pending
		// user prompt pauses the active-time clock.
		executor.SetIterationHook(budget.iterationHook)
		executor.SetPromptHook(budget.promptPending)
		// Record the planner's file scope so a subsequent write/edit to a
		// file outside that set publishes a codex.gap with reason
		// "planner_missed_file" — observability only, never blocking.
		executor.SetPlannedFiles(task.FilesToChange)
		executor.SetReadSeeds(seedLines(task.LineSeeds))

		// Scope codex queries to the work item's branch snapshot.
		if branch, headSHA := tools.ResolveWorkItemBranch(ctx, pool, workID); branch != "" {
			executor.SetWorkItemBranch(branch, headSHA)
		}

		// ── Pre-flight build health check ────────────────────────────
		// Errors that already exist on the base branch are inherited
		// tech debt — log them but never block on them. Only errors
		// that are NEW (not on the base branch) and in target files
		// can block implementation.
		//
		// For review-fix tasks the worktree starts from the previous
		// implementation branch, so errors introduced by that round
		// already exist in the worktree. Using the worktree as the
		// baseline prevents a deadlock where preflight blocks the
		// rework from fixing the very errors it was created to fix.
		baselinePath := clonePath
		if task.IsReviewFix {
			baselinePath = worktreePath
			log.Printf("preflight: review-fix task — using worktree as baseline (errors from previous round are bystanders)")
		}
		baselineErrors := preflightBuildCheck(msgCtx, executor, baselinePath, task.FilesToChange)
		baselineSet := make(map[string]bool, len(baselineErrors))
		for _, be := range baselineErrors {
			baselineSet[preflightFingerprint(be)] = true
		}
		if len(baselineErrors) > 0 {
			log.Printf("preflight: %d known error(s) on base branch — these will not block", len(baselineErrors))
			for _, be := range baselineErrors {
				log.Printf("  preflight: [base-branch] %s line %d: %s", be.File, be.Line, be.Message)
			}
		}

		if preflightErrors := preflightBuildCheck(msgCtx, executor, worktreePath, task.FilesToChange); len(preflightErrors) > 0 {
			changeSet := make(map[string]bool, len(task.FilesToChange))
			for _, f := range task.FilesToChange {
				changeSet[filepath.Clean(f)] = true
			}
			var blocking, bystander []preflightError
			for _, pe := range preflightErrors {
				if baselineSet[preflightFingerprint(pe)] {
					bystander = append(bystander, pe)
				} else if changeSet[filepath.Clean(pe.File)] {
					blocking = append(blocking, pe)
				} else {
					bystander = append(bystander, pe)
				}
			}

			if len(bystander) > 0 {
				log.Printf("preflight: %d inherited/bystander error(s) (proceeding):", len(bystander))
				for _, pe := range bystander {
					tag := "bystander"
					if baselineSet[preflightFingerprint(pe)] {
						tag = "base-branch"
					}
					log.Printf("  preflight: [%s] %s line %d: %s", tag, pe.File, pe.Line, pe.Message)
				}
			}

			if len(blocking) > 0 {
				var diagSummary strings.Builder
				diagSummary.WriteString("Pre-existing build errors in files this task must change:\n\n")
				for i, pe := range blocking {
					if i >= 15 {
						fmt.Fprintf(&diagSummary, "… and %d more errors\n", len(blocking)-15)
						break
					}
					fmt.Fprintf(&diagSummary, "  %s line %d: %s\n", pe.File, pe.Line, pe.Message)
				}

				logger.Warn(msgCtx, "preflight build check failed — errors in target files", map[string]interface{}{
					"task_id":         taskID.String(),
					"blocking_count":  len(blocking),
					"bystander_count": len(bystander),
				})

				prompt := &interactions.Prompt{
					ID:          uuid.New(),
					WorkItemID:  workID,
					AskingAgent: agentID,
					Trigger:     interactions.TriggerPreflightBrokenBuild,
					PromptType:  interactions.PromptTypeEscalation,
					Headline:    fmt.Sprintf("Build errors in target files (%d)", len(blocking)),
					Question: fmt.Sprintf(
						"Pre-existing build errors in files this task needs to modify:\n\n%s\n"+
							"Fix these errors in the repository, then retry.",
						diagSummary.String(),
					),
					ContextPayload: map[string]any{
						"error_count":       len(blocking),
						"files_with_errors": preflightErrorFiles(blocking),
						"task_id":           taskID.String(),
					},
					Actions: []interactions.Action{
						{ID: "retry", Label: "Retry (after fixing errors)", Semantic: interactions.SemanticRetry},
						{ID: "cancel", Label: "Cancel this work item", Semantic: interactions.SemanticCancel},
					},
					State:       interactions.StatePending,
					RequestedAt: time.Now().UTC(),
					PromptedVia: []string{"dashboard"},
				}
				reportOutcome(sink, completionOutcome{
					prompt:      escalationPrompt(prompt, productID),
					blockReason: fmt.Sprintf("preflight_broken_build: %d error(s) in target files", len(blocking)),
					msg: &implementationCompleteMessage{
						TaskID:  task.TaskID,
						WorkID:  task.WorkID,
						Success: false,
						Reason:  fmt.Sprintf("preflight: %d error(s) in files this task must change", len(blocking)),
					},
					assignment: models.AssignmentStatusFailed,
				})
				return nil
			}
		}

		// turnToolCalls accumulates every tool call the model makes across
		// the whole impl-agent invocation, in order. The verify-gate uses
		// this to check whether each file the model proposes in its final
		// answer was the target of a codex_verify_imports call. We reset
		// this slice only on agent exit — across gate-retry loops it keeps
		// growing so the gate can count verifications from earlier passes.
		var turnToolCalls []strategy.ToolCall

		// Publish each tool execution to Kafka so the dashboard can stream
		// real-time progress, record it into turnToolCalls for the
		// verify-gate, and feed it to the controller run (controller.go).
		executor.SetCallback(newToolCallback(ctx, kb, workID, taskID, run, budget, &turnToolCalls))
		// Impact-selected fast checks (fastchecks.go, M3-04).
		fast := startFastChecks(msgCtx, fastCheckDeps{worktree: worktreePath, baseSHA: baseSHA, repositoryID: task.RepositoryID, workID: workID, taskID: taskID, testCommand: testCommand, policies: policyStore, edges: pool, artifacts: wg, executor: executor, logger: logger})

		// Resolve the operating mode for the selected model so the prompt
		// scaffold matches the model's tier. gemma-class (Constrained) gets
		// plan-then-execute + exhaustive surface hints; Opus-class
		// (Advanced) gets minimal hand-holding.
		operatingMode := models.OperatingModeStandard
		if agentDef != nil && agentDef.Provider != "" && agentDef.ModelID != "" {
			if mode, modeErr := registry.GetOperatingMode(msgCtx, agentDef.Provider, agentDef.ModelID); modeErr == nil {
				operatingMode = mode
			}
		}
		scaffold := strategy.PromptScaffolding(operatingMode)

		// The repo has no detected test runner: either the work item asks
		// for tests (setting up the runner is in scope) or the TDD rule is
		// replaced by "implement directly, verify imports".
		noRunnerNote := ""
		if testCommand == "" {
			parentTitle, parentDescription := "", ""
			if wi, wiErr := wg.GetWorkItem(msgCtx, workID); wiErr == nil && wi != nil {
				parentTitle, parentDescription = wi.Title, wi.Description
			}
			noRunnerNote = noTestRunnerNote
			if testdetect.FeatureRequestsTests(parentTitle+" "+task.Title, parentDescription+" "+task.Description) {
				noRunnerNote = testSetupNote
			}
		}
		// T2: role and editing contract, tier preamble, bounded codex
		// grounding, test command, review feedback — the same assembly the
		// floor test measures. Files, steps and criteria are the task
		// message's (buildTaskMessage), not the system prompt's.
		systemPrompt := assembleImplementationSystemPrompt(operatingMode, codexContext, testCommand, noRunnerNote, task.ReviewFeedback)

		// Build the completion request using the scoped edit-phase tools (T1) — git ops are
		// handled deterministically by the agent after the tool loop completes.
		req := adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{
				{Role: adapter.RoleSystem, Content: systemPrompt},
				{Role: adapter.RoleUser, Content: taskInstructions},
			},
			Tools:       editTools(),
			ToolChoice:  "auto",
			MaxTokens:   8192,
			Temperature: 0.2,
		}

		logger.Info(msgCtx, "running tool loop", map[string]interface{}{
			"task_id": taskID.String(),
			"repo":    clonePath,
		})

		// TDD lifecycle lines are logged by the controller run as the tool
		// evidence arrives ("controller: red_observed (go test ./... exit 1,
		// call …)"); nothing is logged for a step the tools have not shown.

		// No wall-clock timeout on the tool loop. Progress is the only
		// valid kill signal: if the model is actively calling tools and
		// generating tokens, let it run. Stuck detection (same tool call
		// with identical args repeated 4×) is the real "locked" signal
		// and fires independently inside RunToolLoop. Wall-clock limits
		// measure slowness, not stalling — a 14-minute single inference
		// on a local 32B model is productive, not broken, and arbitrary
		// timeouts were killing real work mid-generation.
		maxIters := scaffold.MaxIterations
		if maxIters <= 0 {
			maxIters = 200
		}

		gen, ok := runGeneration(msgCtx, generationInputs{
			task:          task,
			taskID:        taskID,
			workID:        workID,
			clonePath:     clonePath,
			worktreePath:  worktreePath,
			subtaskBranch: subtaskBranch,
			baseBranch:    baseBranch,
			testCommand:   testCommand,
			fast:          fast,
			baseSHA:       baseSHA,
			req:           req,
			systemPrompt:  systemPrompt,
			maxIters:      maxIters,
			llmAdapter:    llmAdapter,
			executor:      executor,
			pool:          pool,
			turnToolCalls: &turnToolCalls,
			logger:        logger,
			budget:        budget,
			run:           run,
		}, sink)
		fin := finalizeInputs{
			task:                 task,
			taskID:               taskID,
			clonePath:            clonePath,
			worktreePath:         worktreePath,
			subtaskBranch:        subtaskBranch,
			baseBranch:           baseBranch,
			baseSHA:              baseSHA,
			testCommand:          testCommand,
			fast:                 fast,
			summary:              gen.summary,
			continuationFailures: gen.continuationFailures,
			evidenceStore:        evidenceStore.WithConfigSnapshot(resolution.SnapshotID),
			policies:             policyStore,
			zones:                zoneStore,
			logger:               logger,
			pins:                 newPinLookup(pool),
		}
		if gen.stopped != nil {
			// M3-06: a budget ran out or the repair loop gave up. The work
			// is committed unpushed and the failure is reported with its
			// evidence and an escalation (never partial work as success).
			saveImplementationSummary(msgCtx, wg, run, task, taskID, workID, clonePath, gen)
			finalizeStopped(msgCtx, fin, *gen.stopped, stopContext{productID: productID, workID: workID, budget: budget, attempt: runAttempt(run)}, sink)
			return nil
		}
		if !ok {
			return nil
		}

		// Save the LLM's summary as an implementation artifact.
		saveImplementationSummary(msgCtx, wg, run, task, taskID, workID, clonePath, gen)

		if !finalizeImplementation(msgCtx, fin, sink) {
			return nil
		}

		logger.Info(msgCtx, "task completed", map[string]interface{}{
			"task_id": taskID.String(),
			"work_id": workID.String(),
			"repo_id": task.RepositoryID,
		})
		return nil
	}
}

// createIsolatedWorktree creates a task branch in a dedicated worktree. A
// stale branch is removed before one retry, but the function never falls back
// to checking out the task branch in the shared clone: doing so lets parallel
// tasks overwrite each other's branch and working tree.
func createIsolatedWorktree(ctx context.Context, clonePath, worktreePath, branch, startRef string) error {
	add := func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branch, worktreePath, startRef)
		cmd.Dir = clonePath
		return cmd.CombinedOutput()
	}

	firstOut, firstErr := add()
	if firstErr == nil {
		return nil
	}

	// M3-06: a stopped attempt's work lives only on this local branch;
	// keep it under refs/ae/wip/ before the branch is deleted.
	if ref, err := preserveLocalBranch(ctx, clonePath, branch, wipRefFrom(ctx, branch)); err != nil {
		log.Printf("implementation-agent: preserve local branch %s before recreating it: %v", branch, err)
	} else if ref != "" {
		log.Printf("implementation-agent: local-only branch %s preserved at %s before it is recreated", branch, ref)
	}
	remove := exec.CommandContext(ctx, "git", "branch", "-D", branch)
	remove.Dir = clonePath
	remove.CombinedOutput() //nolint:errcheck -- branch may not exist

	secondOut, secondErr := add()
	if secondErr != nil {
		return fmt.Errorf("first attempt: %s; retry: %s: %w",
			strings.TrimSpace(string(firstOut)), strings.TrimSpace(string(secondOut)), secondErr)
	}
	return nil
}

// runTests executes the detected test command in clonePath and returns combined output.
func runTests(ctx context.Context, clonePath, testCmd string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", testCmd)
	cmd.Dir = clonePath
	return cmd.CombinedOutput()
}

// resolveClonePath looks up the clone path for a repository. If repository_id is
// empty or not found it falls back to the legacy single-repo path.
// taskRepoName is the task's repository name, or "" when it cannot be
// looked up (the executor then labels the worktree by its directory).
func taskRepoName(ctx context.Context, ps *products.ProductStore, repositoryID string) string {
	repoID, err := uuid.Parse(repositoryID)
	if err != nil || ps == nil {
		return ""
	}
	repo, err := ps.GetRepository(ctx, repoID)
	if err != nil || repo == nil {
		return ""
	}
	return repo.Name
}

func resolveClonePath(ctx context.Context, ps *products.ProductStore, repositoryID string) string {
	if repositoryID != "" {
		repoID, err := uuid.Parse(repositoryID)
		if err == nil {
			repo, err := ps.GetRepository(ctx, repoID)
			if err == nil && repo.ClonePath != "" {
				return repo.ClonePath
			}
			log.Printf("implementation-agent: lookup repo %s: %v — falling back to default", repositoryID, err)
		}
	}
	return "/workspace/repo"
}

func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return data
}

// escalationPrompt finishes an escalation prompt for the outcome
// transaction, or returns nil when there is no product to file it under:
// user_interactions.product_id is a NOT NULL foreign key, so a prompt
// without one would roll back the block and implementation.complete it
// travels with. The block still raises status-orchestrator's generic
// "Blocked" escalation through work.blocked.
func escalationPrompt(p *interactions.Prompt, productID *uuid.UUID) *interactions.Prompt {
	if productID == nil || *productID == uuid.Nil {
		log.Printf("implementation-agent: work item %s has no product — escalation prompt %q not raised; the block carries the reason", p.WorkItemID, p.Headline)
		return nil
	}
	p.ProductID = *productID
	return p
}

// truncate returns s truncated to at most n bytes, appending "..." when cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// unresolvedImportHit describes a single failing import found by the
// autofix scan. Path is repo-relative.
type unresolvedImportHit struct {
	File   string
	Line   int
	Import string
	Reason string
}

// collectUnresolvedImports walks the worktree's modified files (per
// `git status --porcelain`) and runs codex_verify_imports on each TS/JS
// file. Returns the flat list of unresolved imports across all files.
// Deterministic — no LLM, just regex + stat.
func collectUnresolvedImports(ctx context.Context, logger *logging.Logger, worktreePath, taskID string) []unresolvedImportHit {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var paths []string
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		// Porcelain format: "XY path" — skip the 2-char status and the
		// following space, plus handle "??" for untracked.
		path := strings.TrimSpace(line[3:])
		// Rename status shows as "R  old -> new" — take the destination.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	return unresolvedImportsIn(ctx, logger, worktreePath, taskID, paths)
}

// unresolvedImportsIn runs codex_verify_imports on each TS/JS file in
// paths (repo-relative) and returns the flat list of unresolved imports.
// Files that no longer exist on disk are skipped.
func unresolvedImportsIn(ctx context.Context, logger *logging.Logger, worktreePath, taskID string, paths []string) []unresolvedImportHit {
	hits, _ := inspectImportsIn(ctx, logger, worktreePath, taskID, paths)
	return hits
}

// inspectImportsIn is unresolvedImportsIn plus the number of files the
// verifier actually inspected: files with a TS/JS extension that exist on
// disk and were verified without error. The final gate uses the count to
// tell "inspected N files and found nothing" from "nothing was applicable",
// which must not be recorded as a passed check.
func inspectImportsIn(ctx context.Context, logger *logging.Logger, worktreePath, taskID string, paths []string) (hits []unresolvedImportHit, inspected int) {
	for _, path := range paths {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		default:
			continue
		}
		full := filepath.Join(worktreePath, filepath.FromSlash(path))
		data, readErr := os.ReadFile(full)
		if readErr != nil {
			continue
		}
		result, vErr := verify.VerifyImports(worktreePath, path, string(data))
		if vErr != nil {
			logger.Warn(ctx, "verify_imports error", map[string]interface{}{"task_id": taskID, "file": path, "err": vErr.Error()})
			continue
		}
		inspected++
		for _, u := range result.Unresolved {
			hits = append(hits, unresolvedImportHit{
				File: path, Line: u.Line, Import: u.Import, Reason: u.Reason,
			})
		}
	}
	return hits, inspected
}

// ── Pre-flight build health check ───────────────────────────────────────────

type preflightError struct {
	File    string
	Line    int
	Message string
}

func preflightFingerprint(pe preflightError) string {
	return filepath.Clean(pe.File) + ":" + pe.Message
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// filePathRe matches common source-code file paths: at least one directory
// separator followed by a filename with a recognised extension.
var filePathRe = regexp.MustCompile(`(?:^|[\s` + "`" + `"'(\[])([a-zA-Z0-9_./-]+\.(?:go|ts|tsx|js|jsx|json|yaml|yml|css|scss|html|py|rs|toml|sql|proto|graphql|svelte|vue|md))(?:$|[\s` + "`" + `"')\]])`)

// filesMarkerRe matches the "Files: [path1 path2 ...]" markers that the
// planner writes into step descriptions. Go's %v for a string slice
// produces "[a b c]" — space-separated, no commas.
var filesMarkerRe = regexp.MustCompile(`Files:\s*\[([^\]]+)\]`)

// extractFilePathsFromDescription scans description text for file-path-like
// strings and returns unique paths. This is a last-resort fallback when the
// planner's structured FilesToChange was empty — descriptions written by
// decomposeIntoTasks routinely reference target files in prose even when the
// JSON `files` field was omitted.
//
// Two extraction strategies run in sequence:
//  1. Parse explicit "Files: [...]" markers (most reliable).
//  2. Scan prose for path-shaped tokens with recognised extensions.
func extractFilePathsFromDescription(desc string) []string {
	seen := make(map[string]struct{})
	var paths []string

	addPath := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, ".") || strings.Count(p, "/") == 0 {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	// Strategy 1: explicit Files markers — "Files: [src/a.ts src/b.go]"
	for _, m := range filesMarkerRe.FindAllStringSubmatch(desc, -1) {
		for _, token := range strings.Fields(m[1]) {
			addPath(token)
		}
	}

	// Strategy 2: freeform path-shaped tokens in prose.
	for _, m := range filePathRe.FindAllStringSubmatch(desc, -1) {
		addPath(m[1])
	}

	return paths
}

// preflightBuildCheck uses the LSP manager to open a sample of files from the
// worktree and check for Error-severity diagnostics BEFORE the agent makes any
// changes. Returns the list of pre-existing errors; empty means the build is
// clean enough to proceed.
func preflightBuildCheck(ctx context.Context, executor *tools.Executor, worktreePath string, filesToChange []string) []preflightError {
	mgr := executor.LSPManager()
	if mgr == nil {
		return nil
	}

	// Guard: if deps are still missing in the worktree after ensureDepsInstalled
	// + symlinkDeps ran, don't run LSP — it will report hundreds of false
	// import errors. This shouldn't happen in normal flow (ensureDepsInstalled
	// blocks earlier) but protects against edge cases.
	if _, err := os.Stat(filepath.Join(worktreePath, "package.json")); err == nil {
		if _, err := os.Lstat(filepath.Join(worktreePath, "node_modules")); err != nil {
			log.Printf("preflight: package.json exists but node_modules missing in worktree (symlink may have failed) — skipping LSP check")
			return nil
		}
	}

	lspCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, language, err := mgr.ClientForRepo(lspCtx, worktreePath)
	if err != nil {
		log.Printf("preflight: LSP unavailable (%v) — skipping build check", err)
		return nil
	}

	// Collect candidate files: the planner's filesToChange first, then a
	// few additional source files from the repo to catch project-wide
	// breakage (e.g., a missing dependency that affects every file).
	candidates := make([]string, 0, len(filesToChange)+3)
	for _, f := range filesToChange {
		candidates = append(candidates, f)
	}
	// Add a few seed files from the repo root to catch project-wide issues.
	if extra := findPreflightSeedFiles(worktreePath, language, 3); len(extra) > 0 {
		candidates = append(candidates, extra...)
	}

	// Deduplicate and cap at a reasonable number.
	seen := make(map[string]bool)
	var unique []string
	for _, c := range candidates {
		norm := filepath.ToSlash(c)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		unique = append(unique, norm)
		if len(unique) >= 10 {
			break
		}
	}

	// Open each file and poll for Error-severity diagnostics.
	var found []preflightError
	for _, relPath := range unique {
		fullPath := filepath.Join(worktreePath, filepath.FromSlash(relPath))
		data, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			continue
		}
		if len(data) > 1024 && looksBinaryPrefix(data[:1024]) {
			continue
		}

		_, langID := lspLanguageForFile(relPath)
		if langID == "" {
			continue
		}

		uri := lsp.PathToURI(fullPath)
		if openErr := client.OpenDoc(uri, langID, string(data)); openErr != nil {
			continue
		}

		// Poll for diagnostics (same pattern as validateAfterWrite).
		deadline := time.Now().Add(3 * time.Second)
		var fileDiags []lsp.Diagnostic
		for time.Now().Before(deadline) {
			diags := client.Diagnostics(uri)
			fileDiags = fileDiags[:0]
			for _, d := range diags {
				if d.Severity == 1 {
					fileDiags = append(fileDiags, d)
				}
			}
			if len(fileDiags) > 0 {
				break
			}
			select {
			case <-time.After(200 * time.Millisecond):
			case <-lspCtx.Done():
				deadline = time.Now()
			}
		}

		for _, d := range fileDiags {
			found = append(found, preflightError{
				File:    relPath,
				Line:    d.Range.Start.Line + 1,
				Message: d.Message,
			})
		}
	}

	// Filter out dependency-resolution errors — these are never real code
	// errors and need `npm install` / `pip install`, not a bug ticket.
	var real []preflightError
	depErrors := 0
	for _, e := range found {
		msg := strings.ToLower(e.Message)
		if strings.Contains(msg, "cannot find module") ||
			strings.Contains(msg, "could not find a declaration file") ||
			strings.Contains(msg, "module not found") ||
			strings.Contains(msg, "no module named") ||
			strings.Contains(msg, "cannot resolve") {
			depErrors++
			continue
		}
		real = append(real, e)
	}
	if depErrors > 0 {
		log.Printf("preflight: filtered %d dependency-resolution errors (missing modules — not code errors)", depErrors)
	}
	if len(real) > 0 {
		log.Printf("preflight: %d real error-severity diagnostic(s) found across %d files before implementation started", len(real), len(unique))
	}
	return real
}

func preflightErrorFiles(errs []preflightError) []string {
	seen := make(map[string]bool)
	var out []string
	for _, e := range errs {
		if !seen[e.File] {
			seen[e.File] = true
			out = append(out, e.File)
		}
	}
	return out
}

// findPreflightSeedFiles walks the worktree for source files in the detected
// language, returning up to `limit` repo-relative paths. Skips vendor/build dirs.
func findPreflightSeedFiles(root, language string, limit int) []string {
	exts := map[string][]string{
		"typescript": {".ts", ".tsx"},
		"go":         {".go"},
		"python":     {".py"},
		"rust":       {".rs"},
		"java":       {".java"},
		"csharp":     {".cs"},
		"cpp":        {".cpp", ".cc", ".c", ".h"},
		"php":        {".php"},
	}[language]
	if len(exts) == 0 {
		return nil
	}

	var found []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".next", "dist", "build", "vendor", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(d.Name())
		for _, e := range exts {
			if ext == e {
				rel, relErr := filepath.Rel(root, p)
				if relErr == nil {
					found = append(found, filepath.ToSlash(rel))
				}
				if len(found) >= limit {
					return filepath.SkipAll
				}
				break
			}
		}
		return nil
	})
	return found
}

// lspLanguageForFile returns (language, languageID) for a file path based on
// its extension. Mirrors the lspLanguageForExt helper in executor.go.
func lspLanguageForFile(relPath string) (string, string) {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".go":
		return "go", "go"
	case ".ts":
		return "typescript", "typescript"
	case ".tsx":
		return "typescript", "typescriptreact"
	case ".js":
		return "javascript", "javascript"
	case ".jsx":
		return "javascript", "javascriptreact"
	case ".mjs", ".cjs":
		return "javascript", "javascript"
	case ".py":
		return "python", "python"
	case ".rs":
		return "rust", "rust"
	case ".java":
		return "java", "java"
	case ".cs":
		return "csharp", "csharp"
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "cpp", "cpp"
	case ".c", ".h":
		return "c", "c"
	case ".php":
		return "php", "php"
	default:
		return "", ""
	}
}

func looksBinaryPrefix(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// ensureDepsInstalled checks whether the main clone has its dependencies
// installed and tries to auto-install them if missing. Returns "" on
// success or a human-readable error string describing what's wrong.
func ensureDepsInstalled(ctx context.Context, clonePath string) string {
	type depCheck struct {
		manifest string // e.g. "package.json"
		depDir   string // e.g. "node_modules"
		install  string // e.g. "npm install"
		cmd      string // executable
		args     []string
	}
	checks := []depCheck{
		{"package.json", "node_modules", "npm install", "npm", []string{"install", "--no-audit", "--no-fund"}},
		{"requirements.txt", "venv", "pip install -r requirements.txt", "", nil}, // no auto-install for Python (needs venv)
		{"Gemfile", "vendor/bundle", "bundle install", "bundle", []string{"install"}},
		{"composer.json", "vendor", "composer install", "composer", []string{"install", "--no-interaction"}},
	}

	for _, c := range checks {
		manifest := filepath.Join(clonePath, c.manifest)
		if _, err := os.Stat(manifest); err != nil {
			continue // this project type doesn't apply
		}
		depDir := filepath.Join(clonePath, c.depDir)
		if info, err := os.Lstat(depDir); err == nil && (info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			continue // deps already present
		}

		log.Printf("ensureDepsInstalled: %s exists but %s is missing — attempting %s", c.manifest, c.depDir, c.install)

		if c.cmd == "" {
			return fmt.Sprintf("`%s` found but `%s` is missing. Auto-install is not supported for this project type — please run `%s` manually in the repository.", c.manifest, c.depDir, c.install)
		}

		installCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		cmd := exec.CommandContext(installCtx, c.cmd, c.args...)
		cmd.Dir = clonePath
		out, err := cmd.CombinedOutput()
		cancel()

		if err != nil {
			snippet := string(out)
			if len(snippet) > 500 {
				snippet = snippet[len(snippet)-500:]
			}
			return fmt.Sprintf("`%s` failed:\n```\n%s\n```\nPlease fix the issue and retry.", c.install, snippet)
		}

		// Verify it worked
		if _, err := os.Stat(depDir); err != nil {
			return fmt.Sprintf("`%s` completed but `%s` still doesn't exist. The install may have used a non-standard path.", c.install, c.depDir)
		}

		log.Printf("ensureDepsInstalled: %s succeeded — %s now exists", c.install, c.depDir)
	}

	return ""
}

// symlinkDeps symlinks untracked dependency directories from the main
// clone into a worktree. Git worktrees only contain tracked files;
// gitignored dirs like node_modules don't exist, which breaks every
// import under the LSP and at runtime.
//
// A link is a file to git, so a directory-only ignore pattern such as
// "node_modules/" does not cover it and `git add -A` would commit it
// (a live run, attempt 6, did). Each link name is therefore written, anchored, to
// the repository's info/exclude before the link is created; stageAll's
// guard refuses the commit should one be staged anyway. Returns the names
// linked.
func symlinkDeps(clonePath, worktreePath string) []string {
	var linked []string
	for _, dir := range depSymlinkDirs {
		src := filepath.Join(clonePath, dir)
		dst := filepath.Join(worktreePath, dir)
		if info, err := os.Lstat(src); err == nil && info.IsDir() {
			if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				// M2: a worktree made before the exclude entries existed
				// already has the link; exclude it now.
				if target, rlErr := os.Readlink(dst); rlErr == nil && samePath(filepath.Clean(target), filepath.Clean(src)) {
					if exErr := excludeFromGit(worktreePath, "/"+dir); exErr != nil {
						log.Printf("symlinkDeps: cannot exclude existing link %s from git: %v — the commit guard will refuse it if staged", dir, exErr)
					}
				}
			} else if err != nil {
				if exErr := excludeFromGit(worktreePath, "/"+dir); exErr != nil {
					log.Printf("symlinkDeps: cannot exclude %s from git: %v — the commit guard will refuse it if staged", dir, exErr)
				}
				if linkErr := os.Symlink(src, dst); linkErr != nil {
					log.Printf("symlinkDeps: symlink failed %s -> %s: %v — trying copy", dst, src, linkErr)
					// Fallback: create a tiny marker directory and bind the
					// real path via a relative symlink won't work in all
					// Docker volume configs. Instead, just copy the top-level
					// directory listing as stub (won't help LSP). Log loudly.
					log.Printf("symlinkDeps: WARNING — %s will be missing in worktree, LSP may report false errors", dir)
				} else {
					log.Printf("symlinkDeps: linked %s -> %s", dst, src)
					linked = append(linked, dir)
				}
			}
		} else {
			log.Printf("symlinkDeps: %s not found in main clone (or not a dir)", src)
		}
	}
	return linked
}

// findExistingAgentBranch looks for remote branches matching the pattern
// agent/{workID}/* and returns the most recent one (last alphabetically).
// This is used by review-fix tasks to branch from the original implementation
// rather than starting from baseBranch with none of the prior changes.
func findExistingAgentBranch(ctx context.Context, repoPath, workID string) string {
	cmd := exec.CommandContext(ctx, "git", "branch", "-r", "--list", fmt.Sprintf("origin/agent/%s/*", workID))
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// Return the last branch (most recently created by naming convention).
	branch := strings.TrimSpace(lines[len(lines)-1])
	// Strip "origin/" prefix — callers add it back when needed.
	return strings.TrimPrefix(branch, "origin/")
}
