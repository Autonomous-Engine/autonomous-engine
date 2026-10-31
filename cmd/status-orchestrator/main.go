package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/alerts"
	"github.com/autonomousengine/autonomous-engine/internal/auth"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/codexgap"
	"github.com/autonomousengine/autonomous-engine/internal/codexindex"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	agentID       = "status-orchestrator"
	healthPort    = 8090
	consumerGroup = "status-orchestrator"
	// idempotencyConsumer is this service's name in processed_events.
	idempotencyConsumer = "status-orchestrator"
)

// stateChangedEvent is published to "state.changed" after every transition.
type stateChangedEvent struct {
	WorkID    string `json:"work_id"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Reason    string `json:"reason"`
	Timestamp string `json:"timestamp"`
}

// reviewCompletePayload is the payload for the review.complete topic.
type reviewCompletePayload struct {
	WorkID              string `json:"work_id"`
	Decision            string `json:"decision"`
	Feedback            string `json:"feedback,omitempty"`
	HumanReviewRequired bool   `json:"human_review_required"`
}

// prMergedPayload is the payload for the pr.merged topic.
type prMergedPayload struct {
	WorkID             string `json:"work_id"`
	RepositoryID       string `json:"repository_id,omitempty"`
	Title              string `json:"title"`
	AcceptanceCriteria string `json:"acceptance_criteria"`

	// Merge identity (webhook-listener, M4-05). Absent from older
	// producers; the outcome then records no merge.
	PRNumber       int    `json:"pr_number,omitempty"`
	Repository     string `json:"repository,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	MergedAt       string `json:"merged_at,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
	// RevertsPRNumbers is the revert heuristic's result for this PR
	// (outcomes.RevertedPRNumbers): the PRs of the same repository it
	// reverts. A revert PR on a non-agent branch arrives with no work_id.
	RevertsPRNumbers []int `json:"reverts_pr_numbers,omitempty"`
}

// qaResultPayload is the payload for the qa.result topic.
type qaResultPayload struct {
	WorkID string `json:"work_id"`
	Result string `json:"result"`
}

