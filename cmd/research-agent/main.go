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
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "research-agent"
	consumerGroup = "research-agent"
	healthPort    = 8093

	// vectorCollection is the Qdrant collection the knowledge-indexer writes to.
	// It MUST match the constant in cmd/knowledge-indexer/main.go so query-time
	// codex_similar_code lookups hit the same dataset the indexer populated.
	vectorCollection = "codebase"
)

// repoRef is a minimal reference to a repository.
type repoRef struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ClonePath string `json:"clone_path"`
	// BaseSHA is the repository's pinned base commit (M2-02), empty for a
	// legacy-unpinned item. Research reads the snapshot worktree at this
	// commit (M2-03, snapshot.go).
	BaseSHA string `json:"base_sha,omitempty"`
}

// researchTaskMessage is received from research.tasks / research.additional.
type researchTaskMessage struct {
	TaskID          string    `json:"task_id"`
	WorkID          string    `json:"work_id"`
	ProductID       string    `json:"product_id"`
	RepositoryID    string    `json:"repository_id,omitempty"`
	ResearchType    string    `json:"research_type"`
	StartingContext string    `json:"starting_context"`
	CodexSections   string    `json:"codex_sections"`
	RepoList        []repoRef `json:"repo_list,omitempty"`
	seedPayload
}

// Research result status values. Every research.results message carries
// one of these so the coordinator can distinguish an honest success from
// a tool-loop safety-limit hit or an LLM timeout. Previously, all of these
// cases produced a message with empty evidence and the coordinator counted
// them toward convergence — that's how hallucinated PRDs got produced from
// an agent that never actually finished research.
const (
	ResearchStatusOK       = "ok"        // completed normally with evidence
	ResearchStatusLimitHit = "limit_hit" // tool loop hit maxIterations safety valve
	ResearchStatusTimedOut = "timed_out" // LLM call or context deadline exceeded
	ResearchStatusError    = "error"     // any other failure mode
	// ResearchStatusLoopStuck (R17): the tool loop ended with a LoopStuck
	// (a cycle, a third repeat of the same read, an identical failing
	// call); the result's loop_stuck says which. Like every non-ok status
	// the coordinator's honest-failure gate records it as a failed task.
	ResearchStatusLoopStuck = "loop_stuck"
	// ResearchStatusNoEvidence (R18): the model answered without running a
	// tool call, again after the evidence nudge, or every call it made
	// failed; the answer is kept as the result's note. A non-ok status
	// like the others: the coordinator's gate records a failed task.
	ResearchStatusNoEvidence = "no_evidence"
)

