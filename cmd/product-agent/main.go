package main

import (
	"context"
	"encoding/json"
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
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// updateTitleSQL replaces the work item title with the one extracted from
// the PRD; it commits with the PRD artifact.
const updateTitleSQL = `UPDATE work_items SET title = $1 WHERE id = $2`

const (
	agentID       = "product-agent"
	consumerGroup = "product-agent"
	healthPort    = 8091
)

// intakeMessage is the payload received from the intake topic.
type intakeMessage struct {
	WorkID      string                 `json:"work_id"`
	ProductID   string                 `json:"product_id"`
	Description string                 `json:"description"`
	Source      string                 `json:"source"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// prdReadyMessage is published to prd.ready after PRD generation.
type prdReadyMessage struct {
	WorkID     string `json:"work_id"`
	ProductID  string `json:"product_id"`
	WorkType   string `json:"work_type"`
	PRDContent string `json:"prd_content"`
	Timestamp  string `json:"timestamp"`
}

// humanReviewMessage is published to human-review.queue for ambiguous items.
type humanReviewMessage struct {
	WorkID string `json:"work_id"`
	Reason string `json:"reason"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("product-agent: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("product-agent: connect db: %v", err)
	}
	defer pool.Close()

	// Wire prompt store so strategy layer resolves prompts from DB.
	strategy.SetPromptStore(prompts.NewStore(pool))

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("product-agent: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
	cx := codex.New(pool)
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

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04). The PRD approval prompt and work.blocked are
	// committed to outbox_events with the state they announce; this loop is
	// what carries them to Kafka, so a failure to start is fatal.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), agentID)
	if err != nil {
		log.Fatalf("product-agent: start outbox dispatcher: %v", err)
	}
	log.Printf("product-agent: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M1-05: deduplicate on the processed-event ledger. The up-front Seen
	// check runs before the handler reads the item, so a late duplicate
	// intake can no longer block an item that has moved on to research.
	//
	// M2-02: intake pins every repository's base commit (pins.go).
	pinner := &intakePinner{repos: products.NewStore(pool), resolver: newResolver(cs, cfg.GitHub.Token)}
	handler := idempotency.NewLedger(pool).Handler("product-agent", buildHandler(ctx, pool, wg, cx, registry, cs, kb, pinner))
	if err := kb.Subscribe(bus.TopicIntake, consumerGroup, handler); err != nil {
		log.Fatalf("product-agent: subscribe to intake: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("product-agent: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("product-agent: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("product-agent: shutting down...")
	cancel()
	dispatcher.Wait()
	log.Println("product-agent: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("product-agent: health server shutdown error: %v", err)
	}
	log.Println("product-agent: stopped")
}

func buildHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	cx *codex.Codex,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	pinner *intakePinner,
) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		var intake intakeMessage
		if err := json.Unmarshal(msg.Value, &intake); err != nil {
			log.Printf("product-agent: unmarshal intake: %v", err)
			return nil
		}

		workID, err := uuid.Parse(intake.WorkID)
		if err != nil {
			log.Printf("product-agent: parse work_id %q: %v", intake.WorkID, err)
			return nil
		}

		log.Printf("product-agent: processing work_id=%s product_id=%v", workID, intake.ProductID)

		// Load the work item once up front: its branch scopes grounding, its
		// parent (if any) is the outbox aggregate, and its product_id backs
		// up an intake payload that omitted one.
		wi, wiErr := wg.GetWorkItem(msgCtx, workID)
		if wiErr != nil {
			log.Printf("product-agent: load work item %s: %v", workID, wiErr)
			wi = nil
		}
		aggregateID := aggregateFor(workID, wi)
		productID := resolveProduct(intake.ProductID, wi)

		// Record agent assignment for this work item.
		assignment := &models.AgentAssignment{
			WorkItemID:      workID,
			AgentType:       models.AgentTypeProduct,
			AgentInstanceID: agentID,
			Status:          models.AssignmentStatusActive,
		}
		if err := wg.AssignAgent(msgCtx, assignment); err != nil {
			log.Printf("product-agent: assign agent: %v", err)
		}

		// A work item without a product cannot get a PRD: grounding is
		// product-scoped (no cross-product fallback) and the PRD approval prompt
		// needs a product (user_interactions.product_id is NOT NULL). Block
		// now, before any LLM work, rather than let the prompt insert roll
		// back the PRD at the end.
		if productID == nil {
			reason := fmt.Sprintf("Work item %s has no product, so product-agent cannot ground or approve a PRD for it. "+
				"Assign the work item to a product, then retry it.", workID)
			log.Printf("product-agent: blocking work_id=%s — no product", workID)
			if err := blockInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason); err != nil {
				log.Printf("product-agent: block work item: %v", err)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		// M2-02: pin one base commit per repository before any LLM work.
		// Resolution is network (fetch / ls-remote), so it runs here,
		// outside any transaction; the pins commit with the PRD (commitPRD).
		// A repository that cannot be resolved blocks the item, with its
		// escalation prompt, in one transaction.
		pins, pinErr := pinner.resolve(msgCtx, workID, *productID, wi)
		if pinErr != nil {
			reason := pinFailureReason(pinErr)
			log.Printf("product-agent: blocking work_id=%s — base SHA resolution failed: %v", workID, pinErr)
			title := ""
			if wi != nil {
				title = wi.Title
			}
			prompt := pinBlockedPrompt(workID, *productID, title, reason)
			if err := blockWithPromptInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason, prompt); err != nil {
				log.Printf("product-agent: block work item: %v", err)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		// Validate context before doing any LLM work: the one grounding gate
		// of every stage (guards.CheckCodexGrounded, codex coherence Part 3).
		// The work item's target branch scopes grounding + context payload
		// to the branch-specific partition; empty = the product's default
		// branch. A refusal escalates (Re-index / Retry / Cancel).
		branch := ""
		if wi != nil && wi.Branch != nil {
			branch = *wi.Branch
		}

		if refused, err := codexGate(msgCtx, pool, aggregateID, workID, *productID, branch, msg); refused || err != nil {
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			if err != nil && !idempotency.IsDuplicate(err) {
				return fmt.Errorf("refuse work item %s on an ungrounded codex: %w", workID, err)
			}
			return nil
		}

		// Resolve LLM service per work item: checks agent_definition first, falls back to global.
		llmSvc, err := llm.NewServiceForWorkItem(msgCtx, registry, workID, cs, "product-agent", pool)
		if err != nil {
			log.Printf("product-agent: resolve LLM service for work_id=%s: %v — proceeding without LLM", workID, err)
			llmSvc = nil
		}

		// Transition to PRD state.
		if err := wg.TransitionState(msgCtx, workID, models.WorkItemStatePRD, agentID, "product-agent starting PRD generation"); err != nil {
			log.Printf("product-agent: transition to prd: %v", err)
		}

		// Load the work item from the DB so we always have the authoritative
		// title + description, regardless of what the upstream publisher put
		// on the Kafka message. Some publishers (portal, update-and-retry)
		// historically omitted `description` from the intake payload, which
		// produced a prompt with no `## Pain Point` section and caused the
		// clarity gate to block every well-formed work item with
		// "What is the description?". The DB is the source of truth: use it.
		workTitle := ""
		workDescription := intake.Description
		if wi, wiErr := wg.GetWorkItem(msgCtx, workID); wiErr == nil && wi != nil {
			workTitle = wi.Title
			if strings.TrimSpace(workDescription) == "" {
				workDescription = wi.Description
			}
		} else if wiErr != nil {
			log.Printf("product-agent: load work item for %s: %v — falling back to intake payload fields", workID, wiErr)
		}

		// Build Codex context — product-scoped (a product-less item was
		// blocked above, so there is no cross-product fallback to consider).
		// A context that cannot be built stops the stage (escalation with
		// Re-index / Retry / Cancel) instead of running without the codex.
		codexContext, err := cx.BuildContextForIntentBranch(msgCtx, *productID, branch)
		if err != nil {
			log.Printf("product-agent: build codex context for work_id=%s: %v — refusing", workID, err)
			reason := guards.ContextErrorReason(*productID, err)
			if rerr := refuseCodex(msgCtx, pool, aggregateID, workID, *productID, branch, reason, msg); rerr != nil && !idempotency.IsDuplicate(rerr) {
				wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
				return fmt.Errorf("refuse work item %s after a codex context error: %w", workID, rerr)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		// Clarity check + work-type classification, both via one LLM call.
		// Structured JSON output makes ambiguity explicit: if the LLM decides
		// the input is too vague to produce a meaningful PRD, we block the
		// work item with the specific questions it needs answered — rather
		// than hallucinating a PRD about a different product. This is the
		// cheap pre-filter; the human approval gate at prd_approval is the
		// second line of defense.
		workType := "feature"
		if llmSvc != nil {
			classifyIntent := strategy.Intent{
				Type: strategy.IntentAnalyzePainPoint,
				Context: map[string]string{
					"title":         workTitle,
					"pain_point":    workDescription,
					"codex_context": codexContext,
					"source":        intake.Source,
					"work_item_id":  workID.String(),
				},
				OutputFormat: `JSON object with exactly these fields:
  "is_clear": boolean — true if a competent engineer with access to the codebase could figure out what needs to change and write a PRD without asking the requester anything else. The requester is a non-technical stakeholder — do NOT require them to name specific files, pages, systems, or acceptance criteria. If the INTENT is clear ("add phone number to contact form", "fix the login bug"), that is clear. Only mark is_clear=false when the request is genuinely ambiguous (e.g. "make it better", "update the thing", no indication of what to change or why). Use the codex context to resolve which systems/pages are involved — that is YOUR job, not the requester's.
  "work_type": one of "feature" | "bug"
  "questions": array of strings — if is_clear=false, list ONLY questions whose answers cannot be inferred from the codebase or common sense. Never ask about implementation details (which file, which page, how to display it) — those are engineering decisions, not requester decisions.
Return ONLY the JSON. No prose before or after.`,
			}
			classResult, err := llmSvc.Execute(msgCtx, classifyIntent)
			if err != nil {
				log.Printf("product-agent: clarity check: %v — proceeding conservatively (work_type=feature)", err)
			} else {
				clarity := parseClarityResult(classResult.Content)
				if clarity.WorkType == "bug" {
					workType = "bug"
				}
				if !clarity.IsClear {
					// Block with the LLM's questions. No PRD generated, no
					// research dispatched, no LLM time wasted.
					reason := "Intake is ambiguous — the LLM could not produce a meaningful PRD without more detail. Questions that need answering:\n\n" +
						"  • " + strings.Join(clarity.Questions, "\n  • ") +
						"\n\nEdit the work item's description to address these, then transition it back to intake."
					log.Printf("product-agent: blocking ambiguous intake for work_id=%s (%d questions)", workID, len(clarity.Questions))
					if bErr := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); bErr != nil {
						log.Printf("product-agent: block work item: %v", bErr)
					}
					wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusCompleted) //nolint:errcheck
					return nil
				}
			}
		}

		// Generate PRD. This is the *required* LLM call for this agent — there
		// is no acceptable placeholder. If the LLM is missing, times out, or
		// returns empty content, block the work item with the real cause. Do
		// NOT fabricate a "TBD" PRD and push it downstream: research agents
		// will spend expensive LLM time trying to analyze an empty brief.
		// This is the contract the principles page promises — fail honestly.
		if llmSvc == nil {
			reason := "No LLM provider configured. Connect one at Admin → Providers, then retry this work item."
			log.Printf("product-agent: %s (work_id=%s)", reason, workID)
			if err := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); err != nil {
				log.Printf("product-agent: block work item: %v", err)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		prdIntent := strategy.Intent{
			Type: strategy.IntentGeneratePRD,
			Context: map[string]string{
				"title":         workTitle,
				"pain_point":    workDescription,
				"work_type":     workType,
				"codex_context": codexContext,
				"source":        intake.Source,
				"work_item_id":  workID.String(),
			},
			OutputFormat: "Markdown with sections: Overview, Problem Statement, Goals, Non-Goals, User Stories, Acceptance Criteria. Only include an Open Questions section if there are genuinely unresolvable unknowns requiring business owner input. Omit it for straightforward requests.",
		}
		prdResult, err := llmSvc.Execute(msgCtx, prdIntent)
		if err != nil {
			reason := fmt.Sprintf("PRD generation failed: %v. Check your LLM provider status and retry.", err)
			log.Printf("product-agent: generate prd failed for work_id=%s: %v", workID, err)
			if bErr := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); bErr != nil {
				log.Printf("product-agent: block work item: %v", bErr)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}
		// R1: store only the PRD body; a model preamble would be re-sent to
		// every research task.
		prdContent := stripPRDPreamble(prdResult.Content)
		if strings.TrimSpace(prdContent) == "" {
			reason := "LLM returned an empty PRD. The provider responded but produced no content — check rate limits, token budgets, and provider logs, then retry."
			log.Printf("product-agent: empty PRD for work_id=%s", workID)
			if err := guards.BlockWorkItem(msgCtx, wg, kb, workID, agentID, reason); err != nil {
				log.Printf("product-agent: block work item: %v", err)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		// Extract a concise title from the PRD content. The LLM-generated PRD
		// typically starts with "# <title>" or has an "## Overview" section.
		// Fall back to the first sentence of the description.
		prdTitle := extractTitle(prdContent, workDescription)

		// Update work item type based on classification.
		item, err := wg.GetWorkItem(msgCtx, workID)
		if err != nil {
			log.Printf("product-agent: get work item: %v", err)
		} else if workType == "bug" && item.Type != models.WorkItemTypeBug {
			item.Type = models.WorkItemTypeBug
			// Reclassification is stored via the artifact; type is immutable after creation.
		}

		// PRD artifact.
		artifactContent, err := json.Marshal(map[string]string{
			"prd":       prdContent,
			"work_type": workType,
		})
		if err != nil {
			log.Printf("product-agent: marshal prd artifact: %v", err)
			artifactContent = []byte(`{"error":"marshal failed"}`)
		}
		artifact := &models.WorkItemArtifact{
			WorkItemID: workID,
			Type:       models.ArtifactTypePRD,
			Content:    artifactContent,
			AgentID:    agentID,
		}

		// The user prompt for the unified interaction surface (bell + sheet).
		// The legacy ApprovalQueue page keeps working via its
		// /api/work-items?state=prd_approval poll. Dashboard-backend's
		// ApprovePRD / RejectPRD handlers mark the prompt answered.
		prompt := &interactions.Prompt{
			ID:          uuid.New(),
			WorkItemID:  workID,
			ProductID:   *productID,
			AskingAgent: "product-agent",
			Trigger:     interactions.TriggerApprovalPRD,
			PromptType:  interactions.PromptTypeApproval,
			Headline:    fmt.Sprintf("PRD ready: %s", prdTitle),
			Question:    prdContent,
			ContextLink: fmt.Sprintf("/approvals?work_item_id=%s", workID),
			ContextPayload: map[string]any{
				"prd_content": prdContent,
				"work_type":   workType,
			},
			Actions: []interactions.Action{
				{ID: "approve", Label: "Approve", Semantic: interactions.SemanticContinue},
				{ID: "reject", Label: "Reject", Semantic: interactions.SemanticCancel},
			},
			State:       interactions.StatePending,
			RequestedAt: time.Now().UTC(),
			PromptedVia: []string{"dashboard"},
		}

		// Human-in-the-loop gate: park the work item in prd_approval and wait
		// for an admin to approve. Do NOT publish prd.ready here — that
		// happens in the approve endpoint after a human has read the PRD and
		// confirmed it matches the intake. A hallucinated PRD cannot reach
		// the expensive research phase without a human saying yes.
		//
		// Title, artifact, transition, prompt and assignment completion
		// commit together. Before the outbox each was its own write and a
		// failed prompt publish was ignored, leaving an item in prd_approval
		// with no bell; now a failure leaves none of it and the item is
		// blocked with the cause.
		pub := interactions.NewPublisher(nil) // PublishTx never touches the bus
		//
		// M1-05: this transaction carries the processed-event guard row
		// (Wrap). Writes before it (assignment, the transition to prd, the
		// pooled guards.BlockWorkItem exits) are outside the guard;
		// Ledger.Handler's up-front Seen check keeps a recorded duplicate
		// from reaching them.
		err = commitPRD(msgCtx, idempotency.Wrap(msgCtx, pool), wg, pub, aggregateID, workID, prdTitle, artifact, prompt, assignment.ID, pins)
		if idempotency.IsDuplicate(err) {
			log.Printf("product-agent: work_id=%s intake already processed — duplicate delivery, PRD not recorded", workID)
			return nil
		}
		if err != nil {
			reason := fmt.Sprintf("PRD was generated but could not be recorded: %v. Retry the work item.", err)
			log.Printf("product-agent: commit prd for work_id=%s: %v", workID, err)
			if bErr := blockInOwnTx(msgCtx, idempotency.Wrap(msgCtx, pool), aggregateID, workID, reason); bErr != nil {
				log.Printf("product-agent: block work item: %v", bErr)
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			return nil
		}

		log.Printf("product-agent: completed work_id=%s work_type=%s (awaiting approval)", workID, workType)
		return nil
	}
}

// commitPRD records a generated PRD in one transaction: the title taken from
// it, the PRD artifact, the transition to prd_approval, the approval prompt
// (its user_interactions row plus the user_prompt.requested event) and the
// assignment completion. aggregateID orders the prompt event with the rest of
// the work item's events (the parent work item, when the item has one).
//
// M2-02: the repository base pins resolved at intake (and their resolved
// events) are written first, so the snapshot the PRD was approved against is
// recorded with it or not at all.
func commitPRD(
	ctx context.Context,
	db workgraph.TxBeginner,
	wg *workgraph.WorkGraph,
	pub *interactions.Publisher,
	aggregateID, workID uuid.UUID,
	title string,
	artifact *models.WorkItemArtifact,
	prompt *interactions.Prompt,
	assignmentID uuid.UUID,
	pins []resolvedPin,
) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		if err := writePinsTx(ctx, tx, workID, pins); err != nil {
			return err
		}
		if title != "" {
			if _, err := tx.Exec(ctx, updateTitleSQL, title, workID); err != nil {
				return fmt.Errorf("update title: %w", err)
			}
		}
		if err := wg.AddArtifactTx(ctx, tx, artifact); err != nil {
			return err
		}
		if _, err := wg.TransitionStateTx(ctx, tx, workID, models.WorkItemStatePRDApproval, agentID,
			"PRD generated — awaiting human approval before research dispatch"); err != nil {
			return fmt.Errorf("transition to prd_approval: %w", err)
		}
		if err := pub.PublishTx(ctx, tx, aggregateID, prompt); err != nil {
			return fmt.Errorf("prd approval prompt: %w", err)
		}
		return wg.CompleteAssignmentTx(ctx, tx, assignmentID, models.AssignmentStatusCompleted)
	})
}

// blockInOwnTx blocks the work item in a short transaction of its own:
// the transition to blocked and its work.blocked event commit together.
func blockInOwnTx(ctx context.Context, db workgraph.TxBeginner, aggregateID, workID uuid.UUID, reason string) error {
	return workgraph.WithTx(ctx, db, func(tx pgx.Tx) error {
		return guards.BlockWorkItemTx(ctx, tx, aggregateID, workID, agentID, reason)
	})
}

// resolveProduct returns the product the work item belongs to: the intake
// payload's product_id when it is a valid, non-nil id, otherwise the work
// item's own. nil means the item has no product.
func resolveProduct(intakeProductID string, wi *models.WorkItem) *uuid.UUID {
	if pid, err := uuid.Parse(intakeProductID); err == nil && pid != uuid.Nil {
		return &pid
	}
	if wi != nil && wi.ProductID != nil && *wi.ProductID != uuid.Nil {
		pid := *wi.ProductID
		return &pid
	}
	return nil
}

// aggregateFor is the outbox aggregate for a work item's events: its parent
// when it has one, otherwise the item itself (common3 outbox rule).
func aggregateFor(workID uuid.UUID, wi *models.WorkItem) uuid.UUID {
	if wi != nil && wi.ParentID != nil && *wi.ParentID != uuid.Nil {
		return *wi.ParentID
	}
	return workID
}

// extractTitle pulls a concise title from PRD content or falls back to the
// first sentence of the description. Keeps it under 100 characters.
func extractTitle(prdContent, description string) string {
	// Try the first markdown heading: "# Some Title"
	for _, line := range strings.Split(prdContent, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			title := strings.TrimPrefix(line, "# ")
			title = strings.TrimPrefix(title, "PRD: ")
			title = strings.TrimSpace(title)
			if len(title) > 100 {
				title = title[:100]
			}
			if title != "" {
				return title
			}
		}
	}
	// Fall back to first sentence of description.
	title := description
	if idx := strings.IndexAny(title, ".!\n"); idx > 0 && idx < 100 {
		title = title[:idx]
	} else if len(title) > 100 {
		title = title[:100]
	}
	return title
}

// clarityResult is the structured output of the pre-PRD classification call.
// The LLM is instructed to return pure JSON matching these fields; we extract
// the first JSON object from its response (tolerant of stray prose around it).
type clarityResult struct {
	IsClear   bool     `json:"is_clear"`
	WorkType  string   `json:"work_type"`
	Questions []string `json:"questions"`
}

// parseClarityResult pulls the first JSON object out of the LLM response.
// On parse failure it returns a conservative result (is_clear=true) so a
// malformed JSON doesn't wrongly block a legit work item — the human
// approval gate at prd_approval remains as the second line of defense.
func parseClarityResult(raw string) clarityResult {
	def := clarityResult{IsClear: true, WorkType: "feature"}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return def
	}
	var r clarityResult
	if err := json.Unmarshal([]byte(raw[start:end+1]), &r); err != nil {
		return def
	}
	// Guard against is_clear=false with empty questions (bad LLM output).
	// If the LLM wants to block without saying why, don't trust it.
	if !r.IsClear && len(r.Questions) == 0 {
		return def
	}
	if r.WorkType == "" {
		r.WorkType = "feature"
	}
	return r
}