// qaReadyMessage is published to qa.ready when work enters QA.
type qaReadyMessage struct {
	WorkID             string `json:"work_id"`
	Title              string `json:"title"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Branch             string `json:"branch,omitempty"`

	// HeadSHA is the verified head SHA the review examined (D1.5), taken
	// from the review artifact alongside Branch. Additive; empty when the
	// review ran on a legacy pr.ready without evidence, in which case the
	// qa-agent applies its EVIDENCE_COMPAT_WINDOW rules.
	HeadSHA string `json:"head_sha,omitempty"`
	// BaseSHA is the work item's pinned base commit (M2-02): the
	// repository's pin, or the single common pin; empty when the item is
	// legacy-unpinned or its repositories have different bases. Additive.
	BaseSHA string `json:"base_sha,omitempty"`
}

// qaBugsPayload is the payload for the qa.bugs topic.
type qaBugsPayload struct {
	WorkID            string `json:"work_id"`
	Bugs              string `json:"bugs"`
	ReproductionSteps string `json:"reproduction_steps"`
}

// workBlockedPayload is the payload for the work.blocked topic.
type workBlockedPayload struct {
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
	// Kind is guards.WorkBlockedPayload.Kind (codex_ungrounded adds the
	// Re-index action to the escalation).
	Kind string `json:"kind,omitempty"`
}

// implementationCompletePayload is the payload for the implementation.complete topic.
type implementationCompletePayload struct {
	TaskID  string `json:"task_id"` // the subtask's work_item ID
	WorkID  string `json:"work_id"` // the parent work_item ID
	Success bool   `json:"success"` // whether the implementation succeeded
	Reason  string `json:"reason"`  // failure reason if success=false

	// Evidence is the additive v1 evidence.ImplementationEvidence object
	// (M0-07). It is kept raw so that a malformed value blocks with a reason
	// instead of failing the whole message decode; decideSubtaskTransition
	// judges it through evidence.ReleaseDecisionFromJSON. Absent (or JSON
	// null) means a pre-evidence producer: the EVIDENCE_COMPAT_WINDOW rules
	// apply and Success is the fallback.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// humanReviewMessage is published to human-review.queue when circuit breaker trips.
type humanReviewMessage struct {
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
}

// prdReadyMessage is published to prd.ready to re-trigger processing after bug loop.
// Shape must be the canonical prd.ready payload accepted by research-coordinator:
// {work_id, product_id, work_type, prd_content, retry?, timestamp}. When republishing
// from a QA-retry path we re-read the latest PRD artifact so downstream research
// runs with full context rather than an empty brief.
type prdReadyMessage struct {
	WorkID     string `json:"work_id"`
	ProductID  string `json:"product_id,omitempty"`
	WorkType   string `json:"work_type,omitempty"`
	PRDContent string `json:"prd_content"`
	Reason     string `json:"reason,omitempty"`
	Retry      bool   `json:"retry,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("status-orchestrator: load config: %v", err)
	}

	// Connect to PostgreSQL.
	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("status-orchestrator: connect db: %v", err)
	}
	defer pool.Close()

	// Run DB migrations.
	execPath, err := os.Executable()
	if err != nil {
		log.Printf("status-orchestrator: get executable path: %v — using relative migrations path", err)
		execPath = "."
	}
	migrationsDir := filepath.Join(filepath.Dir(execPath), "migrations")
	if err := db.Migrate(ctx, pool, migrationsDir); err != nil {
		log.Printf("status-orchestrator: db migrate from %s: %v — retrying with cwd", migrationsDir, err)
		migrationsDir = "migrations"
		if err := db.Migrate(ctx, pool, migrationsDir); err != nil {
			log.Printf("status-orchestrator: db migrate: %v — continuing without migration", err)
		}
	}

	if err := auth.Bootstrap(ctx, pool); err != nil {
		log.Printf("auth bootstrap warning: %v", err)
	}

	// Seed default prompt templates (no-op if already seeded).
	promptStore := prompts.NewStore(pool)
	if err := promptStore.SeedDefaults(ctx); err != nil {
		log.Printf("prompt seed: %v", err)
	}

	// Create agent registry and seed default definitions.
	registry := agentregistry.New(pool)
	if err := registry.SeedDefaults(ctx); err != nil {
		log.Printf("agent registry seed: %v", err)
	}

	// Connect to Kafka.
	// (AlertService is started after Kafka is connected, further below.)
	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("status-orchestrator: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	// Start alert evaluation service.
	alertSvc := alerts.New(pool, kb, 0 /* default 60s interval */)
	alertSvc.Start(ctx)

	// Create work graph.
	wg := workgraph.New(pool)

	maxRetries := cfg.System.MaxRetryCount
	humanReviewRequired := cfg.System.HumanReviewRequired

	// Evidence gate inputs (M0-07): the product store supplies the D2
	// partial-verification opt-in; the compatibility window governs
	// implementation.complete messages that carry no evidence.
	productStore := products.NewStore(pool)
	window := evidence.CompatWindowFromEnv()
	log.Printf("status-orchestrator: %s=%s", evidence.CompatWindowEnv, window)

	// Set up health checker.
	checker := health.New("status-orchestrator")
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04a). Every event that announces a state change
	// this service makes is committed to outbox_events with the change; this
	// loop is what carries them to Kafka. If it does not start, transitions
	// would commit and their qa.ready / implementation.tasks / prompt events
	// would never leave the table, so a failure here is fatal. Started before
	// the consumers so nothing is enqueued without a dispatcher running.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "status-orchestrator")
	if err != nil {
		log.Fatalf("status-orchestrator: start outbox dispatcher: %v", err)
	}
	log.Printf("status-orchestrator: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M4-06: nightly false-success job over work_item_outcomes (verified,
	// then reverted within 14 days).
	go runFalseSuccessJob(ctx, pool, falseSuccessInterval)

	// Subscribe to all relevant topics.
	topics := []string{
		bus.TopicReviewComplete,
		bus.TopicPRMerged,
		bus.TopicQAResult,
		bus.TopicQABugs,
		bus.TopicWorkBlocked,
		bus.TopicImplementationComplete,
		// R14: the parent leaves verification on the stage's decisions.
		bus.TopicVerificationComplete,
		// User interaction surface: drive work items in/out of awaiting_user.
		bus.TopicUserPromptRequested,
		bus.TopicUserPromptAnswered,
		// M4-06: terminate replay items with nothing to review.
		bus.TopicReplayComplete,
		// R17: a dispatched task walks its parent planning → implementation.
		bus.TopicImplementationTasks,
	}

	// M1-05: every handler is guarded by the processed-event ledger. The
	// transaction helpers in outbox_publish.go carry the guard row (Wrap);
	ledger := idempotency.NewLedger(pool)
	handler := ledger.Handler(idempotencyConsumer, buildHandler(wg, kb, registry, productStore, window, maxRetries, humanReviewRequired))
	if err := kb.SubscribeMultiple(topics, consumerGroup, handler); err != nil {
		log.Fatalf("status-orchestrator: subscribe: %v", err)
	}

	// Codex coherence Part 3 (codex_grounding.go): codex.indexed re-runs
	// items refused on an empty codex once Re-index grounded it; codex.gap
	// counts empty codex lookups per run. Own consumer group and ledger
	// consumer.
	gapThreshold := codexgap.ThresholdFromEnv()
	log.Printf("status-orchestrator: %s=%d", codexgap.ThresholdEnv, gapThreshold)
	codexHandler := ledger.Handler(codexIdempotencyConsumer, buildCodexHandler(wg, kb, gapThreshold))
	if err := kb.SubscribeMultiple(codexTopics, codexConsumerGroup, codexHandler); err != nil {
		log.Fatalf("status-orchestrator: subscribe codex topics: %v", err)
	}
	go runReindexMaintenance(ctx, pool, codexindex.URLFromEnv(), reindexTimeoutFromEnv(), 30*time.Second, 5*time.Minute)
	// Codex coherence Part 1: codex.indexed schedules a durable, debounced
	// re-plan sweep of its product and branch (own consumer group); the
	// sweep loop re-plans, notes or escalates the pinned in-flight items.
	replanCfg := replanConfigFromEnv()
	replanner := newCodexReplanner(pool, wg, kb, registry, replanCfg)
	if err := kb.Subscribe(bus.TopicCodexIndexed, codexReplanConsumerGroup, ledger.Handler(codexReplanConsumer, replanner.handleCodexIndexed)); err != nil {
		log.Fatalf("status-orchestrator: subscribe %s: %v", bus.TopicCodexIndexed, err)
	}
	log.Printf("status-orchestrator: codex re-plan debounce=%s max=%d min_plan_tokens=%d of %d tick=%s",
		replanCfg.Debounce, replanCfg.Max, replanCfg.MinPlanTokens, replanCfg.TokenBudget, replanCfg.Tick)
	go replanner.run(ctx)

	// Start health check HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())

	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{
		Addr:    healthAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("status-orchestrator: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("status-orchestrator: health server error: %v", err)
		}
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("status-orchestrator: shutting down...")
	cancel()

	alertSvc.Stop()

	// Let the dispatch loop finish its current iteration so a publish that
	// reached the broker is recorded as published (its bookkeeping runs on a
	// detached context) instead of being delivered again after restart.
	dispatcher.Wait()
	log.Println("status-orchestrator: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("status-orchestrator: health server shutdown error: %v", err)
	}
	log.Println("status-orchestrator: stopped")
}

// buildHandler constructs the Kafka message handler for the status orchestrator.
func buildHandler(
	wg *workgraph.WorkGraph,
	kb bus.MessageBus,
	registry *agentregistry.Registry,
	productStore products.PolicyStore,
	window evidence.CompatWindow,
	maxRetries int,
	humanReviewRequired bool,
) bus.Handler {
	return func(ctx context.Context, msg *bus.Message) error {
		switch msg.Topic {
		case bus.TopicReviewComplete:
			return handleReviewComplete(ctx, wg, kb, registry, msg, humanReviewRequired)
		case bus.TopicPRMerged:
			return handlePRMerged(ctx, wg, kb, registry, msg)
		case bus.TopicQAResult:
			return handleQAResult(ctx, wg, kb, msg)
		case bus.TopicQABugs:
			return handleQABugs(ctx, wg, kb, registry, msg, maxRetries)
		case bus.TopicWorkBlocked:
			return handleWorkBlocked(ctx, wg, kb, msg)
		case bus.TopicImplementationComplete:
			return handleImplementationComplete(ctx, wg, kb, registry, productStore, window, msg)
		case bus.TopicVerificationComplete:
			return handleVerificationComplete(ctx, wg, kb, registry, msg)
		case bus.TopicUserPromptRequested:
			return handleUserPromptRequested(ctx, wg, msg)
		case bus.TopicUserPromptAnswered:
			return handleUserPromptAnswered(ctx, wg, kb, registry, msg)
		case bus.TopicReplayComplete:
			return handleReplayComplete(ctx, wg, kb, msg)
		case bus.TopicImplementationTasks:
			return handleImplementationTasks(ctx, wg, kb, msg)
		default:
			log.Printf("status-orchestrator: unhandled topic %s", msg.Topic)
			return nil
		}
	}
}

// nextState returns the next pipeline state for a work item given its current
// state and the agent definition that governs it. When def is nil, the full
// pipeline is assumed (all steps enabled). Returns models.StatePassed when the
// pipeline is complete.
func nextState(def *agentregistry.AgentDefinition, currentState models.WorkItemState) models.WorkItemState {
	steps := []struct {
		enabled bool
		state   models.WorkItemState
	}{
		{true, models.WorkItemStatePRD},
		{def == nil || def.StepResearch, models.WorkItemStateResearch},
		{def == nil || def.StepPlanning, models.WorkItemStatePlanning},
		{def == nil || def.StepImplementation, models.WorkItemStateImplementation},
		{def == nil || def.StepReview, models.WorkItemStateReview},
		{def == nil || def.StepHumanReview, models.WorkItemStateHumanReview},
		{def == nil || def.StepQA, models.WorkItemStateQA},
	}

	found := false
	for _, s := range steps {
		if s.state == currentState {
			found = true
			continue
		}
		if found && s.enabled {
			return s.state
		}
	}
	return models.WorkItemStatePassed
}

// loadDefForWorkItem looks up the agent definition attached to a work item.
// Returns nil (not an error) when no definition is set.
func loadDefForWorkItem(ctx context.Context, registry *agentregistry.Registry, workID uuid.UUID) *agentregistry.AgentDefinition {
	var defIDStr *string
	err := registry.DB().QueryRow(ctx,
		`SELECT agent_definition_id::text FROM work_items WHERE id=$1`,
		workID,
	).Scan(&defIDStr)
	if err != nil || defIDStr == nil {
		return nil
	}
	defID, err := uuid.Parse(*defIDStr)
	if err != nil {
		return nil
	}
	def, err := registry.Get(ctx, defID)
	if err != nil {
		return nil
	}
	return def
}

// handleReviewComplete processes review.complete messages.
// Decision "approved" + human_review_required → human_review (or next step if disabled).
// Decision "approved" + auto-merge allowed → qa (or next step after review) + publish qa.ready.
// Decision "changes_requested" → implementation.
func handleReviewComplete(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, msg *bus.Message, humanReviewRequired bool) error {
	var payload reviewCompletePayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("status-orchestrator: review.complete: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: review.complete: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}

	// M4-06: a replay item ends on its review: record the outcome and
	// cancel it; it never reaches human review, QA or a fix cycle.
	if terminal, err := isShadowTerminal(ctx, wg, workID); err != nil {
		return err
	} else if terminal {
		return cancelReplay(ctx, wg, kb, workID, "reviewed: "+payload.Decision)
	}

	def := loadDefForWorkItem(ctx, registry, workID)

	switch payload.Decision {
	case "approved":
		// D2/D3: a parent whose subtasks completed on partially verified
		// evidence must pass mandatory human review before QA, regardless
		// of its current state, the agent definition, or either
		// human_review_required flag (resolveReviewApprovedTarget applies
		// the deny-by-target rule). Fails closed when the gate cannot be
		// read.
		target, err := resolveReviewApprovedTarget(ctx, wg, workID, def, payload.HumanReviewRequired, humanReviewRequired)
		if err != nil {
			log.Printf("status-orchestrator: review.complete: resolve target for %s: %v", workID, err)
			return nil
		}
		if target.Gated {
			log.Printf("status-orchestrator: review.complete: work_id=%s gated → %s (hold=%t): %s", workID, target.ToState, target.Hold, target.GateReason)
		}
		if target.Hold {
			return nil
		}
		// The transition and every side effect of this target commit
		// together (M1-04a). The outcome picks up the reviewed head, any
		// structured review defects and the human-review gate (M4-05).
		effects := []txEffect{outcomeEffect(outcomes.Facts{})}
		var hrPrompt txEffect
		if target.PublishHumanReviewPrompt {
			prompt := "Review approved — awaiting human confirmation before QA."
			if target.Gated {
				prompt = "Review approved — " + target.GateReason
			}
			hrPrompt = humanReviewPromptEffect(prompt)
			effects = append(effects, hrPrompt)
		}
		if target.PublishBlockedPrompt {
			effects = append(effects, blockedPromptEffect(target.Reason))
		}
		// qa.ready only if the next state is QA.
		if target.PublishQAReady {
			branch, headSHA := lookupPRBranch(ctx, wg, workID)
			baseSHA := qaBaseSHA(ctx, wg.DB(), workID, "")
			effects = append(effects, func(ctx context.Context, tx pgx.Tx, item *models.WorkItem) error {
				qaMsg := qaReadyMessage{
					WorkID:             workID.String(),
					Title:              item.Title,
					AcceptanceCriteria: item.Description,
					Branch:             branch,
					HeadSHA:            headSHA,
					BaseSHA:            baseSHA,
				}
				return enqueueEffect(bus.TopicQAReady, workID.String(), qaMsg)(ctx, tx, item)
			})
		}
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, target.ToState, target.Reason, effects...); err != nil {
			log.Printf("status-orchestrator: review.complete: transition to %s: %v", target.ToState, err)
			// Legacy behaviour kept: an ungated human-review prompt is
			// raised even when the transition failed (now in its own
			// transaction, since the combined one rolled back).
			if !target.Gated && hrPrompt != nil {
				if perr := raisePromptAlone(ctx, wg.DB(), wg, workID, hrPrompt); perr != nil {
					log.Printf("status-orchestrator: review.complete: raise human review prompt: %v", perr)
				}
			}
			return nil
		}

	case "changes_requested":
		requestChanges(ctx, wg, kb, workID, payload.Feedback, changesFromReview, nil)

	default:
		log.Printf("status-orchestrator: review.complete: unknown decision %q for work_id %s", payload.Decision, workID)
	}

	return nil
}

// handlePRMerged processes pr.merged messages.
// Transitions to the next step after review (QA or skipped) and publishes qa.ready if QA is active.
// Also triggers a post-merge Codex update so the knowledge base reflects the merged changes.
func handlePRMerged(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, msg *bus.Message) error {
	var payload prMergedPayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("status-orchestrator: pr.merged: unmarshal: %v", err)
		return nil
	}

	// M4-05 revert heuristic: a merged PR that reverts earlier PRs marks
	// their outcomes reverted. It runs before the work_id check because a
	// revert is usually a human PR with no work item of its own.
	markReverted(ctx, wg.DB(), payload)

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		if payload.WorkID == "" && len(payload.RevertsPRNumbers) > 0 {
			return nil // a revert PR on a non-agent branch: nothing else to do
		}
		log.Printf("status-orchestrator: pr.merged: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}
	merge := outcomeEffect(outcomes.Facts{Merge: mergeFacts(payload)})

	def := loadDefForWorkItem(ctx, registry, workID)

	// D2/D3: a merged PR does not lift the mandatory human review for a
	// parent whose subtasks completed on partially verified evidence.
	// resolvePRMergedTarget applies the deny-by-target rule: a gated parent
	// only ever goes to human_review (from review, awaiting_user, or any
	// other state) or is held there; it is never handed qa. Ungated parents
	// keep the existing behaviour.
	target, err := resolvePRMergedTarget(ctx, wg, workID, def)
	if err != nil {
		log.Printf("status-orchestrator: pr.merged: resolve target for %s: %v", workID, err)
		return nil
	}
	if target.Gated {
		log.Printf("status-orchestrator: pr.merged: work_id=%s gated → %s (hold=%t): %s", workID, target.ToState, target.Hold, target.GateReason)
	}

	if !target.Hold {
		// The transition and every side effect of this target commit
		// together (M1-04a), including the merge on the outcome (M4-05).
		effects := []txEffect{merge}
		if target.PublishHumanReviewPrompt {
			effects = append(effects, humanReviewPromptEffect("PR merged — "+target.GateReason))
		}
		if target.PublishBlockedPrompt {
			effects = append(effects, blockedPromptEffect(target.Reason))
		}
		if target.PublishQAReady {
			branch, headSHA := lookupPRBranch(ctx, wg, workID)
			qaMsg := qaReadyMessage{
				WorkID:             workID.String(),
				Title:              payload.Title,
				AcceptanceCriteria: payload.AcceptanceCriteria,
				Branch:             branch,
				HeadSHA:            headSHA,
				BaseSHA:            qaBaseSHA(ctx, wg.DB(), workID, payload.RepositoryID),
			}
			effects = append(effects, enqueueEffect(bus.TopicQAReady, workID.String(), qaMsg))
		}
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, target.ToState, target.Reason, effects...); err != nil {
			log.Printf("status-orchestrator: pr.merged: transition to %s: %v", target.ToState, err)
			// The merge happened whatever the state machine says: keep
			// it on the outcome (its own transaction; the combined one
			// rolled back).
			recordOutcomeAlone(ctx, wg.DB(), wg, workID, merge)
			return nil
		}
	} else {
		// Held: no transition, but the merge is still a production fact.
		// Unguarded so codex.updated below keeps the delivery's guard.
		recordHeldMerge(ctx, wg.DB(), wg, workID, merge)
	}

	// Pull the latest repo state via github-service before updating the Codex.
	// This ensures the knowledge indexer sees the merged code, not the pre-merge snapshot.
	repositoryID := payload.RepositoryID
	githubServiceURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubServiceURL == "" {
		githubServiceURL = "http://github-service:8083"
	}
	if repositoryID != "" {
		pullPayload, _ := json.Marshal(map[string]string{"repository_id": repositoryID})
		pullResp, pullErr := http.Post(githubServiceURL+"/pull", "application/json", bytes.NewReader(pullPayload))
		if pullErr != nil {
			log.Printf("status-orchestrator: pr.merged: git pull via github-service: %v", pullErr)
		} else {
			pullResp.Body.Close()
			log.Printf("status-orchestrator: pr.merged: triggered git pull for repository_id=%s", repositoryID)
		}
	}

	// Trigger incremental Codex update — the codebase just changed.
	// It accompanies no write of this service, so it is enqueued in a
	// transaction of its own (outbox checklist step 1).
	codexUpdatePayload := map[string]string{
		"trigger":       "post_merge",
		"repository_id": repositoryID,
		"since":         "HEAD~1",
	}
	if err := enqueueAlone(ctx, wg.DB(), workID, bus.TopicCodexUpdated, workID.String(), codexUpdatePayload); err != nil {
		log.Printf("status-orchestrator: pr.merged: publish codex.updated: %v", err)
	} else {
		log.Printf("status-orchestrator: triggered post-merge Codex update for work_id=%s", workID)
	}

	return nil
}

