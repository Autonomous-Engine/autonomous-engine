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
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/gitref"
	"github.com/autonomousengine/autonomous-engine/internal/guards"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/vector"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "review-agent"
	consumerGroup = "review-agent"
	healthPort    = 8097

	// vectorCollection must match knowledge-indexer so codex_similar_code
	// queries the same dataset the indexer populated.
	vectorCollection = "codebase"
)

// prReadyMessage is received from pr.ready.
//
// ProductID is required for product-scoped codex grounding; older producers
// that omit it will still work, but review-agent will log and skip codex
// injection rather than pull in global/cross-product rows.
type prReadyMessage struct {
	WorkID        string   `json:"work_id"`
	ProductID     string   `json:"product_id,omitempty"`
	RepositoryID  string   `json:"repository_id,omitempty"`
	PRNumber      int      `json:"pr_number,omitempty"`
	PRURL         string   `json:"pr_url,omitempty"`
	PRDescription string   `json:"pr_description"`
	Branch        string   `json:"branch"`
	FilesChanged  []string `json:"files_changed"`
	Timestamp     string   `json:"timestamp"`

	// HeadSHA is the verified head SHA from the implementation evidence
	// (D1.5), set by pr-coordinator. When present, FilesChanged is the
	// git-derived list from that evidence and the review examines exactly
	// this commit; when absent (legacy producer) FilesChanged is the
	// planner's list and the EVIDENCE_COMPAT_WINDOW rules apply.
	HeadSHA string `json:"head_sha,omitempty"`
	// PartiallyVerified is true when the PR was opened on partially
	// verified evidence under the product's opt-in (D2, D3). Informational
	// here: the human-review gate is enforced by the status-orchestrator.
	PartiallyVerified bool `json:"partially_verified,omitempty"`
	// BaseSHA is the work item's pinned base commit for this repository
	// (M2-02); the review diffs against it (M2-03, pinned_base.go). Empty
	// for a legacy-unpinned item, which keeps origin/<base_branch>.
	BaseSHA string `json:"base_sha,omitempty"`
	// BaseDrift is set by pr-coordinator when the base branch moved after
	// the pin and the PR still merges cleanly onto the new tip (M2-03).
	BaseDrift *baseDrift `json:"base_drift,omitempty"`
	// Replay is set by pr-coordinator for a replay (shadow) work item
	// (M4-06): no PR exists; the review runs on Branch at the verified
	// HeadSHA against the pinned base, and nothing is posted to GitHub.
	Replay bool `json:"replay,omitempty"`
}

// reviewableWithoutPR reports whether a pr.ready without a PR may still be
// reviewed: only a replay review request with a branch and a verified head.
func reviewableWithoutPR(p prReadyMessage) bool {
	return p.Replay && p.Branch != "" && p.HeadSHA != ""
}

// reviewCompleteMessage is published to review.complete.
type reviewCompleteMessage struct {
	WorkID              string `json:"work_id"`
	Decision            string `json:"decision"`
	Feedback            string `json:"feedback,omitempty"`
	HumanReviewRequired bool   `json:"human_review_required"`
}

// humanReviewMessage is published to human-review.queue.
type humanReviewMessage struct {
	WorkID        string `json:"work_id"`
	Reason        string `json:"reason"`
	PRDescription string `json:"pr_description"`
}

