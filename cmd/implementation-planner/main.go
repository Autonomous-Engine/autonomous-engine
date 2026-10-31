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
	"github.com/autonomousengine/autonomous-engine/internal/contracts"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/modeljson"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "implementation-planner"
	consumerGroup = "implementation-planner"
	healthPort    = 8094
)

// technicalBriefReadyMessage is received from technical-brief.ready.
type technicalBriefReadyMessage struct {
	WorkID       string `json:"work_id"`
	ProductID    string `json:"product_id"`
	BriefContent string `json:"brief_content"`
	Timestamp    string `json:"timestamp"`
	// Criteria mirror the technical_brief artifact's (M2-04); used when the
	// artifact cannot be read.
	Criteria *plan.BriefCriteria `json:"criteria,omitempty"`
	// Replan and ReplanEventID mark a brief status-orchestrator re-emitted
	// for a codex re-plan (codex coherence Part 1): the replanned event of
	// work_item_codex_events that emitted it. Additive.
	Replan        bool   `json:"replan,omitempty"`
	ReplanEventID string `json:"replan_event_id,omitempty"`
}

// implementationCompletePayload is received from implementation.complete.
// Shape must match the publisher in cmd/implementation-agent/main.go.
// Evidence is the release gate (D1.2): only a verified record may release a
// task's downstream dependencies. Success is the legacy fallback for
// messages without evidence (EVIDENCE_COMPAT_WINDOW=legacy). Reason is
// carried for logging / audit.
type implementationCompletePayload struct {
	TaskID       string   `json:"task_id"`
	WorkID       string   `json:"work_id"`
	FilesChanged []string `json:"files_changed"`
	TestResults  string   `json:"test_results"`
	Success      bool     `json:"success"`
	Reason       string   `json:"reason"`

	// Evidence is the additive v1 evidence.ImplementationEvidence object
	// (M0-07). It is kept raw so that a malformed value blocks with a reason
	// instead of failing the whole message decode; decideDependentRelease
	// judges it through evidence.ReleaseDecisionFromJSON. Absent (or JSON
	// null) means a pre-evidence producer: the EVIDENCE_COMPAT_WINDOW rules
	// apply and Success is the fallback.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// implementationTaskMessage is published to implementation.tasks.
//
// The AffectedModules / RelevantPatterns / RelevantAntiPatterns /
// CrossRepoTouchpoints fields are forwarded from the structured technical
// brief produced by research-coordinator so the downstream implementation
// agent inherits the same scope hints the planner used. All four are
// optional — a legacy brief without structured fields produces empty
// slices and the agent falls back to its own codex queries.
type implementationTaskMessage struct {
	TaskID        string   `json:"task_id"`
	WorkID        string   `json:"work_id"`
	ProductID     string   `json:"product_id"`
	RepositoryID  string   `json:"repository_id,omitempty"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	FilesToChange []string `json:"files_to_change"`
	Dependencies  []string `json:"dependencies"`
	CodexVersion  int      `json:"codex_version"`
	// CodexBranch / CodexModules (migration 060) complete CodexVersion as
	// the parent's codex pin: CodexVersion is then the pinned
	// product_version of CodexBranch, CodexModules the modules the plan
	// relies on. Absent for an unpinned parent (CodexVersion 0 = unknown).
	CodexBranch          string   `json:"codex_branch,omitempty"`
	CodexModules         []string `json:"codex_modules,omitempty"`
	TechnicalBrief       string   `json:"technical_brief"`
	CodexPatterns        string   `json:"codex_patterns"`
	AffectedModules      []string `json:"affected_modules,omitempty"`
	RelevantPatterns     []string `json:"relevant_patterns,omitempty"`
	RelevantAntiPatterns []string `json:"relevant_anti_patterns,omitempty"`
	CrossRepoTouchpoints []string `json:"cross_repo_touchpoints,omitempty"`

	// BaseSHA / BaseRef are the repository's pinned base commit and the
	// branch it was resolved from (M2-02), copied from the parent's pin.
	// Empty for a legacy-unpinned parent. Additive; M2-03 decides how
	// implementation uses them.
	BaseSHA string `json:"base_sha,omitempty"`
	BaseRef string `json:"base_ref,omitempty"`

	// TaskSpec is what the subtask commits to (criterion ids, deliverable,
	// verification, files with evidence) and Criteria are the acceptance
	// criteria it cites (M2-04, criteria.go). The description carries the
	// same in prose. Absent on messages planned before M2-04.
	TaskSpec *plan.TaskSpec   `json:"task_spec,omitempty"`
	Criteria []plan.Criterion `json:"criteria,omitempty"`
	// NonCodeCriteria are the plan's criteria no code task can satisfy
	// (non_code set, with the reason; R3). Every subtask of the plan
	// carries them so review (the verifier's requirements) records each as
	// a gap to verify manually. Not this subtask's work.
	NonCodeCriteria []plan.Criterion `json:"non_code_criteria,omitempty"`
	// RehomedCriteria are the criteria the planner attached to a step by
	// file overlap after the repair round left them owned by no task (R8):
	// the criterion id, the step's 1-based index and the files that matched.
	// Every subtask of the plan carries the list; the criterion itself is
	// an ordinary cited criterion of the subtask that owns the step.
	RehomedCriteria []rehomedCriterion `json:"rehomed_criteria,omitempty"`
	// AttachedChecks are the verification steps the planner attached to
	// satisfy a required check kind (R9: the repository's one whole-suite
	// command; the step itself is marked attached_by "planner" in
	// task_spec). Every subtask of the plan carries the list. Such a step
	// shows the kind is run, never that a criterion is met.
	AttachedChecks []attachedCheck `json:"attached_checks,omitempty"`

	// Contracts are the cross-repository contracts this subtask produces or
	// consumes (M2-06, contract_edges.go), each with the subtask on the
	// other side. A consumer's producers are also in Dependencies. Additive;
	// absent when the plan has no contract edge.
	Contracts []taskContract `json:"contracts,omitempty"`
	// LineSeeds are the brief's line seeds on this subtask's files (R10
	// W5): the implementation task's "## Where to edit". Additive.
	LineSeeds []lineSeed `json:"line_seeds,omitempty"`
	// PRRequirements are the brief's intake sentences asking for something
	// to be stated in the PR text (R15 T2); every subtask carries them and
	// pr-coordinator lists them in the PR description. Additive.
	PRRequirements []string `json:"pr_requirements,omitempty"`
	// Zones are the codex zones (migration 063) this subtask's files fall
	// in, with their policies (zones.go). Additive; absent when none.
	Zones []zones.Ref `json:"zones,omitempty"`
}

// structuredBrief mirrors the `technicalBrief` type written by
// cmd/research-coordinator to work_item_artifacts.content. Defined locally
// (not imported) because Go does not allow importing package main from
// another cmd/ binary. A legacy brief (pre-Agent-B, which only contained
// "brief" and "work_id") unmarshals into zero-valued slices — the caller
// can detect legacy briefs by checking len(Narrative) == 0 and fall back
// to Brief.
type structuredBrief struct {
	Brief                string   `json:"brief"`
	Narrative            string   `json:"narrative"`
	WorkID               string   `json:"work_id"`
	AffectedFiles        []string `json:"affected_files"`
	AffectedModules      []string `json:"affected_modules"`
	AffectedZones        []string `json:"affected_zones"`
	CrossRepoTouchpoints []string `json:"cross_repo_touchpoints"`
	RelevantPatterns     []string `json:"relevant_patterns"`
	RelevantAntiPatterns []string `json:"relevant_anti_patterns"`
	// Criteria are the brief's acceptance criteria and file evidence map
	// (M2-04); nil for a brief written before them.
	Criteria *plan.BriefCriteria `json:"criteria,omitempty"`
	// LineSeeds are research's places to edit (R10 W5); nil for an older
	// brief.
	LineSeeds []lineSeed `json:"line_seeds,omitempty"`
	// PRRequirements are the intake's PR-text sentences (R15 T2); nil for
	// an older brief.
	PRRequirements []string `json:"pr_requirements,omitempty"`

	// zonesPrompt is the planner's code_zones prompt section
	// (zonesPromptText), set by the handler; never part of the artifact.
	zonesPrompt string
}

// proseContent returns the prose body of the brief, preferring the newer
// `narrative` field and falling back to the legacy `brief` field. Empty
// string means the brief carries no prose at all — the caller must treat
// that as a decomposition-blocking failure rather than feed an empty
// string to the LLM.
func (b *structuredBrief) proseContent() string {
	if strings.TrimSpace(b.Narrative) != "" {
		return b.Narrative
	}
	return b.Brief
}

// decomposedTask represents one task from the LLM decomposition result.
//
// Files are the plain paths of FileClaims (filled by UnmarshalJSON); the
// claims, criterion ids, deliverable, verification and derived criteria are
// the M2-04 task contract (criteria.go).
type decomposedTask struct {
	Title           string                  `json:"title"`
	Description     string                  `json:"description"`
	Files           []string                `json:"-"`
	FileClaims      []fileClaim             `json:"files"`
	DependsOn       []string                `json:"depends_on"`
	RepositoryID    string                  `json:"repository_id,omitempty"`
	CriterionIDs    []string                `json:"criterion_ids,omitempty"`
	Deliverable     string                  `json:"deliverable,omitempty"`
	Verification    []plan.VerificationStep `json:"verification,omitempty"`
	DerivedCriteria []derivedCriterion      `json:"derived_criteria,omitempty"`

	// index is the step's 1-based position in the model's output (M2-05:
	// the number plan validation messages use); set by numberSteps.
	index int
}

func main() {
	// M2-05 developer subcommand: validate a captured plan and exit
	// (validate_cmd.go). Needs no configuration, database or bus.
	if len(os.Args) > 1 && os.Args[1] == validateFlag {
		os.Exit(runValidateCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("implementation-planner: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("implementation-planner: connect db: %v", err)
	}
	defer pool.Close()

	// Wire prompt store so strategy layer resolves prompts from DB.
	strategy.SetPromptStore(prompts.NewStore(pool))

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("implementation-planner: connect kafka: %v", err)
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

	// Evidence gate input (M0-07): the compatibility window governs
	// implementation.complete messages that carry no evidence; the product
	// store supplies the D2 partial-verification opt-in. Both are read
	// through the shared helpers so this consumer and status-orchestrator
	// cannot drift (an unrecognised value is strict).
	window := evidence.CompatWindowFromEnv()
	log.Printf("implementation-planner: %s=%s", evidence.CompatWindowEnv, window)

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-03). The planner no longer publishes
	// implementation.tasks directly: the event is committed to outbox_events
	// alongside the subtask row, and this loop is what carries it to Kafka.
	// It runs in the background alongside the consumer for the life of ctx,
	// and is the ONLY thing in this process that publishes implementation
	// tasks — if it does not start, planned work never reaches the
	// implementation agent, so a failure here is fatal rather than logged.
	//
	// Exactly one dispatcher per process: internal/metrics has no labels, so
	// a second would share (and race on) the same gauges, and the loop is
	// already batched across aggregates. The outbox metrics register at
	// package-init time, so /metrics below exports ae_outbox_* as zeros from
	// startup — an absent series and a broken scrape look alike, a zero does
	// not.
	dispatcher, err := outbox.Start(ctx, pool, kb, log.Default())
	if err != nil {
		log.Fatalf("implementation-planner: start outbox dispatcher: %v", err)
	}
	log.Printf("implementation-planner: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// Startup reconstruction (M1-06): plan state lives in plan_tasks, not in
	// this process, so a held task whose dependencies are all completed is
	// released here even if the process that planned it is long gone. Runs
	// before the consumer starts; idempotent, so every replica may run it.
	// A failure is logged, not fatal: the next completion in the same plan
	// releases the same tasks, and the next boot retries.
	if n, err := releaseReadyOnStartup(ctx, pool); err != nil {
		log.Printf("implementation-planner: startup release of ready held tasks: %v (released %d)", err, n)
	} else {
		log.Printf("implementation-planner: startup released %d ready held task(s)", n)
	}

	topics := []string{bus.TopicTechnicalBriefReady, bus.TopicImplementationComplete}
	// M1-05: deduplicate on the processed-event ledger (see the spec).
	handler := idempotency.NewLedger(pool).Handler("implementation-planner", buildHandler(pool, wg, cx, ps, registry, cs, kb, window))
	if err := kb.SubscribeMultiple(topics, consumerGroup, handler); err != nil {
		log.Fatalf("implementation-planner: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("implementation-planner: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("implementation-planner: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("implementation-planner: shutting down...")
	cancel()

	// Let the dispatch loop finish the iteration it is in. Its bookkeeping
	// runs on a detached context, so a publish that already reached the
	// broker is still recorded as published; waiting here just means we do
	// not tear the process down mid-write and re-deliver for nothing.
	// Wait() returns as soon as Run observes the cancelled context.
	dispatcher.Wait()
	log.Println("implementation-planner: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("implementation-planner: health server shutdown error: %v", err)
	}
	log.Println("implementation-planner: stopped")
}

// buildHandler wires the consumer callbacks.
//
// It no longer takes the process-lifetime context: the only thing that used it
// was the direct kb.Publish of implementation.tasks, which M1-03 replaced with
// an outbox write on the message context. Everything the handlers do now is
// scoped to the message being handled.
func buildHandler(
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	cx *codex.Codex,
	ps *products.ProductStore,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	window evidence.CompatWindow,
) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		switch msg.Topic {
		case bus.TopicTechnicalBriefReady:
			return handleTechnicalBrief(msgCtx, pool, wg, cx, ps, registry, cs, kb, msg)
		case bus.TopicImplementationComplete:
			// M1-05: completePlanTask / failPlanTask open the guarded transaction.
			return handleImplementationComplete(msgCtx, idempotency.Wrap(msgCtx, pool), wg, ps, window, msg)
		default:
			log.Printf("implementation-planner: unhandled topic %s", msg.Topic)
			return nil
		}
	}
}

func handleTechnicalBrief(
	msgCtx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	cx *codex.Codex,
	ps *products.ProductStore,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	msg *bus.Message,
) error {
	var payload technicalBriefReadyMessage
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("implementation-planner: technical-brief.ready: unmarshal: %v", err)
		return nil
	}

	workID, err := uuid.Parse(payload.WorkID)
	if err != nil {
		log.Printf("implementation-planner: technical-brief.ready: parse work_id %q: %v", payload.WorkID, err)
		return nil
	}

	log.Printf("implementation-planner: received technical-brief.ready for work_id=%s product_id=%s", workID, payload.ProductID)

	// Codex coherence Part 1: which brief this plan is built from, so its
	// commit can refuse a plan a codex re-plan superseded (codex_replan.go).
	if pool == nil {
		log.Printf("implementation-planner: work_id=%s: no database — the codex re-plan check is skipped for this run", workID)
	} else {
		g := replanGuard{}
		if payload.Replan {
			if id, perr := uuid.Parse(payload.ReplanEventID); perr == nil {
				g.replanEvent = id
			} else {
				log.Printf("implementation-planner: work_id=%s: re-plan brief without a valid replan_event_id %q — judged as an ordinary brief", workID, payload.ReplanEventID)
			}
		}
		if g.replanEvent == uuid.Nil {
			g.briefAt = briefProducedAt(msgCtx, pool, msg, payload.Timestamp)
		}
		msgCtx = withReplanGuard(msgCtx, g)
	}

	// Record agent assignment for this work item.
	assignment := &models.AgentAssignment{
		WorkItemID:      workID,
		AgentType:       models.AgentTypeImplementationPlanner,
		AgentInstanceID: agentID,
		Status:          models.AssignmentStatusActive,
	}
	if err := wg.AssignAgent(msgCtx, assignment); err != nil {
		log.Printf("implementation-planner: assign agent: %v", err)
	}

	// Parse product_id from the technical brief payload so context checks
	// and codex lookups stay scoped to the product this work item belongs to.
	var productID *uuid.UUID
	if payload.ProductID != "" {
		if pid, parseErr := uuid.Parse(payload.ProductID); parseErr == nil {
			productID = &pid
		}
	}

	// Load the structured brief artifact from the DB. The kafka message
	// still carries only `brief_content` (Agent B left the wire format
	// alone on purpose); the structured fields — affected_files,
	// affected_modules, relevant_patterns, etc — live on the artifact row.
	// A nil return is recoverable: we fall back to the payload's prose and
	// the LLM decomposes with no scope hints. This keeps legacy briefs
	// (pre-Agent-B, only "brief"+"work_id") and messages-without-artifact
	// flowing end-to-end rather than hard-failing work items.
	structured := loadStructuredBrief(msgCtx, pool, workID, payload.BriefContent)
	if structured.Criteria == nil && payload.Criteria != nil {
		structured.Criteria = payload.Criteria
	}
	// M2-04: criteria that fail structural validation are not planned
	// against; the plan proceeds as for a legacy brief (every task then
	// rendered as a hypothesis) and the reason is logged.
	if _, bcErr := briefCriteriaOf(structured); bcErr != nil {
		log.Printf("implementation-planner: work_id=%s: technical brief criteria are invalid (%v) — planning without them", workID, bcErr)
		structured.Criteria = nil
	}
	// M2-05: fold derived duplicates of user criteria before the model sees
	// them, so it is not asked to own one requirement under several ids.
	var briefFolds []plan.Fold
	structured.Criteria, briefFolds = foldBrief(workID, structured.Criteria)

	// Validate context before decomposing tasks with the one grounding gate
	// every stage uses (guards.CheckCodexGrounded, codex coherence Part 3),
	// so this stage cannot drift out of sync with the others. If productID
	// is missing we block rather than fall back to a cross-product query.
	// The work item's target branch scopes the check and the context
	// payload; empty means the product's default branch.
	branch := ""
	if wi, wiErr := wg.GetWorkItem(msgCtx, workID); wiErr == nil && wi != nil && wi.Branch != nil {
		branch = *wi.Branch
	}

	// R9: every hard block of the planner goes through escalateBlock (the
	// outbox path that raises the escalation prompt); directBlock is its
	// fallback when that transaction fails.
	directBlock := func(reason string) error {
		return guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason)
	}
	if productID == nil {
		reason := fmt.Sprintf("work item %s has no product_id; cannot check codex grounding", workID)
		log.Printf("implementation-planner: context validation failed for work_id=%s: %s", workID, reason)
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockContextValidation, reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	// Codex coherence Part 3: the one grounding gate (CheckCodexGrounded);
	// a refusal escalates with Re-index / Retry / Cancel.
	if refused, gerr := codexGate(msgCtx, pool, workID, *productID, branch, msg); refused || gerr != nil {
		if gerr != nil && idempotency.IsDuplicate(gerr) {
			log.Printf("implementation-planner: work_id=%s technical-brief.ready already handled — duplicate delivery, nothing committed", workID)
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		if gerr != nil {
			return fmt.Errorf("refuse work item %s on an ungrounded codex: %w", workID, gerr)
		}
		return nil
	}

	// Resolve LLM service per work item: checks agent_definition first, falls back to global.
	llmSvc, err := llm.NewServiceForWorkItem(msgCtx, registry, workID, cs, "implementation-planner", pool)
	if err != nil {
		log.Printf("implementation-planner: resolve LLM service for work_id=%s: %v — proceeding without LLM", workID, err)
		llmSvc = nil
	}

	// Transition to planning state.
	if err := wg.TransitionState(msgCtx, workID, models.WorkItemStatePlanning, agentID, "implementation-planner decomposing tasks"); err != nil {
		log.Printf("implementation-planner: transition to planning: %v", err)
	}

	// Migration 060: the codex pin the plan is built on — the brief's pin at
	// the branch's current version, read before the codex context below.
	// Every subtask message carries it, and commitDecomposition re-pins the
	// parent to it and copies it to each subtask; the subtasks'
	// codex_version is its product_version. A read error fails the
	// delivery, as for the base pins.
	codexPin, codexPinErr := resolvePlanningCodexPin(msgCtx, cx, pool, workID)
	if codexPinErr != nil {
		log.Printf("implementation-planner: work_id=%s: read codex pin: %v — failing the delivery so it is retried", workID, codexPinErr)
		return fmt.Errorf("read codex pin for %s: %w", workID, codexPinErr)
	}

	// Build Codex context — product-scoped (a product-less item was blocked
	// above, so there is no cross-product fallback). A context that cannot
	// be built stops the stage (escalation) instead of planning without the
	// codex.
	codexPatterns, err := cx.BuildContextForIntentBranch(msgCtx, *productID, branch)
	if err != nil {
		log.Printf("implementation-planner: build codex context for work_id=%s: %v — refusing", workID, err)
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		if rerr := refuseCodex(msgCtx, pool, workID, *productID, branch, guards.ContextErrorReason(*productID, err), msg); rerr != nil && !idempotency.IsDuplicate(rerr) {
			return fmt.Errorf("refuse work item %s after a codex context error: %w", workID, rerr)
		}
		return nil
	}

	// Read parent's agent_definition_id so subtasks can inherit it.
	var parentAgentDefID *uuid.UUID
	var parentAgentDefIDStr string
	if qErr := pool.QueryRow(msgCtx,
		`SELECT agent_definition_id::text FROM work_items WHERE id=$1 AND agent_definition_id IS NOT NULL`,
		workID,
	).Scan(&parentAgentDefIDStr); qErr == nil && parentAgentDefIDStr != "" {
		if parsed, pErr := uuid.Parse(parentAgentDefIDStr); pErr == nil {
			parentAgentDefID = &parsed
		}
	}

	// Build repo index for this product to enable repository assignment.
	// Maps repo name/path fragment → repository_id.
	repoIndex := map[string]string{}
	repoNames := map[string]string{}
	repoRoots := map[string]string{}
	if payload.ProductID != "" {
		if productID, parseErr := uuid.Parse(payload.ProductID); parseErr == nil {
			repos, listErr := ps.GetRepositoriesForProduct(msgCtx, productID)
			if listErr != nil {
				log.Printf("implementation-planner: list repos for product %s: %v", payload.ProductID, listErr)
			} else {
				for _, r := range repos {
					repoIndex[r.Name] = r.ID.String()
					repoIndex[r.ClonePath] = r.ID.String()
					repoNames[r.ID.String()] = r.Name
					repoRoots[r.ID.String()] = r.ClonePath
				}
			}
		}
	}

	// Codex zones (migration 063, zones.go): the prompt lists them, every
	// subtask carries the zones of its files, and a frozen zone stops the
	// plan below. A read error fails the delivery so it is retried rather
	// than planning past a frozen zone.
	planZones, zoneErr := loadPlanningZones(msgCtx, zones.NewStore(pool), productID)
	if zoneErr != nil {
		log.Printf("implementation-planner: work_id=%s: read codex zones: %v — failing the delivery so it is retried", workID, zoneErr)
		return fmt.Errorf("read codex zones for %s: %w", workID, zoneErr)
	}
	structured.zonesPrompt = zonesPromptText(planZones, repoNames)

	// M2-02: the parent's base pins (resolved at intake). Each subtask's
	// message carries its repository's pin, and commitDecomposition copies
	// the pin row to the subtask. A repository without a parent pin
	// (legacy-unpinned) gets neither, and implementation keeps its
	// branch-name behaviour. M2-05 also stats paths at the pin's snapshot.
	// A read error fails the delivery (redelivered and re-planned from
	// scratch; nothing has been committed yet) rather than planning a pinned
	// item as unpinned.
	parentPins, pinErr := loadParentPins(msgCtx, pool, workID)
	if pinErr != nil {
		log.Printf("implementation-planner: work_id=%s: read base pins: %v — failing the delivery so it is retried", workID, pinErr)
		return fmt.Errorf("read base pins for %s: %w", workID, pinErr)
	}

	parentItem, _ := wg.GetWorkItem(msgCtx, workID)
	codexVersion := plannedCodexVersion(codexPin, parentItem)
	var productIDPtr *uuid.UUID
	if payload.ProductID != "" {
		if pid, parseErr := uuid.Parse(payload.ProductID); parseErr == nil {
			productIDPtr = &pid
		}
	}
	inputs := &planInputs{
		pool:          pool,
		workID:        workID,
		productID:     payload.ProductID,
		productIDPtr:  productIDPtr,
		structured:    structured,
		briefFolds:    briefFolds,
		codexPatterns: codexPatterns,
		codexVersion:  codexVersion,
		codexPin:      codexPin,
		parentItem:    parentItem,
		repoIndex:     repoIndex,
		parentPins:    parentPins,
	}

	// Decompose into tasks via LLM, build the plan (phase 1, no writes), and
	// validate it (M2-05); a rejected plan gets ONE repair round with the
	// violations appended to the prompt verbatim.
	// decomposeIntoTasks returns nil + reason when the LLM is missing, errors,
	// or produces unparseable output — the planner MUST NOT fabricate a single
	// "Implement work item X" task in that case, because downstream the
	// implementation-agent would enter an open-ended tool loop against a
	// non-existent plan. Fail honestly → block the work item.
	//
	// Design: one branch per repo, one PR per repo. Phase 1 groups the LLM's
	// granular tasks by resolved repository_id into ONE work_item (subtask)
	// per distinct repo, all of a repo's steps consolidated into its
	// description (validation.go). Cross-repo ordering comes from the steps'
	// depends_on titles (M1-06): a subtask whose steps depend on another
	// subtask's steps is committed held and released only when that subtask
	// completes. No plan state is kept in memory (M1-06): it is all in
	// plan_tasks.
	// R9 P2: a repair round that made progress earns one more, while the
	// item has spent at most 60% of its token budget (D4, unchanged).
	outcome := planWithPolicy(workID,
		func(repair string, reserved []string) ([]decomposedTask, error) {
			return decomposeIntoTasks(msgCtx, llmSvc, workID, structured, briefFolds, codexPatterns, repair, reserved)
		},
		func(raw []decomposedTask) *planBuild { return inputs.build(msgCtx, raw) },
		repairBudget{budget: tools.TokenBudget(), spent: func() (int64, error) { return workItemTokensSpent(msgCtx, pool, workID) }},
	)
	if outcome.decomposeErr != nil {
		// R7: the reason tells a model-output failure from a provider one,
		// and the block goes through the outbox (work.blocked) like the
		// plan-validation block, so status-orchestrator raises the
		// escalation prompt instead of leaving a bare blocked item.
		reason := decompositionBlockReason(outcome.decomposeErr) + outcome.previousNote()
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, decompositionBlockKind(outcome.decomposeErr), reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	built := outcome.build
	if built.noRepository {
		reason := "Planner resolved no repository for any decomposed task. Every task needs a repository_id that maps to an enabled repository on this product. Retry re-decomposes once the product's repositories are set up." + outcome.previousNote()
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockNoRepository, reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	planned := built.planned

	// R3 review: every step was non-code work (deploy, notify, …): nothing
	// to implement. Block with the non-code criteria named, so a human sees
	// them, rather than completing as if planned.
	if built.allNonCode {
		reason := built.allNonCodeReason() + outcome.previousNote()
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockAllNonCode, reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}

	// Phase 1 can legitimately produce nothing: every group's tasks may have
	// been test-only on a repo with no test runner. That is not a failure —
	// there is simply nothing to implement — and it is already logged per
	// repo above.
	if len(planned) == 0 {
		log.Printf("implementation-planner: no repository subtask survived the test-infrastructure gate for work_id=%s — nothing to implement.%s", workID, outcome.previousNote())
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusCompleted) //nolint:errcheck
		return nil
	}

	// M2-05: still invalid after the repair round → block the parent with
	// the violations, and record them in the plan-validation artifact, in one
	// guarded transaction. Nothing of the plan is written.
	if outcome.rejected() {
		errs := plan.Errors(outcome.violations)
		log.Printf("implementation-planner: work_id=%s: plan still invalid after the repair round (%d violation(s)) — blocking", workID, len(errs))
		if err := blockForPlanValidation(msgCtx, idempotency.Wrap(msgCtx, pool), workID, outcome.violations, outcome.attempts, outcome.blockNote()); err != nil {
			if idempotency.IsDuplicate(err) {
				log.Printf("implementation-planner: work_id=%s technical-brief.ready already handled — duplicate delivery, nothing committed", workID)
				return nil
			}
			log.Printf("implementation-planner: work_id=%s: record the plan-validation block: %v — blocking without the artifact", workID, err)
			if bErr := directBlock(planValidationReason(errs, outcome.attempts) + outcome.blockNote()); bErr != nil {
				log.Printf("implementation-planner: block work item: %v", bErr)
			}
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	// Codex zones: each subtask carries the zones its files fall in; one in
	// a frozen zone is not planned and the item escalates, nothing written.
	if frozen := attachZones(planned, planZones, repoRoots); len(frozen) > 0 {
		reason := frozenZoneReason(frozen)
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockFrozenZone, reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	attachPlanWarnings(planned, outcome.violations)
	attachNonCodeCriteria(built)
	if built.criteria != nil {
		for _, c := range built.criteria.Criteria {
			if c.NonCode {
				log.Printf("implementation-planner: work_id=%s: %s", workID, plan.NonCodeGap(c))
			}
		}
	}

	// Dependencies (M1-06): resolve every step's depends_on title to the
	// repo-scoped subtask that subsumed it, and reject an unknown title, an
	// ambiguous one, or a cycle before anything is written (the validator has
	// already rejected those; this records the edges). Subtasks with no
	// unmet dependency are released (enqueued) by the commit below; the rest
	// are committed held and released by completions of what they wait on.
	depNotes, depErr := applyPlanDependencies(planned, planSteps(built.raw))
	if depErr != nil {
		reason := fmt.Sprintf(
			"Task decomposition produced an invalid dependency graph: %v. Nothing was created and no implementation.tasks event was queued. "+
				"Retry from technical_brief state to re-decompose.",
			depErr)
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockDependencyGraph, reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}
	if len(depNotes.dropped) > 0 {
		log.Printf("implementation-planner: %d dependency title(s) name steps the planner dropped (they will never run); their own dependencies are inherited instead: %v (work_id=%s)", len(depNotes.dropped), depNotes.dropped, workID)
	}
	for _, e := range depNotes.inherited {
		log.Printf("implementation-planner: inherited dependency %s (work_id=%s)", e, workID)
	}

	// Phase 2: every subtask row, every inherited agent definition, every
	// plan_tasks row and dependency edge (M1-06), and the implementation.tasks
	// events of the released subtasks commit together or not at all (M1-03).
	//
	// Before the outbox, the rows were inserted and the events published
	// separately: a publish failure was logged, the planner completed its
	// assignment, and the subtask sat in `implementation` with nothing on the
	// bus to pick it up — the orphaned-work defect the audit named. There is
	// no direct Publish of implementation.tasks here any more; the dispatcher
	// started in main() delivers the committed rows.
	//
	// M1-05: the plan's transaction carries the processed-event guard row
	// (idempotency.Wrap), so a redelivered technical-brief.ready cannot plan
	// the work item twice. Ledger.Handler (main) already acknowledges a
	// recorded duplicate before any of the work above; the guard here
	// catches one that raced it. Steps before this transaction (assignment,
	// the transition to planning, context-validation blocks) are pooled
	// writes outside the guard.
	if err := commitDecomposition(msgCtx, idempotency.Wrap(msgCtx, pool), wg, workID, parentAgentDefID, planned); err != nil {
		if idempotency.IsDuplicate(err) {
			log.Printf("implementation-planner: work_id=%s technical-brief.ready already planned — duplicate delivery, nothing committed", workID)
			return nil
		}
		if errors.Is(err, errPlanSupersededByReplan) {
			// Codex coherence Part 1: the re-plan's own technical-brief.ready
			// plans the parent against the current codex.
			log.Printf("implementation-planner: work_id=%s: this plan was superseded by a codex re-plan while it was being built — nothing committed; the re-plan's brief plans again", workID)
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}
		// The transaction rolled back, so the database is exactly as it was
		// before decomposition started: no subtask, no event, nothing
		// dispatched. Block the parent the way the planner blocks for any
		// other decomposition failure. A retry from technical_brief
		// re-decomposes from a clean slate and cannot duplicate anything,
		// which is the whole reason the plan commits as one unit.
		if errors.Is(err, errContractCycle) {
			reason := fmt.Sprintf("Task decomposition produced an invalid dependency graph: %v. Nothing was created and no implementation.tasks event was queued. "+
				"Retry from technical_brief state to re-decompose.", err)
			if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockDependencyGraph, reason, directBlock) {
				return nil
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}
		reason := fmt.Sprintf(
			"Could not commit the decomposition: %v. None of the %d planned repository subtask(s) were created and no implementation.tasks event was queued, so nothing was left half-done. "+
				"Retry from technical_brief state once the database is healthy.",
			err, len(planned))
		if escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockCommitFailed, reason, directBlock) {
			return nil
		}
		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
		return nil
	}

	for _, ps := range planned {
		log.Printf("implementation-planner: created repo-scoped subtask %s for repo %s (%d granular steps, %s, depends on %v, %d contract(s))", ps.msg.TaskID, ps.repoName, ps.steps, ps.initialStatus(), ps.dependsOn, len(ps.msg.Contracts))
	}
	log.Printf("implementation-planner: committed %d repository subtask(s) and their implementation.tasks events in one transaction for work_id=%s", len(planned), workID)

	wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusCompleted) //nolint:errcheck
	return nil
}

// plannedSubtask is one repo-scoped subtask and the event that announces it,
// assembled in phase 1 of the decomposition with no database writes.
//
// Holding the whole plan in memory before touching the database is what lets
// phase 2 be a single transaction: nothing is written until every group has
// been resolved, so a group that cannot be built cannot leave earlier groups
// committed behind it.
type plannedSubtask struct {
	// repoName and steps are for the log line only.
	repoName string
	steps    int
	// titles are the granular task titles that resolve to this subtask, and
	// dependsOnTitles the titles those steps depend on. applyPlanDependencies
	// turns them into dependsOn; the title -> task mapping is needed only
	// until then and is not kept anywhere.
	titles          []string
	dependsOnTitles []string
	// dependsOn are the task ids of the other subtasks of this plan this one
	// waits for. Empty: released at commit; otherwise committed held.
	dependsOn []uuid.UUID
	item      *models.WorkItem
	msg       implementationTaskMessage
	// stepTasks are the model's steps this subtask consolidates (M2-05: to
	// attribute validation messages and warnings to their steps).
	stepTasks []decomposedTask
	// criteria are the plan's criteria (the same pointer on every subtask of
	// one plan), read by contract edge derivation (M2-06); nil when none.
	criteria *plan.BriefCriteria
	// contractRefs maps each producer subtask this one consumes from to the
	// contracts of that edge (M2-06): the plan_task_dependencies row for the
	// pair is written with kind 'contract' and these as contract_ref.
	contractRefs map[uuid.UUID][]contracts.Ref
}

// outboxEnqueueAll is the seam the rollback tests use to make the enqueue fail
// after the subtask inserts have already run. Production code never reassigns
// it; it is a var rather than a direct call to outbox.EnqueueAll so that "the
// INSERTs and the enqueue really do share a fate" can be proven against a real
// database, which is the whole claim this ticket makes.
var outboxEnqueueAll = outbox.EnqueueAll

// commitDecomposition writes an entire decomposition — every repo-scoped
// subtask, every inherited agent definition, and every implementation.tasks
// event — in a single PostgreSQL transaction, and returns only once all of it
// is committed.
//
// This is the M1-03 vertical slice (D9). Per planned subtask the writes are:
//
//  1. the subtask work_items row;
//  2. its inherited agent_definition_id, when the parent has one, and its
//     copy of the parent's base pin with a resolved event (M2-02), when the
//     message carries one;
//  3. its plan_tasks row (held or released) and dependency edges (M1-06);
//  4. for a released subtask only, the outbox_events row carrying the
//     implementation.tasks message. A held subtask's message is stored on its
//     plan_tasks row and enqueued when its dependencies complete.
//
// The unit of atomicity is the PLAN, not the subtask. A per-subtask
// transaction would still be correct in the narrow sense — no subtask without
// its event — but it would let a failure partway through leave some groups
// committed and dispatched while the parent is blocked asking for a retry, and
// the retry would then duplicate them. A plan is one decision; it gets one
// commit. An error therefore means the database is exactly as it was before,
// and a retry starts from a clean slate.
//
// The events are enqueued last and in plan order. Their aggregate is the
// PARENT work item, not the subtask: outbox ordering is promised per
// aggregate_id, and the tasks of one plan are what need to reach the bus in
// the order they were planned. Using the subtask id would make every event its
// own aggregate and order nothing. The Kafka key stays the task id, unchanged
// from the direct publish, so partitioning and every consumer behave exactly
// as before.
func commitDecomposition(
	ctx context.Context,
	pool workgraph.TxBeginner, // the handler passes idempotency.Wrap(msgCtx, pool) (M1-05)
	wg *workgraph.WorkGraph,
	parentWorkID uuid.UUID,
	parentAgentDefID *uuid.UUID,
	planned []plannedSubtask,
) error {
	if len(planned) == 0 {
		return nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin decomposition transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	// Codex coherence Part 1: first, under the parent's row lock, refuse a
	// plan that a codex re-plan superseded while it was being built.
	if err := checkNotSupersededTx(ctx, tx, parentWorkID); err != nil {
		return err
	}

	// M2-06: cross-repository contract edges, read and derived in the plan's
	// transaction. A consumer gains its producer as a dependency (so it is
	// committed held below) and both messages carry the contracts; that is
	// why the events and stored messages are built after this.
	if _, err := deriveContractEdgesTx(ctx, tx, planned); err != nil {
		return err
	}
	// Migration 060: the parent's codex pin moves to the version this plan
	// was built on, and the messages carry the pin row's state.
	if err := repinParentTx(ctx, tx, parentWorkID, planned); err != nil {
		return err
	}

	// Build every event and stored message. A marshalling problem is the
	// caller's bug; the deferred rollback discards the transaction.
	events := make([]outbox.Event, 0, len(planned))
	stored := make([]json.RawMessage, 0, len(planned))
	for _, ps := range planned {
		raw, err := encodeStoredMessage(ps.msg)
		if err != nil {
			return err
		}
		stored = append(stored, raw)
		if ps.initialStatus() != planTaskReleased {
			continue
		}
		ev, err := outbox.NewEvent("work_item", parentWorkID, bus.TopicImplementationTasks, ps.msg.TaskID, ps.msg)
		if err != nil {
			return fmt.Errorf("build implementation.tasks event for %s: %w", ps.repoName, err)
		}
		events = append(events, ev)
	}

	for _, ps := range planned {
		if err := wg.CreateWorkItemTx(ctx, tx, ps.item); err != nil {
			return fmt.Errorf("subtask for %s: %w", ps.repoName, err)
		}
		if parentAgentDefID != nil {
			if err := wg.SetAgentDefinitionTx(ctx, tx, ps.item.ID, *parentAgentDefID); err != nil {
				return fmt.Errorf("subtask for %s: %w", ps.repoName, err)
			}
		}
		if err := copyPinTx(ctx, tx, parentWorkID, ps); err != nil {
			return err
		}
		if err := copyCodexPinTx(ctx, tx, parentWorkID, ps); err != nil {
			return err
		}
	}
	if err := insertPlanTasksTx(ctx, tx, parentWorkID, planned, stored); err != nil {
		return err
	}
	if err := outboxEnqueueAll(ctx, tx, events...); err != nil {
		return fmt.Errorf("enqueue implementation.tasks events: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit decomposition and its implementation.tasks events: %w", err)
	}
	return nil
}

// isLikelyOrphan reports whether a file exists in the repo but is not
// imported/referenced by any other source file. Orphan code lands as
// dead weight — editing it doesn't change anything users see. We use a
// cheap substring scan: look for the file's basename (stem) as a quoted
// string (imports in JS/TS/Go all quote paths) across sibling source
// files. False positives are acceptable (better than missing an
// obvious orphan); false negatives bias toward allowing the edit.
//
// Entry-point files that are never imported (pages, routes, cmd mains)
// are excluded — those are roots of the import graph, not orphans.
func isLikelyOrphan(repoRoot, relPath string) bool {
	rel := filepath.ToSlash(relPath)
	base := filepath.Base(rel)
	// Entry points in common frameworks: Next.js app router files,
	// top-level cmd mains, Rust main.rs, Python entrypoints, etc.
	entryPoints := []string{
		"page.tsx", "page.ts", "page.jsx", "page.js",
		"layout.tsx", "layout.ts", "layout.jsx", "layout.js",
		"route.ts", "route.js",
		"main.go", "main.ts", "main.js", "main.rs", "main.py",
		"index.tsx", "index.ts", "index.jsx", "index.js",
	}
	for _, ep := range entryPoints {
		if base == ep {
			return false
		}
	}
	// The search key: base filename without extension. For "contact-form.tsx"
	// we grep for "contact-form".
	stem := base
	if dot := strings.LastIndex(stem, "."); dot > 0 {
		stem = stem[:dot]
	}
	if stem == "" {
		return false
	}
	// Search every source file in the repo except the file itself, node_modules,
	// .next, and .git. If any other file contains the stem as a substring,
	// the file is reachable (or at least referenced). Otherwise orphan.
	absSelf := filepath.Join(repoRoot, filepath.FromSlash(rel))
	found := false
	_ = filepath.Walk(repoRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == ".next" || name == "dist" || name == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if p == absSelf {
			return nil
		}
		// Only scan source-like files to keep this cheap.
		switch {
		case strings.HasSuffix(p, ".tsx"), strings.HasSuffix(p, ".ts"),
			strings.HasSuffix(p, ".jsx"), strings.HasSuffix(p, ".js"),
			strings.HasSuffix(p, ".mjs"), strings.HasSuffix(p, ".cjs"):
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if strings.Contains(string(data), stem) {
				found = true
			}
		}
		return nil
	})
	return !found
}

// isTestOnlyTask reports whether a decomposed task targets only test
// files / test infrastructure. Used to drop test-writing tasks when the
// target repo has no test runner configured. The heuristic is
// conservative: a task is test-only when every file it names looks like
// a test file, OR when it names no files AND its title/description
// clearly describes test authoring.
func isTestOnlyTask(t decomposedTask) bool {
	if len(t.Files) > 0 {
		for _, f := range t.Files {
			if !looksLikeTestPath(f) {
				return false
			}
		}
		return true
	}
	// No files — fall back to title/description keywords. Keep this
	// tight: matching on bare "test" would strip legitimate work like
	// "update contact form test database". Require a clear writing verb.
	text := strings.ToLower(t.Title + " " + t.Description)
	testAuthoringPhrases := []string{
		"write test", "add test", "create test",
		"write unit test", "write integration test",
		"add unit test", "add integration test",
	}
	for _, p := range testAuthoringPhrases {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// looksLikeTestPath returns true for paths that conventionally hold test
// code (jest, vitest, pytest, go test, rspec patterns).
func looksLikeTestPath(p string) bool {
	lower := strings.ToLower(p)
	switch {
	case strings.Contains(lower, "/tests/"),
		strings.Contains(lower, "/test/"),
		strings.Contains(lower, "/__tests__/"),
		strings.Contains(lower, ".test."),
		strings.Contains(lower, ".spec."),
		strings.HasSuffix(lower, "_test.go"),
		strings.HasSuffix(lower, "_spec.rb"),
		strings.HasPrefix(lower, "tests/"),
		strings.HasPrefix(lower, "test/"):
		return true
	}
	return false
}

// inferRepositoryID looks at a list of file paths and tries to identify which
// repository they belong to by matching path prefixes or repo names.
func inferRepositoryID(files []string, repoIndex map[string]string) string {
	for _, f := range files {
		for repoKey, repoID := range repoIndex {
			if len(repoKey) > 0 && (len(f) >= len(repoKey) && f[:len(repoKey)] == repoKey) {
				return repoID
			}
			// Also check if the file path contains the repo name as a segment.
			if len(repoKey) > 0 && contains(f, repoKey) {
				return repoID
			}
		}
	}
	return ""
}

func contains(s, substr string) bool {
	if len(substr) == 0 || len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// handleImplementationComplete processes implementation.complete messages.
//
// Whether the completed task may release the tasks held behind it is decided
// from the message's evidence record via evidence.ReleaseDecision
// (decideDependentRelease): only verified releases (D1.2). partially_verified
// with the product opted in may advance only to mandatory human review (D2),
// so it is logged and nothing changes in the plan; everything else blocks
// with the decision's reason. Messages without evidence follow
// EVIDENCE_COMPAT_WINDOW.
//
// The plan itself is in plan_tasks (M1-06), so this handler holds no state:
//
//   - release: completePlanTask marks the task completed and, in the same
//     transaction, releases (and enqueues) every held task whose dependencies
//     are now all completed;
//   - block: failPlanTask marks the task failed and blocks the parent with
//     the reason, in one transaction. A non-release MUST NOT cascade into
//     releasing downstream dependencies — that would treat an unverified
//     implementation as a pass and corrupt the rest of the plan (D1).
//
// A database error is returned so the offset is not committed past a message
// the planner failed to act on; nothing was half-applied, because each path
// is one transaction. A duplicate message is a no-op.
func handleImplementationComplete(
	msgCtx context.Context,
	db workgraph.TxBeginner,
	wg *workgraph.WorkGraph,
	ps products.PolicyStore,
	window evidence.CompatWindow,
	msg *bus.Message,
) error {
	var payload implementationCompletePayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		log.Printf("implementation-planner: implementation.complete: unmarshal: %v", err)
		return nil
	}

	if !payload.hasEvidence() {
		if window == evidence.CompatWindowStrict {
			log.Printf("implementation-planner: implementation.complete: work_item=%s task=%s carries no evidence — blocking (%s=strict)", payload.WorkID, payload.TaskID, evidence.CompatWindowEnv)
		} else {
			metrics.EvidenceCompatLegacyFallback.Inc()
			log.Printf("implementation-planner: implementation.complete: WARNING work_item=%s task=%s carries no evidence — falling back to legacy success=%t (%s=legacy)", payload.WorkID, payload.TaskID, payload.Success, evidence.CompatWindowEnv)
		}
	}

	// The product policy is only consulted for partially_verified evidence;
	// any lookup failure yields the zero policy ("not opted in"), which can
	// only make the gate stricter.
	policy, repositoryID := releasePolicyForTask(msgCtx, wg, ps, payload.TaskID)

	d := decideDependentReleaseFor(payload, policy, window, repositoryID)

	taskID, parseErr := uuid.Parse(payload.TaskID)
	if parseErr != nil {
		log.Printf("implementation-planner: implementation.complete: task id %q is not a UUID (%v) — no plan task to update (decision %s: %s)", payload.TaskID, parseErr, d.Action, d.Reason)
		return nil
	}

	switch d.Action {
	case evidence.ActionRelease:
		out, err := completePlanTask(msgCtx, db, taskID)
		if idempotency.IsDuplicate(err) {
			return nil // M1-05: already applied; Ledger.Handler acknowledges it
		}
		if err != nil {
			log.Printf("implementation-planner: implementation.complete: task %s: record completion and release dependents: %v", taskID, err)
			return fmt.Errorf("complete plan task %s: %w", taskID, err)
		}
		switch {
		case !out.tracked:
			log.Printf("implementation-planner: implementation.complete: task %s released (%s) but has no plan_tasks row (planned before M1-06?) — nothing to release", taskID, d.Reason)
		case !out.changed:
			log.Printf("implementation-planner: implementation.complete: task %s is %s, not released — completion ignored, nothing released (%s)", taskID, out.previous, d.Reason)
		default:
			log.Printf("implementation-planner: implementation.complete: task %s completed (%s); released %d held task(s): %v", taskID, d.Reason, len(out.released), out.released)
		}
	case evidence.ActionHumanReview:
		log.Printf("implementation-planner: implementation.complete: task %s requires HUMAN REVIEW — NOT releasing dependents: %s", taskID, d.Reason)
	default:
		out, err := failPlanTask(msgCtx, db, taskID, d.Reason)
		if idempotency.IsDuplicate(err) {
			return nil // M1-05: already applied; Ledger.Handler acknowledges it
		}
		if err != nil {
			log.Printf("implementation-planner: implementation.complete: task %s: record failure: %v", taskID, err)
			return fmt.Errorf("fail plan task %s: %w", taskID, err)
		}
		switch {
		case !out.tracked:
			log.Printf("implementation-planner: implementation.complete: task %s BLOCKED — NOT releasing dependents (no plan_tasks row): %s", taskID, d.Reason)
		case !out.changed:
			log.Printf("implementation-planner: implementation.complete: task %s BLOCKED but already %s — no change: %s", taskID, out.previous, d.Reason)
		default:
			log.Printf("implementation-planner: implementation.complete: task %s FAILED — dependents stay held, parent %s blocked: %s", taskID, out.parentWorkID, d.Reason)
		}
	}
	return nil
}

// nonImplementationDropReason is why filterInvalidTasks dropped a step. Such
// a step is non-code work: a criterion only such steps cite is non-code
// (plan.NonCodeCriteria), not an orphan.
const nonImplementationDropReason = "not an implementation task (process or deployment work; it names no repository file)"

// knownRepositoryFileNames are repository files commonly named without an
// extension (compared case-insensitively on the base name).
var knownRepositoryFileNames = map[string]bool{
	"readme": true, "changelog": true, "license": true, "licence": true, "contributing": true,
	"authors": true, "notice": true, "codeowners": true, "makefile": true, "dockerfile": true,
	"procfile": true, "gemfile": true, "rakefile": true, "jenkinsfile": true, "vagrantfile": true,
}

// isRepositoryFilePath reports whether p reads as a file inside the
// repository: a relative path (backslashes read as slashes; no scheme, no
// drive, not absolute, not escaping with .., no host-like first segment
// such as "example.com/") whose base name has an extension (README.md,
// .env.example, src/a.ts) or is a known extension-less file name (README,
// CHANGELOG, Makefile, Dockerfile).
func isRepositoryFilePath(p string) bool {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || strings.ContainsAny(p, " \t\n") || strings.Contains(p, "://") {
		return false
	}
	if strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':') {
		return false
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if seg == ".." {
			return false
		}
	}
	// A scheme-less host path ("example.com/a.md"): a first segment with a
	// dot that is followed by a path. Leading-dot directories (.github,
	// .vscode) are repository directories.
	if len(segs) > 1 && !strings.HasPrefix(segs[0], ".") && strings.Contains(segs[0], ".") {
		return false
	}
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	if base == "" {
		return false
	}
	if knownRepositoryFileNames[strings.ToLower(base)] {
		return true
	}
	dot := strings.LastIndex(base, ".")
	return dot >= 0 && dot < len(base)-1
}

// namesRepositoryFile reports whether the task names at least one
// repository file among its files.
func namesRepositoryFile(t decomposedTask) bool {
	for _, c := range t.claims() {
		if isRepositoryFilePath(c.Path) {
			return true
		}
	}
	return false
}

// filterInvalidTasks removes tasks that don't belong in implementation:
// process, deployment, git-history and similar tasks that edit no file.
//
// R3 ruling: a task that edits files in the repository is an implementation
// task whatever its wording. Documentation files (README*, docs/**, *.md,
// CHANGELOG, .env.example, config samples) are repository files, so a
// documentation task that names one is kept; the keyword exclusion applies
// only to tasks that name no repository file.
func filterInvalidTasks(tasks []decomposedTask) []decomposedTask {
	excludeKeywords := []string{
		"document", "documentation", "user guide", "readme", "wiki",
		"commit history", "git log", "git history", "search for commit",
		"stakeholder", "inform stakeholder", "communicate to", "notify team",
		"deploy to", "deployment plan", "monitor performance", "benchmark",
		"confluence", "jira",
	}

	var filtered []decomposedTask
	for _, task := range tasks {
		if namesRepositoryFile(task) {
			filtered = append(filtered, task)
			continue
		}
		// R13 D3: a step that produces the PR text (the PR description, a
		// commit message, release notes) and names no repository file is
		// not implementation work: pr-coordinator writes the PR text. Its
		// criteria are PR-text criteria, recorded as non-code gaps.
		if plan.TargetsPRText(task.Title) || plan.TargetsPRText(task.Deliverable) {
			log.Printf("implementation-planner: filtering out non-implementation task (it produces the PR text): %s", task.Title)
			continue
		}
		titleLower := strings.ToLower(task.Title)
		descLower := strings.ToLower(task.Description)

		excluded := false
		for _, kw := range excludeKeywords {
			if strings.Contains(titleLower, kw) || strings.Contains(descLower, kw) {
				log.Printf("implementation-planner: filtering out non-implementation task: %s", task.Title)
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		filtered = append(filtered, task)
	}

	// R3 review: no fallback. When every step is non-code work the result
	// is empty and the planner blocks with "nothing to implement"
	// (planBuild.allNonCode) instead of promoting a file-less step to an
	// implementation subtask that would own every criterion.
	if len(filtered) == 0 && len(tasks) > 0 {
		log.Printf("implementation-planner: all %d tasks are non-code work — nothing to implement", len(tasks))
	}
	return filtered
}

// decomposeIntoTasks asks the LLM to break a technical brief into concrete
// implementation subtasks. This is a REQUIRED LLM call — there is no
// acceptable fallback. If the LLM is missing, errors, or returns content that
// cannot be parsed into a task list, the function returns (nil, error) so the
// caller can block the work item. Fabricating a single
// `{Title: "Implement work item X", Description: brief}` task hides a broken
// pipeline: the downstream implementation-agent sees an empty Files list and
// enters an open-ended tool loop against a non-existent plan, burning tokens
// and producing nothing reviewable.
//
// The structured brief's slice fields are rendered as extra Markdown
// sections in the user prompt (via the strategy layer's context map) so
// the LLM sees both the narrative AND explicit hints about which files,
// modules, and patterns to concentrate on. Empty slices produce empty
// sections which strategy.buildUserPrompt skips automatically.
//
// reserved are the ids earlier planning rounds used (R13 review M1; nil on
// the first round): the prompt's "numbering from AC-n" and R7's fresh ids
// skip them.
func decomposeIntoTasks(ctx context.Context, llmSvc *llm.Service, workID uuid.UUID, brief structuredBrief, briefFolds []plan.Fold, codexPatterns, repair string, reserved []string) ([]decomposedTask, error) {
	if llmSvc == nil {
		return nil, fmt.Errorf("no LLM configured for implementation-planner — cannot decompose tasks")
	}

	narrative := brief.proseContent()
	if strings.TrimSpace(narrative) == "" {
		return nil, &emptyBriefError{err: fmt.Errorf("technical brief has no prose content (neither narrative nor legacy brief field) — refusing to decompose against an empty brief")}
	}

	ctxMap := map[string]string{
		"technical_brief": narrative,
		"codex_patterns":  codexPatterns,
		"work_id":         workID.String(),
		"work_item_id":    workID.String(),
	}
	if len(brief.AffectedFiles) > 0 {
		ctxMap["likely_files_to_modify"] = formatBulletList(brief.AffectedFiles) +
			"\n\nYou should strongly prefer these files when assigning subtasks. If you need to touch a file outside this list, cite which research task surfaced the need in the subtask description."
	}
	if len(brief.AffectedModules) > 0 {
		ctxMap["affected_modules"] = formatBulletList(brief.AffectedModules) +
			"\n\nUse these as subtask scoping hints — one subtask per module when the module boundaries are clean."
	}
	if len(brief.RelevantPatterns) > 0 {
		ctxMap["patterns_to_follow"] = formatBulletList(brief.RelevantPatterns) +
			"\n\nName these patterns in the subtask description (e.g. \"follow pattern 'idempotent-handler'\") so the implementation agent knows which reference to pull from the codex."
	}
	if len(brief.RelevantAntiPatterns) > 0 {
		ctxMap["patterns_to_avoid"] = formatBulletList(brief.RelevantAntiPatterns) +
			"\n\nExplicitly steer subtasks away from these."
	}
	if len(brief.CrossRepoTouchpoints) > 0 {
		ctxMap["repos_touched"] = formatBulletList(brief.CrossRepoTouchpoints) +
			"\n\nWhen a subtask touches code in one of these repos, set the subtask's repository_id to that repo."
	}

	if brief.zonesPrompt != "" {
		ctxMap["code_zones"] = brief.zonesPrompt
	}

	criteriaPromptContext(ctxMap, brief.Criteria, briefFolds, reserved...)
	if repair != "" {
		// M2-05: the repair round — the validator's messages, verbatim.
		ctxMap["plan_validation"] = repair
	}

	intent := strategy.Intent{
		Type:         strategy.IntentDecomposeTasks,
		Context:      ctxMap,
		OutputFormat: decomposeOutputFormat,
	}

	const maxDecomposeRetries = 2
	for attempt := 0; attempt < maxDecomposeRetries; attempt++ {
		result, err := llmSvc.Execute(ctx, intent)
		if err != nil {
			if attempt < maxDecomposeRetries-1 {
				log.Printf("implementation-planner: decompose attempt %d failed: %v — retrying", attempt+1, err)
				continue
			}
			return nil, fmt.Errorf("LLM decompose call failed: %w", err)
		}
		if result == nil || strings.TrimSpace(result.Content) == "" {
			if attempt < maxDecomposeRetries-1 {
				log.Printf("implementation-planner: decompose attempt %d returned empty content — retrying", attempt+1)
				continue
			}
			return nil, fmt.Errorf("LLM returned empty content for decomposition — check rate limits and provider logs")
		}

		tasks, parseErr := parseTaskArray(result.Content)
		if parseErr == nil {
			// R7: a brief criterion redeclared under derived_criteria is
			// turned into a citation (renumbered when its text differs)
			// before the strict pass, rather than rejecting the plan. The
			// re-prompt quotes the tasks as the model sent them (sent is a
			// second parse of the same content, untouched by normalising).
			sent, _ := parseTaskArray(result.Content)
			for _, note := range normaliseDerivedCriteria(tasks, brief.Criteria, briefFolds, reserved...) {
				log.Printf("implementation-planner: work_id=%s: %s", workID, note)
			}
			// R13 D1: a task cites the derived criteria it declares.
			for _, note := range citeOwnDerivedCriteria(tasks) {
				log.Printf("implementation-planner: work_id=%s: %s", workID, note)
			}
			// M2-04: strict kinds. A rejected task list is re-prompted once
			// with the reason, then the item is blocked with it.
			vErr := validateDecomposedTasks(tasks, brief.Criteria, briefFolds, reserved...)
			if vErr == nil {
				return tasks, nil
			}
			if attempt < maxDecomposeRetries-1 {
				log.Printf("implementation-planner: decompose attempt %d rejected: %v — re-prompting", attempt+1, vErr)
				intent.Context["retry_nudge"] = decompositionRetryNudge(vErr, sent)
				continue
			}
			return nil, &decompositionOutputError{err: fmt.Errorf("LLM decomposition rejected: %w", vErr)}
		}

		if attempt < maxDecomposeRetries-1 {
			preview := result.Content
			if len(preview) > 300 {
				preview = preview[:300] + "..."
			}
			log.Printf("implementation-planner: decompose attempt %d returned unparseable JSON (preview: %s) — retrying with nudge", attempt+1, preview)
			intent.Context["retry_nudge"] = "Your previous response could not be parsed as a JSON array. Respond with ONLY a valid JSON array, no markdown fences, no explanatory text, no thinking tags. Start with [ and end with ]."
			continue
		}

		preview := result.Content
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		log.Printf("implementation-planner: final decompose attempt returned unparseable content (preview: %s)", preview)
		return nil, &decompositionOutputError{err: fmt.Errorf("LLM returned unparseable JSON for decomposition — the response was not a valid task array")}
	}
	return nil, fmt.Errorf("LLM decompose exhausted all retries")
}

// parseTaskArray extracts a []decomposedTask from raw LLM output, trying
// multiple extraction strategies: direct parse, bracket extraction,
// thinking-tag stripping, code-fence extraction, and comment stripping.
func parseTaskArray(content string) ([]decomposedTask, error) {
	var tasks []decomposedTask

	// 1. Direct parse
	if err := json.Unmarshal([]byte(content), &tasks); err == nil && len(tasks) > 0 {
		return tasks, nil
	}

	// 2. Strip <think>...</think> tags (reasoning models sometimes leak these into content)
	cleaned := stripThinkingTags(content)
	if cleaned != content {
		if err := json.Unmarshal([]byte(cleaned), &tasks); err == nil && len(tasks) > 0 {
			log.Printf("implementation-planner: parsed decomposition after stripping thinking tags")
			return tasks, nil
		}
	}

	// 3. The first balanced top-level array that decodes to tasks: prose,
	// fences, a stray trailing "]" or a second array after it are ignored
	// (the same envelope tolerance as research's criteria parse, R5 I4).
	if got := firstTaskArray(cleaned); len(got) > 0 {
		return got, nil
	}

	// 4. Extract from bracket delimiters
	start := indexOf(cleaned, '[')
	end := lastIndexOf(cleaned, ']')
	if start >= 0 && end > start {
		snippet := cleaned[start : end+1]
		if err := json.Unmarshal([]byte(snippet), &tasks); err == nil && len(tasks) > 0 {
			return tasks, nil
		}
		// 5. Strip JS-style comments
		stripped := stripJSONComments(snippet)
		if err := json.Unmarshal([]byte(stripped), &tasks); err == nil && len(tasks) > 0 {
			log.Printf("implementation-planner: parsed decomposition after stripping JSON-style comments")
			return tasks, nil
		}
		if got := firstTaskArray(stripped); len(got) > 0 {
			log.Printf("implementation-planner: parsed decomposition after stripping JSON-style comments")
			return got, nil
		}
	}

	return nil, fmt.Errorf("could not extract valid task array from content")
}

// firstTaskArray returns the tasks of the first balanced top-level JSON
// array in s that decodes to a non-empty task list, or nil.
func firstTaskArray(s string) []decomposedTask {
	for _, a := range modeljson.Arrays(s) {
		var tasks []decomposedTask
		if err := json.Unmarshal([]byte(a), &tasks); err == nil && len(tasks) > 0 {
			return tasks
		}
	}
	return nil
}

// stripThinkingTags removes <think>...</think> blocks from content.
func stripThinkingTags(s string) string {
	result := s
	for {
		start := strings.Index(result, "<think>")
		if start < 0 {
			break
		}
		end := strings.Index(result[start:], "</think>")
		if end < 0 {
			result = result[:start]
			break
		}
		result = result[:start] + result[start+end+len("</think>"):]
	}
	return strings.TrimSpace(result)
}

// stripJSONComments removes // line comments and /* */ block comments from
// a JSON string while preserving contents inside string literals. LLMs emit
// JS-flavored JSON often enough (qwen2.5-coder routinely adds "// Assuming
// this is the ID of X" annotations) that tolerating the strip-and-retry is
// worth the small parser. We only remove syntax that's never legal JSON;
// if the LLM produced something structurally different, Unmarshal will
// still reject it and we fail honestly in the caller.
func stripJSONComments(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(s) {
			if s[i+1] == '/' {
				// Line comment — skip through end of line.
				j := i + 2
				for j < len(s) && s[j] != '\n' {
					j++
				}
				i = j - 1 // loop will i++
				continue
			}
			if s[i+1] == '*' {
				// Block comment — skip through closing */.
				j := i + 2
				for j+1 < len(s) && !(s[j] == '*' && s[j+1] == '/') {
					j++
				}
				if j+1 < len(s) {
					j += 2
				}
				i = j - 1
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.String()
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func lastIndexOf(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// loadStructuredBrief reads the most recent technical_brief artifact for a
// work item and returns its parsed contents. The fallback prose
// (`fallbackBrief`, from the kafka message) is ALWAYS set on the returned
// struct under Brief+Narrative when the DB lookup returns nothing or the
// JSON fails to unmarshal — callers can assume proseContent() is
// non-empty if the kafka message itself had prose.
//
// This function does not error: any failure degrades to a brief with only
// prose content (legacy shape). Refusing to decompose because the artifact
// row is missing would hard-fail work items whose research ran before
// Agent B added structured fields; we prefer a soft degradation with a
// log line.
func loadStructuredBrief(ctx context.Context, pool *pgxpool.Pool, workID uuid.UUID, fallbackBrief string) structuredBrief {
	fallback := structuredBrief{
		Brief:     fallbackBrief,
		Narrative: fallbackBrief,
		WorkID:    workID.String(),
	}

	var content []byte
	row := pool.QueryRow(ctx, `
		SELECT content FROM work_item_artifacts
		 WHERE work_item_id = $1 AND artifact_type = 'technical_brief'
		   AND COALESCE(content->>'name', '') <> 'plan-validation'
		 ORDER BY created_at DESC LIMIT 1
	`, workID)
	if err := row.Scan(&content); err != nil {
		log.Printf("implementation-planner: no technical_brief artifact for work_id=%s (%v) — using prose from kafka payload", workID, err)
		return fallback
	}

	var brief structuredBrief
	if err := json.Unmarshal(content, &brief); err != nil {
		log.Printf("implementation-planner: technical_brief artifact for work_id=%s is not valid JSON (%v) — using prose from kafka payload", workID, err)
		return fallback
	}

	// If the artifact was written before Agent B added the structured
	// fields, Narrative will be empty. Fill it from Brief so downstream
	// callers don't need to branch on both keys.
	if strings.TrimSpace(brief.Narrative) == "" {
		brief.Narrative = brief.Brief
	}
	// If the artifact has no prose at all (shouldn't happen, but defend
	// anyway) fall back to the kafka payload's prose.
	if strings.TrimSpace(brief.Narrative) == "" && strings.TrimSpace(brief.Brief) == "" {
		brief.Narrative = fallbackBrief
		brief.Brief = fallbackBrief
	}
	// R3: a stored brief never carries a trusted non-code mark; only
	// attachNonCodeCriteria sets one, after validation.
	plan.StripNonCode(brief.Criteria)
	return brief
}

// intersectOrRetain returns the intersection of `wanted` and `scope` when
// `scope` is non-empty, otherwise returns `wanted` unchanged. The result
// preserves wanted's ordering for reproducibility in dispatched task
// messages. When the intersection is empty but both inputs are non-empty
// — i.e. the LLM assigned files outside the research brief's affected
// set — we return the LLM's set verbatim and let the implementation
// agent's codex.gap signal surface the discrepancy. Hard-blocking would
// cause a cascade when the brief is incomplete.
//
// When `wanted` is empty but `scope` is non-empty, we return `scope` as a
// fallback — the research brief's affected files are a better default than
// nothing. An empty FilesToChange disables the implementation-agent's
// continuation loop, causing partial completions to go undetected.
func intersectOrRetain(wanted, scope []string) []string {
	if len(wanted) == 0 {
		return scope
	}
	if len(scope) == 0 {
		return wanted
	}
	allowed := make(map[string]struct{}, len(scope))
	for _, s := range scope {
		allowed[s] = struct{}{}
	}
	var intersect []string
	for _, w := range wanted {
		if _, ok := allowed[w]; ok {
			intersect = append(intersect, w)
		}
	}
	if len(intersect) == 0 {
		return wanted
	}
	return intersect
}

// filePathRe matches common source-code file paths: at least one directory
// separator followed by a filename with a recognised extension. Anchored
// to word boundaries via the surrounding (?:^|[\s`"'(\[]) to avoid
// matching URL fragments or prose that happens to contain slashes.
var filePathRe = regexp.MustCompile(`(?:^|[\s` + "`" + `"'(\[])([a-zA-Z0-9_./-]+\.(?:go|ts|tsx|js|jsx|json|yaml|yml|css|scss|html|py|rs|toml|sql|proto|graphql|svelte|vue|md))(?:$|[\s` + "`" + `"')\]])`)

// filesMarkerRe matches the "Files: [path1 path2 ...]" markers that the
// planner writes into step descriptions. Go's %v for a string slice
// produces "[a b c]" — space-separated, no commas.
var filesMarkerRe = regexp.MustCompile(`Files:\s*\[([^\]]+)\]`)

// extractFilePathsFromDescription scans description text for file-path-like
// strings and returns unique paths. This is a last-resort fallback when the
// LLM's structured `files` array is empty — descriptions generated by
// decomposeIntoTasks routinely reference target files in prose even when
// the JSON `files` field was omitted.
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

// formatBulletList renders a slice as a Markdown bullet list. Returns an
// empty string on empty input so callers can omit the section entirely.
func formatBulletList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString("- ")
		sb.WriteString(it)
		sb.WriteString("\n")
	}
	return sb.String()
}