// handleQAResult processes qa.result messages.
// Result "pass" → passed; "fail" → failed.
func handleQAResult(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, msg *bus.Message) error {
	var payload qaResultPayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("status-orchestrator: qa.result: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: qa.result: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}

	switch payload.Result {
	case "pass":
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStatePassed, "QA passed",
			outcomeEffect(outcomes.Facts{QAResult: outcomes.QAPass})); err != nil {
			log.Printf("status-orchestrator: qa.result: transition to passed: %v", err)
		}
	case "fail":
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateFailed, "QA failed",
			outcomeEffect(outcomes.Facts{QAResult: outcomes.QAFail})); err != nil {
			log.Printf("status-orchestrator: qa.result: transition to failed: %v", err)
		}
	default:
		log.Printf("status-orchestrator: qa.result: unknown result %q for work_id %s", payload.Result, workID)
	}

	return nil
}

// handleQABugs processes qa.bugs messages.
// Counts previous failures. If >= maxRetries → blocked + human-review.queue (circuit breaker).
// Otherwise → first enabled step after PRD + prd.ready for re-processing.
func handleQABugs(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, msg *bus.Message, maxRetries int) error {
	var payload qaBugsPayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("status-orchestrator: qa.bugs: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: qa.bugs: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}

	// Count previous failures from transition history.
	transitions, err := wg.GetTransitions(ctx, workID)
	if err != nil {
		log.Printf("status-orchestrator: qa.bugs: get transitions: %v", err)
		return nil
	}

	failureCount := 0
	for _, t := range transitions {
		if t.ToState == models.WorkItemStateFailed {
			failureCount++
		}
	}

	reason := fmt.Sprintf("QA bugs reported: %s", payload.Bugs)

	if failureCount >= maxRetries {
		// Circuit breaker: too many retries — escalate to human review.
		blockReason := reason + " (max retries exceeded)"
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateBlocked, blockReason, blockedPromptEffect(blockReason)); err != nil {
			log.Printf("status-orchestrator: qa.bugs: transition to blocked: %v", err)
			return nil
		}
	} else {
		// Retry: transition qa → failed → implementation with a targeted fix subtask.
		// Previous behavior restarted from research, burning ~$12 per retry.
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateFailed, reason); err != nil {
			log.Printf("status-orchestrator: qa.bugs: transition to failed: %v", err)
			return nil
		}
		if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateImplementation, reason+" (retrying from implementation)"); err != nil {
			log.Printf("status-orchestrator: qa.bugs: transition to implementation: %v", err)
			return nil
		}

		item, _ := wg.GetWorkItem(ctx, workID)
		repoID := ""
		var repoIDPtr *uuid.UUID
		var productIDPtr *uuid.UUID
		if item != nil {
			if item.RepositoryID != nil {
				repoID = item.RepositoryID.String()
				repoIDPtr = item.RepositoryID
			}
			productIDPtr = item.ProductID
		}
		if repoIDPtr == nil {
			if siblings, sErr := wg.GetChildren(ctx, workID); sErr == nil {
				for _, sib := range siblings {
					if sib.RepositoryID != nil {
						repoIDPtr = sib.RepositoryID
						repoID = sib.RepositoryID.String()
						break
					}
				}
			}
		}

		bugDescription := payload.Bugs
		if strings.TrimSpace(payload.ReproductionSteps) != "" {
			bugDescription += "\n\n## Reproduction steps\n" + payload.ReproductionSteps
		}

		fixTaskID := uuid.New()
		fixItem := &models.WorkItem{
			ID:           fixTaskID,
			Type:         models.WorkItemTypeSubtask,
			State:        models.WorkItemStateImplementation,
			Title:        "Fix QA-reported bugs",
			Description:  bugDescription,
			Priority:     models.PriorityHigh,
			ParentID:     &workID,
			RepositoryID: repoIDPtr,
			ProductID:    productIDPtr,
		}
		fixMsg := map[string]interface{}{
			"task_id":       fixTaskID.String(),
			"work_id":       workID.String(),
			"repository_id": repoID,
			"title":         "Fix QA-reported bugs",
			"description":   bugDescription,
			"qa_bugs":       payload.Bugs,
			"is_qa_fix":     true,
		}
		dispatchFixOrBlock(ctx, wg.DB(), wg, kb, "qa.bugs", workID, fixItem, fixMsg)
	}

	return nil
}