// researchResultMessage is published to research.results. A result without
// Status=="ok" is a failure signal — consumers (research-coordinator) must
// NOT treat it as successful research, regardless of whether Evidence is
// non-empty. An empty/missing Status field is also treated as a failure
// (legacy-compat: old messages predating this field are not trustworthy).
//
// The structured Affected*/Relevant* fields are populated deterministically
// from the tool-call history during the research loop (Approach A in the
// pivot-B plan). They preserve concrete identifiers (file paths, module
// names, pattern names) that were previously re-extracted by a downstream
// LLM from prose Evidence — every re-extraction hop lost information and
// burned tokens. If Agent A's codex_find_module / codex_patterns_for tools
// have not landed yet, these slices simply stay empty; downstream consumers
// tolerate empty slices (omitempty keeps the wire payload compact).
type researchResultMessage struct {
	TaskID              string  `json:"task_id"`
	WorkID              string  `json:"work_id"`
	Status              string  `json:"status"` // "ok" | "limit_hit" | "timed_out" | "error" | "loop_stuck" | "no_evidence"
	FindingType         string  `json:"finding_type"`
	Evidence            string  `json:"evidence"`
	Confidence          float64 `json:"confidence"`
	FollowupSuggestions string  `json:"followup_suggestions"`
	ErrorDetail         string  `json:"error_detail,omitempty"`
	// LoopStuck is the structured stuck failure (status loop_stuck).
	LoopStuck *tools.LoopStuck `json:"loop_stuck,omitempty"`
	// Note is the model's tool-less answer on a no_evidence result (R18),
	// kept for the reader; it is not evidence.
	Note string `json:"note,omitempty"`

	// Criteria are this task's acceptance criteria and evidence map (M2-04),
	// grounded against the tool calls it made. Present on every ok result
	// except when the pass was skipped because the token budget was spent.
	Criteria *plan.BriefCriteria `json:"criteria,omitempty"`

	// Structured research output — populated from the tool-call side-channel
	// captured during the research tool loop. Deterministic (no LLM prompt),
	// so it survives weak LLMs that produce prose Evidence but can't be
	// trusted to emit valid JSON summaries.
	AffectedFiles        []string `json:"affected_files,omitempty"`         // concrete file paths touched by read_file / returned by search_code
	AffectedModules      []string `json:"affected_modules,omitempty"`       // codex module names from codex_find_module results
	AffectedZones        []string `json:"affected_zones,omitempty"`         // code_zones.name values derived from affected modules
	CrossRepoTouchpoints []string `json:"cross_repo_touchpoints,omitempty"` // repo names whose clone_path prefixes an affected file
	RelevantPatterns     []string `json:"relevant_patterns,omitempty"`      // pattern names from codex_patterns_for results
	RelevantAntiPatterns []string `json:"relevant_anti_patterns,omitempty"` // anti-pattern names to AVOID, from codex tooling

	// BaseSHAs maps repository id → the pinned commit this research read
	// (its snapshot worktree, M2-03); omitted when nothing was pinned.
	BaseSHAs map[string]string `json:"base_shas,omitempty"`
	// CodexVersion is the product_version (migration 060) of the codex on
	// CodexBranch when the research ran: the Codex it was grounded on.
	// Pre-060 builds sent no CodexBranch and a global version_number here;
	// the coordinator ignores a CodexVersion without a CodexBranch.
	CodexVersion int    `json:"codex_version,omitempty"`
	CodexBranch  string `json:"codex_branch,omitempty"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("research-agent: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("research-agent: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("research-agent: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
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

	// Outbox dispatcher (M1-04). research.results and work.blocked are
	// committed to outbox_events with the state they announce; this loop
	// carries them to Kafka, so a failure to start is fatal. tool.executed
	// and console.stream are telemetry and stay direct.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), agentID)
	if err != nil {
		log.Fatalf("research-agent: start outbox dispatcher: %v", err)
	}
	log.Printf("research-agent: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M2-03: reclaim snapshot worktrees no non-terminal pin references and
	// nobody used for 7 days. Background, bounded; never blocks startup.
	go pruneSnapshotsOnStartup(ctx, pool)

	topics := []string{bus.TopicResearchTasks, bus.TopicResearchAdditional}
	// M1-05: deduplicate on the processed-event ledger. The up-front Seen
	// check acknowledges a recorded duplicate before any LLM work.
	handler := idempotency.NewLedger(pool).Handler("research-agent", buildHandler(ctx, pool, cfg.Qdrant.URL, registry, cs, wg, kb, cx))
	if err := kb.SubscribeMultiple(topics, consumerGroup, handler); err != nil {
		log.Fatalf("research-agent: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("research-agent: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("research-agent: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("research-agent: shutting down...")
	cancel()
	dispatcher.Wait()
	log.Println("research-agent: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("research-agent: health server shutdown error: %v", err)
	}
	log.Println("research-agent: stopped")
}

func buildHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	qdrantURL string,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	wg *workgraph.WorkGraph,
	kb bus.MessageBus,
	cx *codex.Codex,
) bus.Handler {
	logger := logging.New("research-agent")
	// One LSP manager for the process (R1): tasks share warm servers and the
	// readiness-failure cooldown.
	lspMgr := lsp.NewManager()
	return func(msgCtx context.Context, msg *bus.Message) error {
		var task researchTaskMessage
		if err := json.Unmarshal(msg.Value, &task); err != nil {
			log.Printf("research-agent: unmarshal task: %v", err)
			return nil
		}

		workID, err := uuid.Parse(task.WorkID)
		if err != nil {
			log.Printf("research-agent: parse work_id %q: %v", task.WorkID, err)
			return nil
		}

		// Attach work item ID to context for structured log correlation.
		msgCtx = logging.WithWorkItemID(msgCtx, workID.String())

		logger.Info(msgCtx, "processing task", map[string]interface{}{
			"task_id":       task.TaskID,
			"work_id":       task.WorkID,
			"research_type": task.ResearchType,
			"product_id":    task.ProductID,
			"repo_id":       task.RepositoryID,
		})

		// Record agent assignment for this work item.
		assignment := &models.AgentAssignment{
			WorkItemID:      workID,
			AgentType:       models.AgentTypeResearch,
			AgentInstanceID: agentID,
			Status:          models.AssignmentStatusActive,
		}
		if err := wg.AssignAgent(msgCtx, assignment); err != nil {
			log.Printf("research-agent: assign agent: %v", err)
		}

		// report ends the task: the research.results event and the
		// assignment's completion commit together. The coordinator only
		// converges when every task has reported, so a result that cannot be
		// recorded must not vanish into a log line — the work item is blocked
		// with the cause instead.
		wi, wiErr := wg.GetWorkItem(msgCtx, workID)
		if wiErr != nil {
			wi = nil
		}
		aggregateID := aggregateFor(workID, wi)
		resultKey := fmt.Sprintf("%s-%s", task.WorkID, task.TaskID)
		// M2-03: every result records the snapshot it read (repository id →
		// pinned base_sha; set once the read paths are resolved) and the
		// Codex version research was grounded on.
		var resultBaseSHAs map[string]string
		// Both are set where the codex branch is resolved (codex grounding).
		resultCodexVersion, resultCodexBranch := 0, ""
		report := func(status models.AssignmentStatus, result researchResultMessage) {
			result.BaseSHAs = resultBaseSHAs
			result.CodexVersion = resultCodexVersion
			result.CodexBranch = resultCodexBranch
			// M1-05: the result's transaction carries the processed-event
			// guard row (Wrap).
			err := commitResearchResult(msgCtx, idempotency.Wrap(msgCtx, pool), wg, aggregateID, assignment.ID, status, resultKey, result)
			if idempotency.IsDuplicate(err) {
				log.Printf("research-agent: task_id=%s already reported — duplicate delivery", task.TaskID)
				return
			}
			if err != nil {
				log.Printf("research-agent: commit research.results for task_id=%s: %v", task.TaskID, err)
				reason := fmt.Sprintf("Research task %s (%s) finished but its result could not be recorded: %v. Retry the work item.",
					task.TaskID, task.ResearchType, err)
				if bErr := blockInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason); bErr != nil {
					log.Printf("research-agent: block work item: %v", bErr)
				}
				wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			}
		}

		// Determine which repo path(s) to use for this task.
		selectedRepos := selectResearchRepos(task)
		var repoPaths []string
		for _, r := range selectedRepos {
			repoPaths = append(repoPaths, r.ClonePath)
		}

		// Validate context — at least one repo must be available.
		repoAvailable := false
		for _, rp := range repoPaths {
			if guards.CheckRepoAvailable(rp).Valid {
				repoAvailable = true
				break
			}
		}

		if !repoAvailable {
			log.Printf("research-agent: no repo available for task_id=%s", task.TaskID)
			// Publish with Status=error — the coordinator needs to know this
			// task failed so it can block honestly instead of converging on
			// empty evidence and treating it as successful research.
			noContextResult := researchResultMessage{
				TaskID:              task.TaskID,
				WorkID:              task.WorkID,
				Status:              ResearchStatusError,
				FindingType:         task.ResearchType,
				Evidence:            "Unable to investigate — no repository available on disk.",
				Confidence:          0.0,
				FollowupSuggestions: "",
				ErrorDetail:         "no repo_available on disk for any configured repo path",
			}
			report(models.AssignmentStatusFailed, noContextResult)
			return nil
		}

		// M2-03: a pinned repository is read from its snapshot worktree at
		// the pinned commit, never from the shared clone's working tree.
		pinned, snapErr := pinnedReadPaths(msgCtx, newSnapshotDeps(pool), workID, selectedRepos)
		if snapErr != nil {
			log.Printf("research-agent: task_id=%s: %v", task.TaskID, snapErr)
			report(models.AssignmentStatusFailed, researchResultMessage{
				TaskID:      task.TaskID,
				WorkID:      task.WorkID,
				Status:      ResearchStatusError,
				FindingType: task.ResearchType,
				Evidence:    "Unable to investigate — the pinned repository snapshot could not be prepared.",
				ErrorDetail: snapErr.Error(),
			})
			return nil
		}
		repoPaths = pinned.Paths
		resultBaseSHAs = pinned.BaseSHAs

		// Resolve LLM adapter per work item: checks agent_definition first, falls back to global.
		resolution, err := registry.Resolve(msgCtx, workID, cs, "research-agent", "instance-1")
		var llmAdapter adapter.ModelAdapter
		var agentDef *agentregistry.AgentDefinition
		if resolution != nil {
			llmAdapter, agentDef = resolution.Adapter, resolution.Definition
		}
		if err == nil {
			// M4-05: tag this task's inference rows (role research, snapshot).
			msgCtx = llm.WithInferenceTags(msgCtx, llm.TagsForResolution(resolution, workID))
		}
		if err != nil {
			log.Printf("research-agent: resolve adapter for work_id=%s: %v", workID, err)
			reason := "Cannot create LLM adapter: provider not configured or credentials missing. Configure model credentials in Admin → Credentials."
			if blockErr := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); blockErr != nil {
				log.Printf("research-agent: block work item: %v", blockErr)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		// Use the first available repo as the primary target.
		primaryPath := repoPaths[0]
		for _, rp := range repoPaths {
			if guards.CheckRepoAvailable(rp).Valid {
				primaryPath = rp
				break
			}
		}

		// Resolve product_id for this task so codex tools are scoped to the
		// right product. If it's empty/invalid we still create the executor;
		// the codex tools themselves return a clean "unavailable" error and
		// the LLM falls back to search_code.
		var productIDPtr *uuid.UUID
		if task.ProductID != "" {
			if pid, pidErr := uuid.Parse(task.ProductID); pidErr == nil {
				productIDPtr = &pid
			}
		}

		// Build a VectorStore for codex_similar_code. Query-time vectors
		// MUST be produced by the same provider + model the knowledge-indexer
		// wrote with, otherwise the dot product is meaningless and hits score
		// close to zero regardless of relevance. We resolve the default
		// embedding provider from the DB (embedding_providers table) via the
		// configstore. If no provider is configured or the lookup fails, the
		// vectorStore is left nil and codex_similar_code returns a clean
		// error — the LLM falls back to search_code.
		var vectorStore *vector.VectorStore
		if qdrantURL != "" && cs != nil {
			embSvc, _, embErr := vector.BuildEmbeddingService(msgCtx, pool, cs)
			if embErr != nil {
				log.Printf("research-agent: embedding provider unavailable — codex_similar_code disabled: %v", embErr)
				vectorStore = vector.New(qdrantURL, vectorCollection)
			} else {
				vectorStore = vector.NewWithEmbeddings(qdrantURL, vectorCollection, embSvc)
			}
		} else if qdrantURL != "" {
			// configstore not available (no master key set) — still expose
			// a client so the Search path (raw vector) stays callable, but
			// codex_similar_code will error because it needs an embedding
			// service. This preserves prior no-master-key behaviour.
			vectorStore = vector.New(qdrantURL, vectorCollection)
		}

		// Create executor rooted at the real repo, with the codex + vector
		// store + product_id wired in so the codex_* tools can serve real
		// data. kb + workID enable codex.gap event emission.
		executor := tools.NewExecutorWithCodex(primaryPath, cx, vectorStore, productIDPtr, kb, &workID)

		// Scope codex queries to the work item's branch snapshot.
		if branch, headSHA := tools.ResolveWorkItemBranch(ctx, pool, workID); branch != "" {
			executor.SetWorkItemBranch(branch, headSHA)
		}

		// Publish each tool call to Kafka so the dashboard's Live Activity
		// panel streams research progress in real time. Mirrors the pattern
		// in implementation-agent. Without this, research phases appear as
		// a static "research" state with no visible activity for 10+ minutes.
		//
		// The SAME callback also feeds a process-local toolHistory so we can
		// derive structured research output (affected_files, affected_modules,
		// ...) without adding a second layer of plumbing to the executor. The
		// dashboard stream and the structured capture are independent consumers
		// of one event stream — no duplicated publish, no duplicated cost.
		workIDForCb := workID.String()
		taskIDForCb := task.TaskID
		history := newToolHistory()
		executor.SetCallback(func(callID, toolName, arguments, result string, isError bool, _ tools.ResultMeta) {
			// Side-channel capture first: the dashboard publish can fail without
			// affecting structured output derivation, and vice versa.
			history.record(callID, toolName, arguments, result, isError)

			event := map[string]interface{}{
				"work_item_id":   workIDForCb,
				"task_id":        taskIDForCb,
				"tool":           toolName,
				"arguments":      arguments,
				"result_preview": truncate(result, 200),
				"is_error":       isError,
				"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
			}
			payload, _ := json.Marshal(event)
			kb.Publish(ctx, bus.TopicToolExecuted, workIDForCb, payload) //nolint:errcheck
			consoleEvent := map[string]interface{}{
				"work_item_id": workIDForCb,
				"task_id":      taskIDForCb,
				"agent_type":   "research-agent",
				"tool":         toolName,
				"arguments":    arguments,
				"result":       result,
				"is_error":     isError,
				"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
			}
			consolePayload, _ := json.Marshal(consoleEvent)
			kb.Publish(ctx, bus.TopicConsoleStream, workIDForCb, consolePayload) //nolint:errcheck
		})

		// Codex grounding (B1): resolve the branch (the item's, else the
		// product's default, else main) — the codex rejects an empty
		// branch — and log a failed lookup once instead of dropping it.
		codexContext := ""
		if cx != nil && productIDPtr != nil {
			itemBranch := ""
			if wi != nil && wi.Branch != nil {
				itemBranch = *wi.Branch
			}
			branch, branchErr := resolveCodexBranch(msgCtx, itemBranch, func(c context.Context) (string, error) {
				return codex.ResolveDefaultBranch(c, pool, *productIDPtr)
			})
			// Read before the grounding: a version written meanwhile makes
			// the pin older than what was read (a needless re-plan later),
			// never newer.
			resultCodexVersion, resultCodexBranch = codexVersionFor(msgCtx, cx, *productIDPtr, branch), branch
			grounding, gErr := buildCodexGrounding(msgCtx, cx, *productIDPtr, branch)
			if err := errors.Join(branchErr, gErr); err != nil {
				// One line per task covers the branch lookup and every
				// failed codex read.
				log.Printf("research-agent: task_id=%s: codex grounding incomplete (branch %s): %v", task.TaskID, branch, err)
			}
			codexContext = grounding
		}

		// Resolve operating mode so we can tier-adjust the system prompt.
		operatingMode := models.OperatingModeStandard
		if agentDef != nil && agentDef.Provider != "" && agentDef.ModelID != "" {
			if mode, modeErr := registry.GetOperatingMode(msgCtx, agentDef.Provider, agentDef.ModelID); modeErr == nil {
				operatingMode = mode
			}
		}
		scaffold := strategy.PromptScaffolding(operatingMode)

		// R1: the system prompt is role + strategy + type guidance + codex
		// grounding + the tier preamble, assembled by the same function the
		// floor test measures; the PRD travels once, in the user message.
		systemPrompt := assembleResearchSystemPrompt(task.ResearchType, codexContext, operatingMode)
		userMessage := buildResearchUserMessage(task, primaryPath,
			snapshotPromptLine(selectedRepos, repoPaths, primaryPath, resultBaseSHAs))

		// R1: one process-wide LSP manager, so a warm server and a cached
		// readiness failure outlive the task. Warm it in the background; the
		// LSP tools are offered only once it reports ready.
		executor.SetLSPManager(lspMgr)
		go executor.WarmLSP(ctx)

		userMessage += seedsSection(task)

		req := adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{
				{Role: adapter.RoleSystem, Content: systemPrompt},
				{Role: adapter.RoleUser, Content: userMessage},
			},
			Tools:       researchToolSet(task.ResearchType, executor.LSPReady(msgCtx)),
			ToolChoice:  "auto",
			MaxTokens:   8192,
			Temperature: 0.2,
		}

		logger.Info(msgCtx, "running tool loop", map[string]interface{}{
			"task_id":       task.TaskID,
			"research_type": task.ResearchType,
		})

		// Run the tool loop — the LLM will actually read files and search code.
		// MaxIterations comes from the prompt scaffold (tier-adjusted).
		maxIters := scaffold.MaxIterations
		if maxIters <= 0 {
			maxIters = 15
		}
		resp, err := tools.RunToolLoopWithOptions(msgCtx, llmAdapter, req, executor, maxIters, pool, &workID,
			researchTaskLoopOptions(task, func() bool { return executor.LSPReady(msgCtx) }))
		if err != nil {
			// Classify the failure mode so the coordinator can respond to
			// the specific cause. Every branch here publishes a result with
			// a non-"ok" Status — we deliberately do NOT drop the Kafka
			// publish, because the coordinator needs to know the task
			// failed, not that it "didn't respond". Otherwise the
			// coordinator's convergence timer fires on a partial count and
			// the pipeline treats missing results as success.

			if errors.Is(err, tools.ErrToolCallingNotSupported) {
				log.Printf("research-agent: model does not support tool calling: %v", err)
				failResult := researchResultMessage{
					TaskID:      task.TaskID,
					WorkID:      task.WorkID,
					Status:      ResearchStatusError,
					FindingType: task.ResearchType,
					Evidence: "Unable to investigate: the configured LLM model does not support tool calling. " +
						"Research requires tool calling to read files and search code. " +
						"Action needed: assign a tool-calling capable model to the research agent.",
					Confidence:  0.0,
					ErrorDetail: err.Error(),
				}
				report(models.AssignmentStatusFailed, failResult)
				return nil
			}

			logger.Error(msgCtx, "tool loop failed", err, map[string]interface{}{
				"task_id": task.TaskID,
			})

			report(models.AssignmentStatusFailed, loopFailureResult(task, err))
			return nil
		}

		evidence := ""
		if resp != nil {
			evidence = resp.Content
		}

		// R18 E1: an answer is ok only when at least one tool call
		// returned something (the loop refuses an answer before any call;
		// here, every call it ran may still have failed).
		if r, failed := noEvidenceResult(task, evidence, history.snapshot()); failed {
			log.Printf("research-agent: task_id=%s: %s", task.TaskID, r.ErrorDetail)
			report(models.AssignmentStatusFailed, r)
			return nil
		}

		// Derive structured fields from the tool-call side-channel. Approach A
		// from the pivot-B plan: deterministic, no second LLM call. If Agent
		// A's codex_find_module / codex_patterns_for tools haven't landed
		// yet, the codex-derived slices just stay nil — Evidence still
		// ships, downstream still works, and the next deploy upgrades the
		// payload automatically.
		var structuredProductID uuid.UUID
		if task.ProductID != "" {
			if pid, perr := uuid.Parse(task.ProductID); perr == nil {
				structuredProductID = pid
			}
		}
		sf := deriveStructuredFields(msgCtx, pool, structuredProductID, history.snapshot())

		// Acceptance criteria pass (M2-04, criteria.go). Output that is still
		// invalid after one re-prompt fails the task with the parse error, so
		// the coordinator blocks the item on it instead of planning without.
		criteria, critErr := runCriteriaPass(msgCtx, pool, llmAdapter, workID, wi, task, repoPaths, evidence, history.snapshot())
		if critErr != nil {
			log.Printf("research-agent: task_id=%s: %v", task.TaskID, critErr)
			report(models.AssignmentStatusFailed, researchResultMessage{
				TaskID:      task.TaskID,
				WorkID:      task.WorkID,
				Status:      ResearchStatusError,
				FindingType: task.ResearchType,
				Evidence:    evidence,
				Confidence:  0.0,
				ErrorDetail: critErr.Error(),
			})
			return nil
		}

		// Publish structured findings — Status=ok confirms to the coordinator
		// this is an honest success and evidence can be passed to synthesis.
		resultMsg := researchResultMessage{
			TaskID:               task.TaskID,
			WorkID:               task.WorkID,
			Status:               ResearchStatusOK,
			FindingType:          task.ResearchType,
			Evidence:             evidence,
			Confidence:           0.9,
			AffectedFiles:        sf.AffectedFiles,
			AffectedModules:      sf.AffectedModules,
			AffectedZones:        sf.AffectedZones,
			CrossRepoTouchpoints: sf.CrossRepoTouchpoints,
			RelevantPatterns:     sf.RelevantPatterns,
			RelevantAntiPatterns: sf.RelevantAntiPatterns,
			Criteria:             criteria,
		}
		report(models.AssignmentStatusCompleted, resultMsg)
		logger.Info(msgCtx, "task completed", map[string]interface{}{
			"task_id": task.TaskID,
			"work_id": task.WorkID,
		})
		return nil
	}
}

// commitResearchResult completes the agent's assignment with status and
// enqueues the task's research.results event in one transaction. key and
// payload are exactly what the direct publish sent.
func commitResearchResult(ctx context.Context, db workgraph.TxBeginner, wg *workgraph.WorkGraph, aggregateID, assignmentID uuid.UUID, status models.AssignmentStatus, key string, result researchResultMessage) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := wg.CompleteAssignmentTx(ctx, tx, assignmentID, status); err != nil {
			return err
		}
		return outbox.EnqueueJSON(ctx, tx, aggregateID, bus.TopicResearchResults, key, result)
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

// truncate cuts a string to n runes, appending "…" if cut. Used to keep
// tool-call result previews sane in the dashboard WebSocket stream —
// full results can be thousands of characters and the UI only shows a
// snippet per event.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// loopFailureResult is the failed research.results message for a tool-loop
// error. Classify: a LoopStuck (R17: structured, the task fails; the
// coordinator's honest-failure gate records it, research-agent does not
// block the item); ErrSafetyLimit (typed) or substring match for older
// error shapes; context timeout; everything else is "error".
func loopFailureResult(task researchTaskMessage, err error) researchResultMessage {
	status := ResearchStatusError
	var stuck *tools.LoopStuck
	var none *tools.NoEvidence
	note := ""
	switch {
	case errors.As(err, &none):
		status = ResearchStatusNoEvidence
		note = none.Note
	case errors.As(err, &stuck):
		status = ResearchStatusLoopStuck
	case errors.Is(err, tools.ErrSafetyLimit) || strings.Contains(err.Error(), "safety limit"):
		status = ResearchStatusLimitHit
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") || strings.Contains(strings.ToLower(err.Error()), "deadline exceeded"):
		status = ResearchStatusTimedOut
	}
	return researchResultMessage{
		TaskID:      task.TaskID,
		WorkID:      task.WorkID,
		Status:      status,
		FindingType: task.ResearchType,
		Evidence:    fmt.Sprintf("Research tool loop failed: %v", err),
		Confidence:  0.0,
		ErrorDetail: err.Error(),
		LoopStuck:   stuck,
		Note:        note,
	}
}

// noEvidenceResult (R18 E1) is the no_evidence result for a loop that
// ended with an answer although no tool call it made returned a result
// (none ran, or every one failed). failed is false when at least one call
// succeeded.
func noEvidenceResult(task researchTaskMessage, answer string, calls []toolCallRecord) (researchResultMessage, bool) {
	ok := 0
	for _, c := range calls {
		if !c.isError {
			ok++
		}
	}
	if ok > 0 {
		return researchResultMessage{}, false
	}
	detail := fmt.Sprintf("no evidence: the answer rests on no successful tool call (%d call(s), all failed)", len(calls))
	if len(calls) == 0 {
		detail = "no evidence: the answer was given without running a tool call"
	}
	return researchResultMessage{
		TaskID:      task.TaskID,
		WorkID:      task.WorkID,
		Status:      ResearchStatusNoEvidence,
		FindingType: task.ResearchType,
		Evidence:    "Research produced no evidence: " + detail,
		Confidence:  0.0,
		ErrorDetail: detail,
		Note:        truncate(answer, 4000),
	}, true
}