// reviewCommentRequest matches the github-service POST /review-comment body.
type reviewCommentRequest struct {
	RepositoryID string `json:"repository_id"`
	PRNumber     int    `json:"pr_number"`
	Body         string `json:"body"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	CommitID     string `json:"commit_id"`
}

// reviewSystemPrompt is the second-pass (and, before M3-07, the only)
// review prompt. It lives in internal/prompts so cmd/verifier-eval runs the
// identical author-only baseline.
const reviewSystemPrompt = prompts.ReviewSystemPrompt

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("review-agent: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("review-agent: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("review-agent: connect kafka: %v", err)
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

	humanReviewRequired := cfg.System.HumanReviewRequired

	githubServiceURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubServiceURL == "" {
		githubServiceURL = "http://github-service:8085"
	}

	// D1.5 compatibility window for pr.ready messages without a verified
	// head_sha (legacy producers). Unknown values fall back to strict.
	window := evidence.CompatWindowFromEnv()
	log.Printf("review-agent: %s=%s", evidence.CompatWindowEnv, window)

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04): review.complete and work.blocked are
	// committed to outbox_events with the writes they announce, and this
	// loop carries them to Kafka. A start failure is fatal; one per process.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "review-agent")
	if err != nil {
		log.Fatalf("review-agent: start outbox dispatcher: %v", err)
	}
	log.Printf("review-agent: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M1-05: deduplicate on the processed-event ledger. The up-front Seen
	// check acknowledges a recorded duplicate before any git or LLM work.
	ledger := idempotency.NewLedger(pool)
	handler := ledger.Handler("review-agent", buildHandler(ctx, pool, wg, registry, cs, kb, humanReviewRequired, githubServiceURL, cx, window))
	if err := kb.Subscribe(bus.TopicPRReady, consumerGroup, handler); err != nil {
		log.Fatalf("review-agent: subscribe: %v", err)
	}

	// R14: the verification stage — a second consumer group on
	// implementation.complete (verification.go). It verifies each subtask's
	// verified head before any PR exists and announces
	// verification.complete through the outbox; its own processed-event
	// ledger name keeps its deliveries apart from the pr.ready consumer's.
	verification := ledger.Handler(verificationIdempotencyConsumer, buildVerificationHandler(ctx, verificationDeps{
		pool: pool, wg: wg, registry: registry, cs: cs, kb: kb, cx: cx, window: window, products: products.NewStore(pool),
	}))
	if err := kb.Subscribe(bus.TopicImplementationComplete, verificationConsumerGroup, verification); err != nil {
		log.Fatalf("review-agent: subscribe verification stage: %v", err)
	}
	log.Printf("review-agent: verification stage subscribed to %s (group %s)", bus.TopicImplementationComplete, verificationConsumerGroup)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("review-agent: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("review-agent: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("review-agent: shutting down...")
	cancel()
	// Let an in-flight outbox publish finish being recorded before exit.
	dispatcher.Wait()
	log.Println("review-agent: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("review-agent: health server shutdown error: %v", err)
	}
	log.Println("review-agent: stopped")
}

func buildHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	humanReviewRequired bool,
	githubServiceURL string,
	cx *codex.Codex,
	window evidence.CompatWindow,
) bus.Handler {
	logger := logging.New("review-agent")
	return func(msgCtx context.Context, msg *bus.Message) error {
		// Outcome writes and their events commit together (M1-04). They use
		// the process ctx so a cancelled message ctx cannot drop the event.
		// M1-05: the writer's transactions carry the processed-event guard
		// row (the first to commit records the event); Wrap captures this
		// delivery now, so the process ctx can still be used to commit.
		rw := reviewWriter{db: idempotency.Wrap(msgCtx, pool), wg: wg}
		var payload prReadyMessage
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("review-agent: unmarshal: %v", err)
			return nil
		}

		workID, err := uuid.Parse(payload.WorkID)
		if err != nil {
			log.Printf("review-agent: parse work_id %q: %v", payload.WorkID, err)
			return nil
		}
		// Validate once at entry: whitespace-only reads as absent and a
		// malformed value is dropped (logged) so it is never compared with
		// the remote tip or handed to git.
		payload.HeadSHA = sanitizeHeadSHA(payload.WorkID, payload.HeadSHA)

		// Attach work item ID to context for structured log correlation.
		msgCtx = logging.WithWorkItemID(msgCtx, workID.String())

		// Guard: never review a pr.ready event without a real PR URL.
		// pr-coordinator now refuses to publish without one, but belt-and-
		// suspenders: any older/replayed message that snuck through would
		// have the review-agent hallucinate feedback against an empty
		// branch (observed on 4/22 @ 19:33 — review_decision=request_changes
		// on a branch with zero commits, which restarted the impl loop).
		// M4-06: a replay review request has no PR by design; it is
		// reviewed when it names a branch and a verified head.
		if (payload.PRURL == "" || payload.PRNumber == 0) && !reviewableWithoutPR(payload) {
			log.Printf("review-agent: skipping pr.ready without a PR URL — work_id=%s branch=%s", workID, payload.Branch)
			return nil
		}

		logger.Info(msgCtx, "reviewing PR", map[string]interface{}{
			"work_id":            workID.String(),
			"branch":             payload.Branch,
			"pr_url":             payload.PRURL,
			"head_sha":           payload.HeadSHA,
			"partially_verified": payload.PartiallyVerified,
		})

		// Record agent assignment for this work item.
		assignment := &models.AgentAssignment{
			WorkItemID:      workID,
			AgentType:       models.AgentTypeReview,
			AgentInstanceID: agentID,
			Status:          models.AssignmentStatusActive,
		}
		if err := wg.AssignAgent(msgCtx, assignment); err != nil {
			log.Printf("review-agent: assign agent: %v", err)
		}

		// Resolve repository_id and clone_path.
		// If the prReadyMessage carries a repository_id, use it.
		// Otherwise, look up the work item from the DB.
		repositoryID := payload.RepositoryID
		if repositoryID == "" {
			workItem, err := wg.GetWorkItem(msgCtx, workID)
			if err != nil {
				log.Printf("review-agent: lookup work item %s: %v", workID, err)
			} else if workItem.RepositoryID != nil {
				repositoryID = workItem.RepositoryID.String()
			}
		}

		repo := resolveRepoInfo(msgCtx, pool, repositoryID)
		clonePath := repo.ClonePath

		// Validate that the repo is available.
		repoCheck := guards.CheckRepoAvailable(clonePath)
		if !repoCheck.Valid {
			log.Printf("review-agent: repo not available for work_id=%s: %s", workID, repoCheck.Message)
		}

		// Transition to review state.
		if err := wg.TransitionState(msgCtx, workID, models.WorkItemStateReview, agentID, "review-agent starting code review"); err != nil {
			log.Printf("review-agent: transition to review: %v", err)
		}

		// M3-07: the independent verifier's requirements and recorded
		// evidence (never the PR description or the implementation
		// summary). A read failure leaves them empty; the verifier then
		// has fewer criteria, never more trust.
		verReqs, verEvidence, loadErr := verifier.Load(msgCtx, pool, workID)
		if loadErr != nil {
			log.Printf("review-agent: work_id=%s: load verifier inputs: %v", workID, loadErr)
		}
		log.Printf("review-agent: work_id=%s verifier inputs: %d criteria, %d task specs, %d evidence records, %d runs",
			workID, len(verReqs.Criteria), len(verReqs.TaskSpecs), len(verEvidence.Records), len(verEvidence.Runs))

		// No clone: nothing can be verified. The verifier escalates with
		// every criterion not_verifiable (no model call) and the item goes
		// to human_review with the verdict recorded.
		if !repoCheck.Valid {
			in := verifier.Build(verifier.Sources{WorkID: workID, Requirements: verReqs, Evidence: verEvidence,
				Unavailable: "repository not available on disk: " + repoCheck.Message})
			v := verifier.Run(msgCtx, verifier.Runner{}, in)
			d := verifier.Gate(v)
			plan := planAfterVerifier(v, d, false, humanReviewRequired)
			// Nothing was examined (reviewed_sha empty), but the pr.ready's
			// verified head is the head a human approves from here (D1.5).
			art := verifierArtifact(workID, v, d, plan, payload.Branch, payload.HeadSHA, "")
			logger.Info(msgCtx, "verifier verdict", map[string]interface{}{"work_id": workID.String(), "action": string(d.Action), "reason": d.Reason})
			if payload.PRNumber > 0 && repositoryID != "" {
				postReviewToGitHub(msgCtx, githubServiceURL, repositoryID, payload.PRNumber, verifier.RenderComment(v, d))
			}
			rw.recordReview(ctx, workID, assignment.ID, art, *plan.decision)
			return nil
		}

		// Resolve LLM adapter per work item: checks agent_definition first, falls back to global.
		resolution, resolveErr := registry.Resolve(msgCtx, workID, cs, "review-agent", "instance-1")
		if resolveErr != nil {
			log.Printf("review-agent: resolve adapter for work_id=%s: %v", workID, resolveErr)
			reason := "Cannot create LLM adapter: provider not configured or credentials missing. Configure model credentials in Admin → Credentials."
			rw.block(ctx, workID, assignment.ID, reason) //nolint:errcheck // logged inside
			return nil
		}
		llmAdapter, agentDef := resolution.Adapter, resolution.Definition
		// M4-05: tag this review's inference rows (role reviewer, snapshot).
		msgCtx = llm.WithInferenceTags(msgCtx, llm.TagsForResolution(resolution, workID))

		// Resolve the product ID for product-scoped codex grounding. Prefer
		// the ID carried on the pr.ready message; when older producers omit
		// it, fall back to the work item's product_id. If neither source has
		// a product_id, skip codex injection rather than leak cross-product
		// rows via the global GetAntiPatterns path.
		var reviewProductID *uuid.UUID
		if payload.ProductID != "" {
			if pid, pidErr := uuid.Parse(payload.ProductID); pidErr == nil {
				reviewProductID = &pid
			}
		}
		if reviewProductID == nil {
			if workItem, wiErr := wg.GetWorkItem(msgCtx, workID); wiErr == nil && workItem != nil && workItem.ProductID != nil {
				pid := *workItem.ProductID
				reviewProductID = &pid
			}
		}

		// Resolve branch for codex-grounding lookups. Empty = main.
		branch := ""
		if wi, wiErr := wg.GetWorkItem(msgCtx, workID); wiErr == nil && wi != nil && wi.Branch != nil {
			branch = *wi.Branch
		}

		// Build Codex context for the prompt.
		codexContext := ""
		if cx != nil && reviewProductID != nil {
			var sb strings.Builder
			sb.WriteString("\n\n## PROJECT CODEX — Follow these patterns:\n\n")

			patterns, _ := cx.GetPatternsForProductBranch(msgCtx, *reviewProductID, branch)
			if len(patterns) > 0 {
				sb.WriteString("### Patterns to Follow:\n")
				for _, p := range patterns {
					sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", p.Name, p.Category, p.Rationale))
					for _, rule := range p.KeyRules {
						sb.WriteString(fmt.Sprintf("  Rule: %s\n", rule))
					}
				}
				sb.WriteString("\n")
			}

			antiPatterns, _ := cx.GetAntiPatternsForProductBranch(msgCtx, *reviewProductID, branch)
			if len(antiPatterns) > 0 {
				sb.WriteString("### Anti-Patterns to AVOID:\n")
				for _, ap := range antiPatterns {
					sb.WriteString(fmt.Sprintf("- %s: %s → Instead: %s\n", ap.Name, ap.WhyWrong, ap.WhatInstead))
				}
			}

			codexContext = sb.String()
		} else if cx != nil {
			log.Printf("review-agent: work_id=%s has no product_id — skipping codex grounding", workID)
		}

		// Default to request_changes. Approval requires an explicit APPROVE
		// signal from a successful, parsed LLM review — fail honestly instead
		// of auto-approving on any error path. Previously this was
		// "decision := \"approve\"" which meant a tool-loop error or empty
		// response silently merged unreviewed code.
		decision := "request_changes"
		feedback := ""
		// verdictDefects are the verifier's defect counts, repeated on the
		// second-pass artifact so the latest review_comments row always
		// carries them (work_item_outcomes.review_defects, M4-05).
		var verdictDefects verifier.DefectCounts

		// reviewedSHA is the commit the review actually examined: the
		// verified head SHA when the message carried one, the remote tip
		// under the legacy window, or empty when only the origin/<branch>
		// ref was available. verifiedSHA is the evidence-derived head SHA
		// only (empty for a legacy message); the review artifact records it
		// as head_sha so the status-orchestrator hands QA the same D1.5 SHA
		// and never promotes a legacy tip to "verified".
		reviewedSHA := ""
		verifiedSHA := ""

		if repoCheck.Valid {
			vectorStore := codexVectorStore(msgCtx, pool, cs)

			// Fetch the PR branch so git can resolve the three-dot diff
			// against it. The implementation agent pushed the branch to
			// origin but the main clone doesn't have it locally yet.
			if payload.Branch != "" {
				fetchCmd := exec.CommandContext(msgCtx, "git", "fetch", "origin", payload.Branch)
				fetchCmd.Dir = clonePath
				if fetchOut, fetchErr := fetchCmd.CombinedOutput(); fetchErr != nil {
					log.Printf("review-agent: git fetch origin %s: %v: %s", payload.Branch, fetchErr, string(fetchOut))
				}
			}

			// D1.5: review the verified head SHA, not whatever the branch
			// points at now. Ask the remote for the branch tip and compare;
			// a moved branch invalidates the evidence and blocks the item.
			tipSHA, tipErr := gitref.RemoteTip(msgCtx, clonePath, payload.Branch)
			if tipErr != nil {
				log.Printf("review-agent: resolve remote tip of %s for work_id=%s: %v", payload.Branch, workID, tipErr)
				if payload.HeadSHA != "" {
					reason := fmt.Sprintf("cannot verify that branch %s still points at verified head %s: %v", payload.Branch, payload.HeadSHA, tipErr)
					blockReview(ctx, rw, workID, assignment.ID, reason)
					return nil
				}
			}
			target := resolveReviewTarget(payload.HeadSHA, tipSHA, window)
			if target.Blocked {
				blockReview(ctx, rw, workID, assignment.ID, target.Reason)
				return nil
			}
			if target.Legacy {
				metrics.EvidenceCompatLegacyFallback.Inc()
				log.Printf("review-agent: WARNING work_id=%s branch=%s: %s", workID, payload.Branch, target.Reason)
			}
			if target.SHA != "" && !gitref.CommitPresent(msgCtx, clonePath, target.SHA) {
				reason := fmt.Sprintf("verified head %s of branch %s is not available in the local clone after fetch; cannot review it", target.SHA, payload.Branch)
				blockReview(ctx, rw, workID, assignment.ID, reason)
				return nil
			}
			reviewedSHA = target.SHA
			if !target.Legacy {
				verifiedSHA = target.SHA
			}

			// M2-03: diff against the pinned base commit (legacy-unpinned:
			// origin/<base_branch>). A pinned base the clone cannot produce
			// blocks rather than reviewing against a moving branch.
			rb, rbErr := resolveReviewBase(msgCtx, newReviewBaseDeps(pool, clonePath), workID, repositoryID, payload.BaseSHA, repo.BaseBranch)
			if rbErr != nil {
				blockReview(ctx, rw, workID, assignment.ID, rbErr.Error())
				return nil
			}

			// diffHead is the exact commit under review. Only a legacy
			// message whose remote tip could not be read falls back to the
			// origin/<branch> ref, as before this change.
			diffHead := reviewedSHA
			if diffHead == "" && payload.Branch != "" {
				diffHead = "origin/" + payload.Branch
			}

			// R14: the verification stage verified this head before the PR
			// existed; its stored verdict is reused (stage.ReusableVerdict).
			// Only when none exists for the verified head (a legacy message,
			// a head the stage never saw) does the review run the verifier
			// itself, as M3-07 did: its model resolved for agent type
			// "verifier" (falling back to this review's model, recorded as
			// shared), its calls tagged role=verifier, in a scratch
			// repository at the head under review, diffed against the same
			// pinned base.
			stored, why := storedVerdictFor(msgCtx, pool, workID, verifiedSHA)
			var verdict verifier.Verdict
			var gate verifier.Decision
			sharedModel := false
			if stored != nil {
				verdict, gate = stored.Content.Verdict, stored.Content.Gate
				sharedModel = stored.Content.VerifierModelSharedWithAuthor
				log.Printf("review-agent: work_id=%s reusing the verdict the verification stage recorded for head %s (%s, artifact %v); the verifier is not run again", workID, verifiedSHA, stored.Result.Decision, stored.Result.VerdictArtifactID)
			} else {
				log.Printf("review-agent: work_id=%s running the independent verifier on pr.ready: %s", workID, why)
				vrun, vCtx := newVerifierRun(ctx, msgCtx, pool, registry, cs, kb, cx, reviewProductID, workID, resolution)
				verdict = verifyAt(vCtx, vrun, workID, verReqs, verEvidence, clonePath, rb.Ref, rb.SHA, diffHead)
				gate = verifier.Gate(verdict)
				sharedModel = vrun.info.SharedWithAuthor
			}
			logger.Info(msgCtx, "verifier verdict", map[string]interface{}{
				"work_id": workID.String(), "action": string(gate.Action), "reason": gate.Reason,
				"criteria": len(verdict.PerCriterion), "defects": len(verdict.Defects),
				"model_shared_with_author": sharedModel, "reused": stored != nil,
			})
			if payload.PRNumber > 0 && repositoryID != "" {
				postReviewToGitHub(msgCtx, githubServiceURL, repositoryID, payload.PRNumber, verifier.RenderComment(verdict, gate))
			}
			vplan := planAfterVerifier(verdict, gate, secondPassEnabled(), humanReviewRequired)
			if stored != nil {
				// The verdict artifact is already committed (by the stage);
				// the outcome is recorded against it.
				if !vplan.secondPass {
					rw.recordReview(ctx, workID, assignment.ID, reusedVerdictArtifact(workID, *stored, vplan, payload.Branch, verifiedSHA, reviewedSHA), *vplan.decision)
					return nil
				}
			} else {
				verdictArtifact := verifierArtifact(workID, verdict, gate, vplan, payload.Branch, verifiedSHA, reviewedSHA)
				if !vplan.secondPass {
					rw.recordReview(ctx, workID, assignment.ID, verdictArtifact, *vplan.decision)
					return nil
				}
				// Approved: the LLM review is the second opinion. The
				// verdict commits with its outcome, or with the block if it
				// fails.
				rw.pending = append(rw.pending, verdictArtifact)
			}
			verdictDefects = verifier.CountDefects(verdict.Defects)

			executor := tools.NewExecutorWithCodex(clonePath, cx, vectorStore, reviewProductID, kb, &workID)
			executor.SetDiffBase(rb.Ref)
			if diffHead != "" {
				executor.SetDiffHead(diffHead)
			}

			// Scope codex queries to the work item's branch snapshot.
			if branch, headSHA := tools.ResolveWorkItemBranch(ctx, pool, workID); branch != "" {
				executor.SetWorkItemBranch(branch, headSHA)
			}

			// Run tool-assisted review against the real repo. With a
			// verified head SHA the file list is the evidence's git-derived
			// list; a legacy message only has the planner's guess, and the
			// prompt says so.
			userMessage := fmt.Sprintf(`Review the changes on branch: %s
%s
%s
PR description:
%s

%s

Work ID: %s
Repository path: %s

Use git_diff to examine the actual changes (it will automatically diff against the base branch). Only review the code that was actually changed in this PR — do not comment on pre-existing code outside the diff. Use read_file to understand context for changed lines. Produce a thorough review.`,
				payload.Branch,
				baseBranchLine(repo.BaseBranch, rb, payload.BaseDrift),
				headSHALine(reviewedSHA, target.Legacy),
				payload.PRDescription,
				filesChangedLine(payload.FilesChanged, payload.HeadSHA != ""),
				workID.String(),
				clonePath,
			)

			// Publish each tool call to Kafka so the dashboard's Live Activity
			// panel streams review progress in real time. Mirrors the pattern in
			// research-agent / implementation-agent. Without this, review phases
			// appear as a static "review" state with no visible activity.
			executor.SetCallback(toolStream(ctx, kb, workID.String(), "review-agent"))

			// Resolve operating mode to tier-adjust the prompt + iteration cap.
			operatingMode := models.OperatingModeStandard
			if agentDef != nil && agentDef.Provider != "" && agentDef.ModelID != "" {
				if mode, modeErr := registry.GetOperatingMode(msgCtx, agentDef.Provider, agentDef.ModelID); modeErr == nil {
					operatingMode = mode
				}
			}
			scaffold := strategy.PromptScaffolding(operatingMode)
			// Compose the reviewer preamble with codex context (if available).
			// Do NOT include the Q&A-side CodexFirstStrategy — its coverage
			// rule forces 4-surface verification that causes review loops.
			// Reviewers only need the codex tools for targeted lookups.
			reviewPrompt := reviewSystemPrompt + "\n\n" + codexContext
			if preamble := scaffold.PromptPreamble(); preamble != "" {
				reviewPrompt += "\n\n" + preamble
			}

			req := adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{Role: adapter.RoleSystem, Content: reviewPrompt},
					{Role: adapter.RoleUser, Content: userMessage},
				},
				Tools:       tools.ReviewTools(),
				ToolChoice:  "auto",
				MaxTokens:   8192,
				Temperature: 0.2,
			}

			logger.Info(msgCtx, "running tool loop", map[string]interface{}{
				"work_id": workID.String(),
			})

			maxIters := scaffold.MaxIterations
			if maxIters <= 0 {
				maxIters = 15
			}
			resp, err := tools.RunToolLoop(msgCtx, llmAdapter, req, executor, maxIters, pool, &workID)
			if err != nil {
				if errors.Is(err, tools.ErrToolCallingNotSupported) {
					log.Printf("review-agent: model does not support tool calling: %v", err)
					rw.block(ctx, workID, assignment.ID, //nolint:errcheck // logged inside
						"Cannot review: the configured LLM model does not support tool calling. "+
							"Code review requires tool calling to read the actual diff and source files. "+
							"Action needed: assign a tool-calling capable model to the review agent.")
					return nil
				}
				// Tool-loop errored (LLM timeout, adapter network error, OOM, etc.).
				// Fail honestly: block the work item. Previously this fell through
				// to decision=="approve" and auto-merged unreviewed code.
				logger.Error(msgCtx, "tool loop failed", err, map[string]interface{}{
					"work_id": workID.String(),
				})
				reason := fmt.Sprintf("Review failed: %v. A human must review this PR manually — the agent could not produce a review.", err)
				rw.block(ctx, workID, assignment.ID, reason) //nolint:errcheck // logged inside
				return nil
			}

			if resp == nil || strings.TrimSpace(resp.Content) == "" {
				// Empty response from the LLM is not an approval. Block for
				// human review rather than silently merging.
				reason := "Review agent returned empty content from the LLM — cannot auto-approve. A human must review this PR manually."
				log.Printf("review-agent: %s (work_id=%s)", reason, workID)
				rw.block(ctx, workID, assignment.ID, reason) //nolint:errcheck // logged inside
				return nil
			}

			// Reasoning models (qwen3, deepseek-r1) sometimes put their
			// actual review in the thinking field, leaving content with
			// just a brief chain-of-thought fragment. If the response is
			// too short to be a real review, nudge the model to produce
			// its full analysis. Cap retries at 3 to prevent infinite
			// retry loops that produce increasingly degraded output.
			const maxShortRetries = 3
			for shortRetry := 0; shortRetry < maxShortRetries && len(strings.TrimSpace(resp.Content)) < 200; shortRetry++ {
				log.Printf("review-agent: response too short (%d chars, retry %d/%d) — nudging for full review (work_id=%s)",
					len(resp.Content), shortRetry+1, maxShortRetries, workID)
				nudge := "Your response was too brief. You have read the diff and relevant files. " +
					"Now produce your COMPLETE code review. Include:\n" +
					"1. Summary of what this PR changes\n" +
					"2. Issues found (with file and line references)\n" +
					"3. Your verdict: either 'APPROVE' if the code is correct, or 'REQUEST_CHANGES' with specific feedback\n" +
					"Do not call any more tools. Write your full review now."
				req.Messages = append(req.Messages,
					adapter.ChatMessage{Role: adapter.RoleAssistant, Content: resp.Content},
					adapter.ChatMessage{Role: adapter.RoleUser, Content: nudge},
				)
				nudgeResp, nudgeErr := tools.RunToolLoop(msgCtx, llmAdapter, req, executor, 3, pool, &workID)
				if nudgeErr == nil && nudgeResp != nil && len(strings.TrimSpace(nudgeResp.Content)) > len(resp.Content) {
					resp = nudgeResp
				} else {
					break // nudge didn't help, stop retrying
				}
			}

			// ── Bug 18: Malformed review output detection ────────────
			// After retries the model sometimes produces a raw source
			// code dump instead of structured review feedback. Detect
			// this and replace with a safe fallback so downstream
			// agents don't try to implement nonsensical instructions.
			feedback = resp.Content
			feedback = validateReviewOutput(feedback, workID.String())
			lc := lowercaseStr(feedback)
			// Approval requires an explicit APPROVE signal. Any hint of
			// changes needed keeps the default request_changes verdict.
			if strContains(lc, "request_changes") || strContains(lc, "changes requested") ||
				strContains(lc, "needs changes") || strContains(lc, "blocking issue") {
				decision = "request_changes"
			} else if strContains(lc, "decision: approve") || strContains(lc, "decision:approve") ||
				strContains(lc, "\napprove\n") || strContains(lc, "**approve**") ||
				strContains(lc, "verdict: approve") || strContains(lc, "verdict:approve") ||
				strContains(lc, "i approve") ||
				strContains(lc, "lgtm") || strContains(lc, "looks good to me") {
				decision = "approve"
			}
			// If neither signal is present, we keep the safe default
			// request_changes — do not auto-approve on ambiguous output.

			// ── Completeness gate ─────────────────────────────────────
			// Even if the LLM said APPROVE, reject if the diff is clearly
			// incomplete relative to the task. Get the actual changed files
			// from git and compare against what the task title implies.
			if decision == "approve" {
				if diffHead == "" {
					diffHead = "HEAD"
				}
				diffBase := rb.Ref
				statCmd := exec.CommandContext(msgCtx, "git", "diff", "--name-only", diffBase+"..."+diffHead)
				statCmd.Dir = clonePath
				statOut, statErr := statCmd.CombinedOutput()
				if statErr == nil {
					changedFiles := []string{}
					for _, line := range strings.Split(strings.TrimSpace(string(statOut)), "\n") {
						f := strings.TrimSpace(line)
						if f != "" {
							changedFiles = append(changedFiles, f)
						}
					}

					// Fetch the parent work item title for completeness check.
					wi, wiErr := wg.GetWorkItem(msgCtx, workID)
					taskTitle := ""
					if wiErr == nil && wi != nil {
						taskTitle = strings.ToLower(wi.Title)
					}

					// If the task title implies user-visible changes, check
					// that at least one frontend file was touched.
					uiSignals := []string{
						"display", "show", "add to page", "on page",
						"on the page", "frontend", "contact page",
						"ui ", "button", "form", "component",
					}
					taskImpliesUI := false
					for _, sig := range uiSignals {
						if strings.Contains(taskTitle, sig) {
							taskImpliesUI = true
							break
						}
					}

					if taskImpliesUI {
						frontendExts := []string{".tsx", ".jsx", ".vue", ".svelte", ".html", ".css", ".scss"}
						hasFrontendChange := false
						for _, cf := range changedFiles {
							cfLower := strings.ToLower(cf)
							for _, ext := range frontendExts {
								if strings.HasSuffix(cfLower, ext) {
									hasFrontendChange = true
									break
								}
							}
							if hasFrontendChange {
								break
							}
						}

						if !hasFrontendChange {
							feedback += "\n\n---\n[AUTOMATED NOTE]\n" +
								"The task title implies user-visible changes (\"" + wi.Title + "\") " +
								"but the diff contains zero frontend/UI files. " +
								"Files changed: " + strings.Join(changedFiles, ", ") + ". " +
								"If frontend changes are handled in a separate subtask, this may be fine."
							log.Printf("review-agent: completeness note added (no frontend files for UI task, work_id=%s) — not overriding decision", workID)
						}
					}
				}
			}

			// Post review comments to GitHub if we have a PR number.
			if payload.PRNumber > 0 && repositoryID != "" {
				postReviewToGitHub(msgCtx, githubServiceURL, repositoryID, payload.PRNumber, feedback)
			}
		}
		// (A missing repository never reaches here: the verifier escalated
		// it to human_review above.)

		// The review artifact, the verifier's verdict artifact (pending),
		// the human_review transition or the review.complete event, and the
		// assignment completion commit in one transaction (M1-04, M3-07).
		reviewArtifact := &models.WorkItemArtifact{
			WorkItemID: workID,
			Type:       models.ArtifactTypeReviewComments,
			Content: mustMarshal(map[string]any{
				"kind": artifactKindSecondPass, "decision": decision, "feedback": feedback,
				"branch": payload.Branch, "head_sha": verifiedSHA, "reviewed_sha": reviewedSHA,
				"defects": verdictDefects,
			}),
			AgentID: agentID,
		}

		logger.Info(msgCtx, "review decision", map[string]interface{}{
			"decision": decision,
			"work_id":  workID.String(),
		})

		rw.recordReview(ctx, workID, assignment.ID, reviewArtifact, reviewDecision{
			approve:             decision == "approve",
			humanReviewRequired: humanReviewRequired,
			feedback:            feedback,
		})
		return nil
	}
}

// codexVectorStore builds the VectorStore for codex_similar_code. Query-time
// vectors MUST come from the same embedding provider + model the
// knowledge-indexer wrote with: the default embedding_providers row via the
// configstore. If that lookup fails (no provider configured or no master
// key) codex_similar_code is disabled and the model falls back to
// search_code. Nil without QDRANT_URL.
func codexVectorStore(ctx context.Context, pool *pgxpool.Pool, cs *configstore.ConfigStore) *vector.VectorStore {
	qdrantURL := os.Getenv("QDRANT_URL")
	switch {
	case qdrantURL == "":
		return nil
	case cs == nil:
		return vector.New(qdrantURL, vectorCollection)
	}
	embSvc, _, embErr := vector.BuildEmbeddingService(ctx, pool, cs)
	if embErr != nil {
		log.Printf("review-agent: embedding provider unavailable — codex_similar_code disabled: %v", embErr)
		return vector.New(qdrantURL, vectorCollection)
	}
	return vector.NewWithEmbeddings(qdrantURL, vectorCollection, embSvc)
}

// sanitizeHeadSHA returns the normalised head_sha when it is a well-formed
// object name, else "" (logging the drop when the value was non-empty).
func sanitizeHeadSHA(workID, raw string) string {
	sha, ok := gitref.Sanitize(raw)
	if !ok {
		log.Printf("review-agent: WARNING work_id=%s pr.ready head_sha %q is not a well-formed object name — ignoring it", workID, raw)
	}
	return sha
}

// resolveReviewTarget applies gitref.ResolveTarget (the shared D1.5 rule)
// with review-agent's policy: a branch that is gone from the remote blocks
// the review outright — there is no PR branch left to review — whereas QA
// may still accept a verified SHA that a merge keeps reachable.
func resolveReviewTarget(headSHA, tipSHA string, window evidence.CompatWindow) gitref.Resolution {
	return gitref.ResolveTarget("pr.ready", headSHA, tipSHA, window)
}

// blockReview blocks the work item with reason and fails the assignment. It
// is the single exit for every "must not review" path.
func blockReview(ctx context.Context, rw reviewWriter, workID, assignmentID uuid.UUID, reason string) {
	log.Printf("review-agent: BLOCKED work_id=%s: %s", workID, reason)
	rw.block(ctx, workID, assignmentID, reason) //nolint:errcheck // logged inside
}

// headSHALine names the commit under review in the prompt, or says that
// only the branch tip was available.
func headSHALine(sha string, legacy bool) string {
	switch {
	case sha != "" && !legacy:
		return "Verified head SHA under review: " + sha + " (the diff is pinned to this commit)\n"
	case sha != "":
		return "Branch tip under review: " + sha + " (legacy message: no verified head SHA)\n"
	default:
		return ""
	}
}

// filesChangedLine labels the file list by provenance so the reviewer does
// not treat a planner guess as git truth.
func filesChangedLine(files []string, fromEvidence bool) string {
	if fromEvidence {
		return fmt.Sprintf("Files changed (git-derived, from verification evidence): %v", files)
	}
	return fmt.Sprintf("Files changed (from legacy message; not git-verified): %v", files)
}

type repoInfo struct {
	ClonePath  string
	BaseBranch string
}

// resolveRepoInfo looks up clone_path and base_branch for a repository.
// Falls back to /workspace/repo and "main" if the ID is empty or not found.
func resolveRepoInfo(ctx context.Context, pool *pgxpool.Pool, repositoryID string) repoInfo {
	if repositoryID == "" {
		return repoInfo{ClonePath: "/workspace/repo", BaseBranch: "main"}
	}
	var clonePath, baseBranch string
	err := pool.QueryRow(ctx,
		`SELECT clone_path, COALESCE(base_branch, 'main') FROM repositories WHERE id = $1`, repositoryID,
	).Scan(&clonePath, &baseBranch)
	if err != nil || clonePath == "" {
		log.Printf("review-agent: lookup repo info for %s: %v — using defaults", repositoryID, err)
		return repoInfo{ClonePath: "/workspace/repo", BaseBranch: "main"}
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	return repoInfo{ClonePath: clonePath, BaseBranch: baseBranch}
}

// postReviewToGitHub posts a top-level review comment to the github-service.
// It uses the /review-comment endpoint with no specific path/line (line=0) to
// post the review body as a general PR comment.
func postReviewToGitHub(ctx context.Context, githubServiceURL, repositoryID string, prNumber int, feedback string) {
	commentReq := reviewCommentRequest{
		RepositoryID: repositoryID,
		PRNumber:     prNumber,
		Body:         feedback,
		Path:         "",
		Line:         0,
		CommitID:     "",
	}
	data, _ := json.Marshal(commentReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		githubServiceURL+"/review-comment", bytes.NewReader(data))
	if err != nil {
		log.Printf("review-agent: build review-comment request: %v", err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("review-agent: post review comment to github-service: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("review-agent: github-service /review-comment returned %d", resp.StatusCode)
	}
}

// validateReviewOutput checks that the review feedback looks like structured
// review output (containing SUMMARY and/or DECISION sections) rather than a
// raw source-code dump. After multiple short-response retries the LLM
// sometimes emits a code block or import statements instead of review
// comments. When malformed output is detected, it is replaced with a safe
// fallback message so downstream agents don't try to implement nonsensical
// "review feedback" that is actually source code.
func validateReviewOutput(feedback, workID string) string {
	trimmed := strings.TrimSpace(feedback)
	if trimmed == "" {
		return feedback
	}

	lc := lowercaseStr(trimmed)

	// Check for expected review structure markers.
	hasStructure := strContains(lc, "summary") ||
		strContains(lc, "decision") ||
		strContains(lc, "approve") ||
		strContains(lc, "request_changes") ||
		strContains(lc, "changes requested") ||
		strContains(lc, "lgtm") ||
		strContains(lc, "looks good")

	if hasStructure {
		return feedback
	}

	// If no structural markers, check if it looks like source code.
	codeSignals := 0

	// Starts with a code fence
	if strings.HasPrefix(trimmed, "```") {
		codeSignals += 2
	}

	// Count code-like lines: import statements, function defs, brackets, etc.
	lines := strings.Split(trimmed, "\n")
	codeLines := 0
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "import ") || strings.HasPrefix(l, "from ") ||
			strings.HasPrefix(l, "package ") || strings.HasPrefix(l, "func ") ||
			strings.HasPrefix(l, "const ") || strings.HasPrefix(l, "var ") ||
			strings.HasPrefix(l, "export ") || strings.HasPrefix(l, "module.exports") ||
			strings.HasPrefix(l, "require(") || strings.HasPrefix(l, "class ") ||
			strings.HasPrefix(l, "def ") || strings.HasPrefix(l, "#include") ||
			strings.HasPrefix(l, "src/") {
			codeLines++
		}
	}

	// If more than 30% of lines look like code, it's probably a code dump.
	if len(lines) > 0 && float64(codeLines)/float64(len(lines)) > 0.3 {
		codeSignals += 2
	}

	// Lots of code fences in the output
	fenceCount := strings.Count(trimmed, "```")
	if fenceCount >= 4 {
		codeSignals++
	}

	if codeSignals >= 2 {
		log.Printf("review-agent: malformed review output detected (code dump, %d code signals) — using fallback (work_id=%s)", codeSignals, workID)
		return "Review produced malformed output (source code dump instead of structured feedback). " +
			"REQUEST_CHANGES: this PR requires re-review. The automated review was unable to produce " +
			"a valid structured assessment. A human reviewer or a retry is needed."
	}

	return feedback
}

func lowercaseStr(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b[i] = c
	}
	return string(b)
}

func strContains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return data
}

// truncate cuts a string to n runes, appending "…" if cut. Used to keep
// tool-call result previews sane in the dashboard WebSocket stream — full
// results can be thousands of characters and the UI only shows a snippet.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