// handleWorkBlocked processes work.blocked messages.
// Transitions to blocked with the reason from the payload.
func handleWorkBlocked(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, msg *bus.Message) error {
	var payload workBlockedPayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("status-orchestrator: work.blocked: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		log.Printf("status-orchestrator: work.blocked: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}

	reason := payload.Reason
	if reason == "" {
		reason = "blocked by external event"
	}

	// An item that is already blocked (converted producers block with
	// guards.BlockWorkItemTx before emitting this event) is not transitioned
	// again, and the escalation prompt is raised only when none is pending
	// for the item; see blockFromWorkBlocked.
	if err := blockFromWorkBlockedKind(ctx, wg.DB(), wg, kb, workID, reason, payload.Kind); err != nil {
		log.Printf("status-orchestrator: work.blocked: block %s: %v", workID, err)
	}

	return nil
}

// handleImplementationComplete processes implementation.complete messages.
//
// The subtask's transition is decided from the message's evidence record via
// evidence.ReleaseDecision (decideSubtaskTransition): verified → passed;
// partially_verified with the product opted in → passed with
// partialVerificationMarker, which forces the parent through mandatory human
// review before QA (parentHumanReviewGate in handleReviewComplete and
// handlePRMerged); everything else → blocked with the decision's reason.
// Messages without evidence follow EVIDENCE_COMPAT_WINDOW.
//
// Then checks if all children tasks are complete; if so the parent moves to
// verification (R14) and on to the next step after implementation (review
// or skipped step) only once every passed subtask is verified
// (settleVerification).
func handleImplementationComplete(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, productStore products.PolicyStore, window evidence.CompatWindow, msg *bus.Message) error {
	var payload implementationCompletePayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("status-orchestrator: implementation.complete: unmarshal: %v", err)
		return nil
	}

	// TaskID is the subtask that completed. WorkID is the parent.
	// Use TaskID when available (subtask flow), fall back to WorkID (direct flow).
	itemIDStr := payload.TaskID
	if itemIDStr == "" {
		itemIDStr = payload.WorkID
	}
	workID, err := uuid.Parse(itemIDStr)
	if err != nil {
		log.Printf("status-orchestrator: implementation.complete: parse id %q: %v", itemIDStr, err)
		return nil
	}

	// Get the work item (subtask or direct item).
	item, err := wg.GetWorkItem(ctx, workID)
	if err != nil {
		log.Printf("status-orchestrator: implementation.complete: get work item: %v", err)
		return nil
	}

	log.Printf("status-orchestrator: implementation.complete: item=%s type=%s state=%s parent=%v", workID, item.Type, item.State, item.ParentID)

	// Codex coherence Part 1 review: a completion from a subtask outside
	// the parent's current round (superseded by a codex re-plan, or created
	// before it) belongs to a plan that no longer exists: it neither walks
	// the parent nor settles it.
	if item.ParentID != nil {
		if out, why := outsideCodexRound(ctx, wg, item); out {
			log.Printf("status-orchestrator: implementation.complete: subtask %s ignored: %s", workID, why)
			return nil
		}
	}

	// R17 L4: a parent still in planning (its implementation.tasks walk was
	// missed) is recorded in implementation before its subtask's outcome.
	// Unguarded: the guard belongs to the transitions below.
	if item.ParentID != nil {
		if err := walkParentToImplementation(idempotency.Unguarded(ctx), wg.DB(), wg, kb, *item.ParentID); err != nil {
			log.Printf("status-orchestrator: implementation.complete: walk parent %s to implementation: %v", *item.ParentID, err)
		}
	}

	// Transition this subtask from the evidence record (D1–D3).
	if item.State == models.WorkItemStateImplementation {
		if !payload.hasEvidence() {
			if window == evidence.CompatWindowStrict {
				log.Printf("status-orchestrator: implementation.complete: work_item=%s task=%s carries no evidence — blocking (%s=strict)", payload.WorkID, workID, evidence.CompatWindowEnv)
			} else {
				metrics.EvidenceCompatLegacyFallback.Inc()
				log.Printf("status-orchestrator: implementation.complete: WARNING work_item=%s task=%s carries no evidence — falling back to legacy success=%t (%s=legacy)", payload.WorkID, workID, payload.Success, evidence.CompatWindowEnv)
			}
		}
		policy, perr := products.LoadReleasePolicyForItem(ctx, productStore, wg, item)
		if perr != nil {
			log.Printf("status-orchestrator: implementation.complete: %v — partial verification policy defaults to not opted in", perr)
		}
		toState, reason := decideSubtaskTransitionFor(payload, policy, window, repositoryIDString(item.RepositoryID))
		if toState == models.WorkItemStateHumanReview && item.ParentID == nil {
			// The human gate is realised on the parent; an item with no
			// parent has nothing to gate, and the state machine offers no
			// implementation→human_review edge. Never let partially
			// verified work pass silently: block with the reason.
			toState = models.WorkItemStateBlocked
			reason = noParentHumanReviewReason(payload, reason)
		}
		// M4-05: the subtask's release decision goes on its top-level
		// item's outcome in the transaction of the subtask's transition.
		decision := outcomeEffect(outcomes.Facts{TaskID: workID.String(), Decision: subtaskDecision(toState, payload.hasEvidence())})
		switch toState {
		case models.WorkItemStateBlocked:
			log.Printf("status-orchestrator: implementation.complete: subtask %s BLOCKED: %s", workID, reason)
			prompt := blockedPromptEffect(reason)
			if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, models.WorkItemStateBlocked, reason, prompt, decision); err != nil {
				log.Printf("status-orchestrator: implementation.complete: transition subtask to blocked: %v", err)
				// Legacy behaviour kept: the blocked prompt is raised even
				// when the transition failed.
				if perr := raisePromptAlone(ctx, wg.DB(), wg, workID, prompt); perr != nil {
					log.Printf("status-orchestrator: implementation.complete: raise blocked prompt: %v", perr)
				}
			}
			return nil // don't check siblings — this one is blocked
		case models.WorkItemStateHumanReview:
			// Partially verified, product opted in (D2). The subtask's own
			// implementation is done, but the parent may only advance to
			// mandatory human review, never straight to QA: the marker on
			// this transition is what parentHumanReviewGate reads.
			// Verified-but-security-flagged work (D3) gets its own marker.
			reason = humanReviewMarkerFor(payload) + ": " + reason
			log.Printf("status-orchestrator: implementation.complete: subtask %s %s", workID, reason)
			// M1-05: unguarded (see the default case).
			if err := transitionAndPublish(idempotency.Unguarded(ctx), wg.DB(), wg, kb, workID, models.WorkItemStatePassed, reason, decision); err != nil {
				log.Printf("status-orchestrator: implementation.complete: transition subtask to passed: %v", err)
				return nil // without the marker recorded the gate would be lost; keep the subtask where it is
			}
		default:
			log.Printf("status-orchestrator: implementation.complete: subtask %s passed: %s", workID, reason)
			// M1-05: the subtask's implementation→passed transition is
			// idempotent on its own (a redelivery finds the subtask no
			// longer in implementation and skips it), so it does not
			// consume the guard: the guard goes into the parent's
			// transition below, which has no other trigger. With the guard
			// here, a crash between the two transactions would make the
			// redelivery a duplicate and strand the parent. When no parent
			// transition follows, Ledger.Handler records the event after
			// the handler returns.
			if err := transitionAndPublish(idempotency.Unguarded(ctx), wg.DB(), wg, kb, workID, models.WorkItemStatePassed, reason, decision); err != nil {
				log.Printf("status-orchestrator: implementation.complete: transition subtask to passed: %v", err)
			}
		}
	}

	// R14: when every sibling is implemented the parent moves to
	// verification (walking planning → implementation first if needed), and
	// leaves it once each subtask that passed has a verification result —
	// possibly right away, when the verification stage was faster than this
	// event (settleVerification). The parent is never handed review before
	// its subtasks were verified.
	if item.ParentID != nil {
		return settleVerification(ctx, wg, kb, registry, *item.ParentID)
	}

	// No parent — transition the item itself to the next step after implementation.
	def := loadDefForWorkItem(ctx, registry, workID)
	toState := nextState(def, models.WorkItemStateImplementation)
	if err := transitionAndPublish(ctx, wg.DB(), wg, kb, workID, toState, "implementation complete", outcomeEffect(outcomes.Facts{})); err != nil {
		log.Printf("status-orchestrator: implementation.complete: transition to %s: %v", toState, err)
	}

	return nil
}

// lookupPRBranch finds the PR branch and the verified head SHA the review
// examined for a work item from its most recent review artifact (D1.5).
func lookupPRBranch(ctx context.Context, wg *workgraph.WorkGraph, workID uuid.UUID) (branch, headSHA string) {
	artifacts, err := wg.GetArtifacts(ctx, workID, models.ArtifactTypeReviewComments)
	if err != nil || len(artifacts) == 0 {
		return "", ""
	}
	// Parse the most recent review artifact for the branch and the verified
	// head SHA the review examined (D1.5; empty for a legacy review).
	var parsed struct {
		Branch  string `json:"branch"`
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(artifacts[len(artifacts)-1].Content, &parsed); err != nil {
		return "", ""
	}
	return parsed.Branch, parsed.HeadSHA
}

// handleUserPromptRequested transitions the work item to awaiting_user and
// pins the prompt id. Agents yielding for user input publish this topic via
// the request_user_input tool (Task 17).
//
// R17 L4: the move is recorded as a transition (parkOnPrompt), from the
// state read under the row lock, in a transaction that carries the
// delivery's guard row; an item already awaiting_user is left alone.
func handleUserPromptRequested(ctx context.Context, wg *workgraph.WorkGraph, msg *bus.Message) error {
	var p struct {
		ID         uuid.UUID `json:"id"`
		WorkItemID uuid.UUID `json:"work_item_id"`
	}
	if err := json.Unmarshal(msg.Value, &p); err != nil {
		log.Printf("status-orchestrator: user_prompt.requested bad payload: %v", err)
		return nil // unparseable — don't redeliver
	}
	var err error
	var from models.WorkItemState
	parked := false
	for _, delay := range []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond, 1 * time.Second, 2 * time.Second, 5 * time.Second} {
		if delay > 0 {
			time.Sleep(delay)
		}
		from, parked, err = parkOnPrompt(ctx, wg.DB(), p.WorkItemID, p.ID)
		if err == nil || idempotency.IsDuplicate(err) {
			break
		}
		if !strings.Contains(err.Error(), "violates foreign key") {
			break
		}
		log.Printf("status-orchestrator: user_prompt.requested FK retry (delay=%v): %v", delay, err)
	}
	if err != nil && !idempotency.IsDuplicate(err) {
		return fmt.Errorf("park work_item=%s on prompt %s: %w", p.WorkItemID, p.ID, err)
	}
	if parked {
		log.Printf("status-orchestrator: work_item=%s %s → awaiting_user (prompt=%s)", p.WorkItemID, from, p.ID)
	}
	return nil
}

// handleUserPromptAnswered transitions the work item back to previous_state
// after the user answers. The asking agent service is responsible for
// reading the answered payload and resuming its tool loop (Task 20).
func handleUserPromptAnswered(ctx context.Context, wg *workgraph.WorkGraph, kb bus.MessageBus, registry *agentregistry.Registry, msg *bus.Message) error {
	// R17 L4: the restore is recorded as a transition (restoreFromPrompt),
	// in a transaction that carries the delivery's guard row.
	var a struct {
		PromptID     uuid.UUID `json:"prompt_id"`
		WorkItemID   uuid.UUID `json:"work_item_id"`
		ActionChosen string    `json:"action_chosen"`
	}
	if err := json.Unmarshal(msg.Value, &a); err != nil {
		log.Printf("status-orchestrator: user_prompt.answered bad payload: %v", err)
		return nil
	}
	// Codex coherence Part 1: the codex re-plan escalation's answer is
	// acted on here, with the restore, in one guarded transaction.
	if handled, err := handleCodexPromptAnswer(ctx, wg, kb, registry, a.PromptID, a.WorkItemID, a.ActionChosen); handled {
		return err
	}
	to, restored, err := restoreFromPrompt(ctx, wg.DB(), a.WorkItemID, a.PromptID)
	if err != nil && !idempotency.IsDuplicate(err) {
		return fmt.Errorf("restore work_item=%s from prompt %s: %w", a.WorkItemID, a.PromptID, err)
	}
	if restored {
		log.Printf("status-orchestrator: work_item=%s awaiting_user → %s (prompt=%s)", a.WorkItemID, to, a.PromptID)
	}
	// R14: a parent that comes back to verification may have missed its
	// settle while the prompt was pending; settle it now (a no-op while
	// verdicts are still outstanding).
	if item, err := wg.GetWorkItem(ctx, a.WorkItemID); err == nil && item != nil && item.State == models.WorkItemStateVerification {
		return settleVerification(ctx, wg, kb, registry, a.WorkItemID)
	}
	return nil
}
