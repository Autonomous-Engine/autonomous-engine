package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/health"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/autonomousengine/autonomous-engine/internal/verifier/stage"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	agentID       = "pr-coordinator"
	consumerGroup = "pr-coordinator"
	// idempotencyConsumer is this service's name in processed_events.
	idempotencyConsumer = "pr-coordinator"
	healthPort          = 8096
)

// implementationCompletePayload is received from implementation.complete.
type implementationCompletePayload struct {
	TaskID       string   `json:"task_id"`
	WorkID       string   `json:"work_id"`
	FilesChanged []string `json:"files_changed"`
	TestResults  string   `json:"test_results"`
	Summary      string   `json:"summary,omitempty"`
	Success      bool     `json:"success"`
	Reason       string   `json:"reason,omitempty"`

	// Evidence is the additive v1 evidence.ImplementationEvidence object
	// (M0-07). It is kept raw so that a malformed value blocks with a reason
	// instead of failing the whole message decode; decideSubtaskPR judges it
	// through evidence.ReleaseDecisionFromJSON and takes the changed files
	// and verified head SHA from it in preference to FilesChanged (D1.4,
	// D1.5). Absent (or JSON null) means a pre-evidence producer: the
	// EVIDENCE_COMPAT_WINDOW rules apply and Success is the fallback.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// prReadyMessage is published to pr.ready.
//
// ProductID is load-bearing for review-agent: without it, the reviewer
// cannot scope the Codex grounding it injects into its LLM prompt and would
// either leak anti-patterns across products or — worse — receive no codex
// context at all (see audit cluster F / review-agent read path).
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

	// HeadSHA is the exact verified head SHA from the implementation
	// evidence (D1.5), empty for a legacy message. Additive.
	HeadSHA string `json:"head_sha,omitempty"`
	// PartiallyVerified is true when the PR was opened on partially
	// verified evidence under the product's opt-in and therefore requires
	// human review (D2, D3). Additive and informational: no consumer reads
	// it yet. The gate that stops auto-merge is the status-orchestrator's
	// parent human-review gate on the subtask's partially-verified pass
	// transition, not this flag.
	PartiallyVerified bool `json:"partially_verified,omitempty"`
	// BaseSHA is the repository's pinned base commit for the work item
	// (M2-02), empty when it is legacy-unpinned. review-agent diffs against
	// it (M2-03).
	BaseSHA string `json:"base_sha,omitempty"`
	// BaseDrift is set when origin's base branch moved after the pin and
	// the PR head still merges cleanly onto the new tip (M2-03, drift.go).
	// A conflicting drift blocks the parent instead of reaching pr.ready.
	BaseDrift *baseDrift `json:"base_drift,omitempty"`
	// Replay marks a PR-less review request for a replay (shadow) work
	// item (M4-06): review-agent reviews Branch at HeadSHA against the
	// pinned base; no PR exists and none is opened. Additive.
	Replay bool `json:"replay,omitempty"`
	// ContractsChecked are the PR-time contract checks of a consumer
	// subtask's PR (M2-06, contracts.go): one result per contract it
	// consumes (ok or unchecked; a mismatch blocks instead of reaching
	// pr.ready). Absent when the subtask consumes no contract.
	ContractsChecked []contractChecked `json:"contracts_checked,omitempty"`
}

// createPRRequest matches the github-service POST /pr body.
type createPRRequest struct {
	RepositoryID string `json:"repository_id"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Head         string `json:"head"`
	Base         string `json:"base"`
}

// createPRResponse is the github-service POST /pr response.
type createPRResponse struct {
	Success  bool   `json:"success"`
	PRURL    string `json:"pr_url,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	Message  string `json:"message,omitempty"`
}

// prParentRefused are the parent work item states pr-coordinator refuses to
// open a PR in (M1-05 / roadmap 1.2 precondition). It is a deny-list, not an
// allow-list: status-orchestrator moves the parent concurrently on the same
// implementation.complete, and depending on the agent definition it can be
// anywhere from planning to review, human_review, qa, or even passed /
// released when every post-implementation step is disabled — all of which
// still need their PR. An item paused in awaiting_user is judged by the
// state it paused in.
var prParentRefused = []models.WorkItemState{
	models.WorkItemStateCancelled,
	models.WorkItemStateBlocked,
	models.WorkItemStateFailed,
	models.WorkItemStateRejected,
}

// workSession tracks completion state for a given work item.
type workSession struct {
	workID uuid.UUID
	// outcomes holds the per-subtask PR decision keyed by task id; the
	// latest implementation.complete for a task wins.
	outcomes        map[string]subtaskOutcome
	expectedTaskIDs map[string]bool
	prPublished     bool
	// prRound is the round start prPublished latched for.
	prRound time.Time
	mu      sync.Mutex
}

// tracker manages per-work-item sessions.
type tracker struct {
	sessions map[string]*workSession
	mu       sync.Mutex
}

func newTracker() *tracker {
	return &tracker{sessions: make(map[string]*workSession)}
}

func (t *tracker) getOrCreate(workID uuid.UUID) *workSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := workID.String()
	if s, ok := t.sessions[key]; ok {
		return s
	}
	s := &workSession{
		workID:          workID,
		outcomes:        make(map[string]subtaskOutcome),
		expectedTaskIDs: make(map[string]bool),
	}
	t.sessions[key] = s
	return s
}

func (t *tracker) remove(workID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, workID.String())
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("pr-coordinator: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("pr-coordinator: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("pr-coordinator: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
	// The product store supplies the D2 partial-verification opt-in; the
	// evidence store is the restart fallback for a passed subtask whose
	// implementation.complete event was not seen; the compatibility window
	// governs messages that carry no evidence.
	productStore := products.NewStore(pool)
	evidenceStore := evidence.NewStore(pool)
	window := evidence.CompatWindowFromEnv()
	log.Printf("pr-coordinator: %s=%s", evidence.CompatWindowEnv, window)

	githubServiceURL := os.Getenv("GITHUB_SERVICE_URL")
	if githubServiceURL == "" {
		githubServiceURL = "http://github-service:8085"
	}

	t := newTracker()

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// The outbox dispatcher is the only thing in this process that puts
	// pr.gated, pr.ready and the pr-gate prompt on Kafka: the handler
	// enqueues them in the same transaction as the rows they describe. A
	// dispatcher that does not start means committed events nobody drains,
	// so it is fatal. Migrations 046 (pending_pr_changes.head_sha) and 047
	// (outbox_events) must be applied first (status-orchestrator).
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "pr-coordinator")
	if err != nil {
		log.Fatalf("pr-coordinator: start outbox dispatcher: %v", err)
	}
	log.Printf("pr-coordinator: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M1-05: deduplicate on the processed-event ledger (see the spec).
	// R14: the PR waits for the verification stage. pr-coordinator consumes
	// verification.complete (which carries the implementation.complete
	// payload verbatim) and no longer acts on implementation.complete.
	handler := idempotency.NewLedger(pool).Handler(idempotencyConsumer,
		buildHandler(wg, interactions.NewPublisher(nil), t, githubServiceURL, productStore, evidenceStore, window))
	if err := kb.Subscribe(bus.TopicVerificationComplete, consumerGroup, handler); err != nil {
		log.Fatalf("pr-coordinator: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("pr-coordinator: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("pr-coordinator: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("pr-coordinator: shutting down...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("pr-coordinator: health server shutdown error: %v", err)
	}
	// Let an in-flight outbox publish finish being recorded before exit.
	dispatcher.Wait()
	log.Println("pr-coordinator: stopped")
}

func buildHandler(
	wg *workgraph.WorkGraph,
	prompts *interactions.Publisher,
	t *tracker,
	githubServiceURL string,
	productStore products.PolicyStore,
	evidenceStore storedEvidenceLoader,
	window evidence.CompatWindow,
) bus.Handler {
	return func(msgCtx context.Context, msg *bus.Message) error {
		var vmsg stage.Message
		if err := json.Unmarshal(msg.Value, &vmsg); err != nil {
			log.Printf("pr-coordinator: unmarshal verification.complete: %v", err)
			return nil
		}
		payload := payloadFromVerification(vmsg)

		workID, err := uuid.Parse(payload.WorkID)
		if err != nil {
			log.Printf("pr-coordinator: parse work_id %q: %v", payload.WorkID, err)
			return nil
		}

		// Record agent assignment for this work item.
		assignment := &models.AgentAssignment{
			WorkItemID:      workID,
			AgentType:       models.AgentTypePRCoordinator,
			AgentInstanceID: agentID,
			Status:          models.AssignmentStatusActive,
		}
		if err := wg.AssignAgent(msgCtx, assignment); err != nil {
			log.Printf("pr-coordinator: assign agent: %v", err)
		}

		// Decide this subtask from its evidence under the product's D2
		// policy. The decision is recorded now (not when the PR is built)
		// so a later sibling's message cannot change it.
		if !payload.hasEvidence() {
			if window == evidence.CompatWindowStrict {
				log.Printf("pr-coordinator: implementation.complete: work_item=%s task=%s carries no evidence — no PR (%s=strict)", workID, payload.TaskID, evidence.CompatWindowEnv)
			} else {
				metrics.EvidenceCompatLegacyFallback.Inc()
				log.Printf("pr-coordinator: implementation.complete: WARNING work_item=%s task=%s carries no evidence — falling back to legacy success=%t (%s=legacy)", workID, payload.TaskID, payload.Success, evidence.CompatWindowEnv)
			}
		}
		// The product policy is only consulted for partially_verified
		// evidence; any lookup failure yields the zero policy ("not opted
		// in"), which can only make the gate stricter.
		policy, repositoryID := loadPolicyForTask(msgCtx, wg, productStore, payload.TaskID, workID)
		outcome := decideSubtaskPRFor(payload, policy, window, repositoryID)
		// R14: the verifier's decision for this head, on top of the
		// evidence decision.
		outcome = applyVerification(outcome, verificationFromMessage(vmsg))
		log.Printf("pr-coordinator: verification.complete work_id=%s task=%s head=%s decision=%s: %s", workID, payload.TaskID, vmsg.HeadSHA, vmsg.Decision, vmsg.Reason)
		switch outcome.Action {
		case evidence.ActionRelease:
			log.Printf("pr-coordinator: subtask %s on work_id=%s eligible for PR (%s): %s", payload.TaskID, workID, outcome.Label, outcome.Reason)
		case evidence.ActionHumanReview:
			log.Printf("pr-coordinator: subtask %s on work_id=%s eligible for PR marked %s: %s", payload.TaskID, workID, humanReviewBodyMarker([]subtaskOutcome{outcome}), outcome.Reason)
		default:
			log.Printf("pr-coordinator: subtask %s on work_id=%s gets no PR (%s): %s — checking if all siblings are terminal", payload.TaskID, workID, outcome.Label, outcome.Reason)
		}

		session := t.getOrCreate(workID)
		session.mu.Lock()
		session.outcomes[payload.TaskID] = outcome
		session.expectedTaskIDs[payload.TaskID] = true
		eligibleCount := 0
		for _, o := range session.outcomes {
			if o.Action != evidence.ActionBlock {
				eligibleCount++
			}
		}
		session.mu.Unlock()

		// Codex coherence Part 1 review: no PR while the parent is being
		// planned again or a codex re-plan escalation awaits its answer
		// ("Continue as is" re-enqueues the latest verification.complete).
		if hold := codexReplanHold(msgCtx, wg, workID); hold != "" {
			log.Printf("pr-coordinator: work_id=%s NOT opening PRs yet: %s", workID, hold)
			return nil
		}

		// Check if all children of this work item are complete.
		children, err := wg.GetChildren(msgCtx, workID)
		if err != nil {
			log.Printf("pr-coordinator: get children for work_id=%s: %v", workID, err)
			// Proceed without waiting for all children.
		}
		// Only the subtasks of the parent's current round count (codex
		// coherence Part 1 review): not those a codex re-plan superseded or
		// replaced, nor those that passed in an earlier round.
		hadSubtasks := false
		for _, c := range children {
			if c.Type == models.WorkItemTypeSubtask {
				hadSubtasks = true
				break
			}
		}
		round := loadRoundView(msgCtx, wg, workID, children)
		children = round.inRound(children)

		// Only count subtask children — research and other types have their own lifecycle.
		// Check the DB state of each subtask, AND treat any subtask whose
		// implementation.complete event we've already received as "done"
		// even if its DB row hasn't flipped to 'passed' yet. Without that
		// second clause we'd lose a race: the Kafka event can arrive before
		// the status-orchestrator commits the subtask's state transition,
		// leaving the coordinator stuck at N-1/N and never creating the PR.
		// The session's expectedTaskIDs is populated at line ~221 on every
		// implementation.complete received, so it's authoritative for
		// "events we've seen" regardless of DB commit ordering.
		session.mu.Lock()
		seenTasks := make(map[string]bool, len(session.expectedTaskIDs))
		for tid := range session.expectedTaskIDs {
			seenTasks[tid] = true
		}
		session.mu.Unlock()
		// R14: a passed subtask is done for the PR only once it is verified:
		// its verification.complete was seen, or (after a restart) a
		// verification_results row records its decision.
		verified := verifiedSubtasks(msgCtx, wg, workID)
		subtaskDone := func(c *models.WorkItem) bool {
			switch c.State {
			case models.WorkItemStateCancelled, models.WorkItemStateFailed, models.WorkItemStateBlocked:
				return true
			}
			return seenTasks[c.ID.String()] || verified[c.ID.String()]
		}

		subtaskCount := 0
		doneCount := 0
		for _, c := range children {
			if c.Type != models.WorkItemTypeSubtask {
				continue
			}
			subtaskCount++
			if subtaskDone(c) {
				doneCount++
			}
		}
		if hadSubtasks && subtaskCount == 0 {
			log.Printf("pr-coordinator: work_id=%s: no subtask of the current round yet (the earlier ones were superseded or belong to an earlier round) — waiting", workID)
			return nil
		}
		// Log child states for debugging container-restart scenarios
		if subtaskCount > 0 {
			childStates := make([]string, 0, subtaskCount)
			for _, c := range children {
				if c.Type != models.WorkItemTypeSubtask {
					continue
				}
				seen := seenTasks[c.ID.String()]
				childStates = append(childStates, fmt.Sprintf("%s=%s(seen=%v)", c.ID.String()[:8], c.State, seen))
			}
			log.Printf("pr-coordinator: work_id=%s subtask states: %s (%d/%d done)", workID, strings.Join(childStates, ", "), doneCount, subtaskCount)
		}

		if subtaskCount > 0 && doneCount < subtaskCount {
			// Re-check after a short delay — status-orchestrator may still be
			// committing state transitions for other subtasks.
			time.Sleep(3 * time.Second)
			children2, err2 := wg.GetChildren(msgCtx, workID)
			if err2 == nil {
				children2 = loadRoundView(msgCtx, wg, workID, children2).inRound(children2)
				doneCount2 := 0
				subtaskCount2 := 0
				verified = verifiedSubtasks(msgCtx, wg, workID)
				for _, c := range children2 {
					if c.Type != models.WorkItemTypeSubtask {
						continue
					}
					subtaskCount2++
					if subtaskDone(c) {
						doneCount2++
					}
				}
				if subtaskCount2 > 0 && doneCount2 >= subtaskCount2 {
					log.Printf("pr-coordinator: work_id=%s all %d subtasks done after re-check", workID, subtaskCount2)
					doneCount = doneCount2
					subtaskCount = subtaskCount2
					// The re-read is the current picture: a subtask that
					// became blocked meanwhile must not be judged (by the
					// all-or-nothing check or the PR loop) from the first,
					// stale read.
					children = children2
				} else {
					log.Printf("pr-coordinator: work_id=%s waiting: %d/%d subtasks complete (after re-check)", workID, doneCount2, subtaskCount2)
					return nil
				}
			} else {
				log.Printf("pr-coordinator: work_id=%s waiting: %d/%d subtasks complete", workID, doneCount, subtaskCount)
				return nil
			}
		}

		// R14 all-or-nothing (owner ruling): an escalated, rejected or
		// unverified subtask of this round stops every sibling's PR and
		// pending change; status-orchestrator moves the parent.
		if refusal := allOrNothingCheck(msgCtx, wg, productStore, evidenceStore, session, children, workID); refusal != "" {
			log.Printf("pr-coordinator: work_id=%s NOT opening PRs: %s", workID, refusal)
			if terminal, err := rollout.IsShadowTerminal(msgCtx, wg.DB(), workID); err != nil {
				return fmt.Errorf("replay check for work_id=%s: %w", workID, err)
			} else if terminal {
				return recordReplayComplete(msgCtx, idempotency.Wrap(msgCtx, wg.DB()), workID, "not every subtask was approved by the verifier")
			}
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusCompleted) //nolint:errcheck
			return nil
		}

		if eligibleCount == 0 {
			log.Printf("pr-coordinator: all subtasks terminal for work_id=%s but none verified or partially verified with opt-in — skipping PR creation", workID)
			// M4-06: a replay item must still terminate deterministically.
			if terminal, err := rollout.IsShadowTerminal(msgCtx, wg.DB(), workID); err != nil {
				return fmt.Errorf("replay check for work_id=%s: %w", workID, err)
			} else if terminal {
				// F2: a failed commit is retried in process, then the item
				// is blocked; an error means the block failed too.
				return recordReplayComplete(msgCtx, idempotency.Wrap(msgCtx, wg.DB()), workID, "no verified subtask")
			}
			return nil
		}

		// The latch is per round: a later round (a fix, or a codex re-plan's
		// new plan) gets its own PRs.
		session.mu.Lock()
		if session.prPublished && session.prRound.Equal(round.round.Start) {
			session.mu.Unlock()
			log.Printf("pr-coordinator: work_id=%s — pr.ready already published for this round, skipping duplicate", workID)
			return nil
		}
		session.prPublished = true
		session.prRound = round.round.Start
		session.mu.Unlock()

		// M1-05: the PR and gate transactions below carry the
		// processed-event guard row (the first to commit records the
		// event). GitHub PR creation is an HTTP call before its
		// transaction: a crash between the two re-runs it on redelivery,
		// as before.
		db := idempotency.Wrap(msgCtx, wg.DB())

		// Roadmap 1.2: validate the parent's state before opening any PR.
		// A parent that was cancelled, blocked, failed or rejected must not
		// get a PR (prParentRefused); a mismatch is committed with the guard
		// row so the stale event is not replayed. Only a failed parent is
		// transitioned (to blocked, with the observed state in the reason);
		// cancelled and rejected have no edge to blocked, and a blocked one
		// is already there.
		pre, perr := idempotency.CheckPrecondition(msgCtx, db, idempotency.Precondition{
			Action:      "open a pull request",
			AgentID:     agentID,
			AggregateID: workID,
			WorkID:      workID,
			NotIn:       prParentRefused,
		})
		if idempotency.IsDuplicate(perr) {
			return nil
		}
		if perr != nil {
			log.Printf("pr-coordinator: work_id=%s precondition check: %v — retrying delivery", workID, perr)
			session.mu.Lock()
			session.prPublished = false
			session.mu.Unlock()
			return fmt.Errorf("precondition check for work_id=%s: %w", workID, perr)
		}
		if !pre.OK {
			log.Printf("pr-coordinator: work_id=%s NOT opening PRs (blocked=%t): %s", workID, pre.Blocked, pre.Reason)
			wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusFailed) //nolint:errcheck
			t.remove(workID)
			return nil
		}

		// M4-06: a replay item (shadow evaluation of a candidate
		// configuration), or an item that ran a requested snapshot outside
		// a scratch product without an approved rollout, never gets a PR
		// or a pending gate: its outputs are never merged. Each verified
		// subtask branch is sent to review-agent as a PR-less pr.ready
		// (replay=true); status-orchestrator cancels the item on the
		// review.complete ("replay: evidence recorded"). With no reviewable
		// branch, replay.complete terminates it directly. A failed lookup
		// fails closed: the delivery is retried rather than risk a PR.
		replay, rerr := rollout.IsShadowTerminal(msgCtx, wg.DB(), workID)
		if rerr != nil {
			log.Printf("pr-coordinator: work_id=%s replay check: %v — retrying delivery", workID, rerr)
			session.mu.Lock()
			session.prPublished = false
			session.mu.Unlock()
			return fmt.Errorf("replay check for work_id=%s: %w", workID, rerr)
		}
		replayReviews := 0
		if replay {
			session.mu.Lock()
			decisions := make([]string, 0, len(session.outcomes))
			for tid, o := range session.outcomes {
				decisions = append(decisions, fmt.Sprintf("%s=%s", tid, o.Label))
			}
			session.mu.Unlock()
			sort.Strings(decisions)
			log.Printf("pr-coordinator: work_id=%s is a replay item — no PR; verified branches go to review only (%s)", workID, strings.Join(decisions, ", "))
		}

		log.Printf("pr-coordinator: all tasks complete for work_id=%s, creating one PR per repo", workID)

		// Look up the work item to get title and product.
		workItem, err := wg.GetWorkItem(msgCtx, workID)
		if err != nil {
			log.Printf("pr-coordinator: lookup work item %s: %v", workID, err)
		}
		workTitle := fmt.Sprintf("Automated implementation for work item %s", workID)
		if workItem != nil && workItem.Title != "" {
			workTitle = workItem.Title
		}
		productIDStr := ""
		if workItem != nil && workItem.ProductID != nil {
			productIDStr = workItem.ProductID.String()
		}
		isRetry := false
		if workItem != nil {
			transitions, _ := wg.GetTransitions(msgCtx, workID)
			for _, tr := range transitions {
				if tr.ToState == models.WorkItemStateFailed {
					isRetry = true
					break
				}
			}
		}

		// Design: one branch per repo, one PR per repo. The planner creates
		// exactly one passed subtask per distinct repository, and that
		// subtask's branch holds ALL the work landed in that repo. Iterate
		// every passed subtask child and open a PR from its branch into
		// that repo's base_branch. Each PR gets its own pr.ready event so
		// the downstream review-agent runs per PR.
		type perRepoResult struct {
			repoName string
			repoID   string
			branch   string
			prURL    string
			prNumber int
		}
		var results []perRepoResult

		// Check whether this work item gates PR creation behind manual approval.
		var prGateMode string
		if dbErr := wg.DB().QueryRow(msgCtx,
			"SELECT pr_gate_mode FROM work_items WHERE id = $1", workID,
		).Scan(&prGateMode); dbErr != nil {
			prGateMode = "auto"
		}

		// M2-06: producers of a contract edge open their PRs before their
		// consumers, so the consumer's PR can link them.
		if p := wg.DB(); p != nil {
			children = orderProducersFirst(msgCtx, p, children)
		}
		openedPRs := map[uuid.UUID]openedPR{}
		shippedPRs := map[uuid.UUID]bool{}
		// F2: unackedErr is a commit that failed after its in-process
		// retries AND whose block failed too: the loop stops and the
		// delivery is not acknowledged.
		var unackedErr error

	subtasks:
		for _, c := range children {
			if c.Type != models.WorkItemTypeSubtask {
				continue
			}
			// Same race-tolerance as the count loop: the subtask's DB row
			// may still be in state=implementation when status-orchestrator
			// hasn't committed the "→ passed" transition yet, even though
			// we've received its implementation.complete event. Without
			// this, we'd log "all tasks complete" and then turn around and
			// say "no eligible subtasks" one line later — exactly the bug
			// the 02:03 run hit.
			if c.State != models.WorkItemStatePassed && !seenTasks[c.ID.String()] {
				continue
			}
			if c.RepositoryID == nil {
				log.Printf("pr-coordinator: passed subtask %s has no repository_id — skipping PR creation", c.ID)
				continue
			}
			repoID := c.RepositoryID.String()
			baseSHA := pinnedBaseSHA(msgCtx, wg.DB(), workID, *c.RepositoryID)

			// The subtask's decision: from its implementation.complete
			// event when we saw it; else from its persisted evidence under
			// its own product policy (coordinator restarted between the
			// event and the PR); with neither, work-item state alone is
			// not evidence and the subtask gets no PR.
			session.mu.Lock()
			outcome, seen := session.outcomes[c.ID.String()]
			session.mu.Unlock()
			if !seen {
				outcome = decideUnseenSubtask(msgCtx, wg, productStore, evidenceStore, workID, c.ID.String())
				// R14: and from its stored verification decision; none
				// blocks (no PR without the verifier's approval).
				var vq verificationQuerier
				if p := wg.DB(); p != nil {
					vq = p
				}
				outcome = applyVerification(outcome, storedVerification(msgCtx, vq, c.ID.String()))
				outcome.Summary = c.Title
				log.Printf("pr-coordinator: subtask %s on work_id=%s had no verification event; decided from stored evidence and verification (%s): %s", c.ID, workID, outcome.Label, outcome.Reason)
			}
			if outcome.Action == evidence.ActionBlock {
				log.Printf("pr-coordinator: subtask %s on work_id=%s repo=%s gets no PR (%s): %s", c.ID, workID, repoID, outcome.Label, outcome.Reason)
				continue
			}
			outcomes := []subtaskOutcome{outcome}
			partiallyVerified := requiresHumanReview(outcomes)

			// Per-repo base_branch + clone_path + name.
			var repoBase, repoClonePath, repoName, repoURL string
			baseBranch := "main"
			// G1: a repository whose remote is not on github.com has no
			// GitHub API; its PR goes through the manual PR gate.
			repoNoGitHub := false
			repoKind := gitremote.Kind("")
			if dbErr := wg.DB().QueryRow(msgCtx,
				`SELECT base_branch, clone_path, name, github_url FROM repositories WHERE id = $1`, repoID,
			).Scan(&repoBase, &repoClonePath, &repoName, &repoURL); dbErr == nil {
				if repoBase != "" {
					baseBranch = repoBase
				}
				repoKind, _ = gitremote.Classify(repoURL)
				repoNoGitHub = !repoKind.IsGitHub()
			} else {
				log.Printf("pr-coordinator: lookup repository %s: %v", repoID, dbErr)
			}

			taskShort := c.ID.String()
			if len(taskShort) > 8 {
				taskShort = taskShort[:8]
			}
			branch := fmt.Sprintf("agent/%s/%s", workID.String(), taskShort)

			// PR title: on retry use the subtask title (describes the fix);
			// otherwise prefix the work item title with the repo name so a
			// reviewer looking at a multi-repo feature can tell which PR is
			// which at a glance.
			prTitle := workTitle
			if isRetry {
				prTitle = c.Title
			} else if repoName != "" {
				prTitle = fmt.Sprintf("[%s] %s", repoName, workTitle)
			}
			// A partially verified PR is marked in the title so it cannot
			// be mistaken for a verified one in a PR list (D2, D3).
			// R14: a verifier approval the second-pass review still
			// decides is labelled too (inside the human-review marker).
			prTitle = prTitleFor(verificationTitle(prTitle, outcomes), outcomes)

			// Per-repo body — only this subtask's outcome, so the
			// description matches the branch. Files and head SHA come from
			// the evidence, not the planner.
			// R13 D3: the criteria that target the PR text are written into it.
			var prRequirements []plan.Criterion
			if pool := wg.DB(); pool != nil {
				prRequirements = loadPRTextRequirements(msgCtx, pool, c.ID)
			}
			prDesc := buildPRDescription(msgCtx, repoClonePath, baseBranch, branch, prTitle, outcomes, prRequirements)
			allFiles := changedFilesFor(outcomes)
			headSHA := outcome.HeadSHA
			if partiallyVerified {
				log.Printf("pr-coordinator: work_id=%s repo=%s branch=%s head=%s — PR will be marked %s", workID, repoName, branch, headSHA, humanReviewBodyMarker(outcomes))
			}

			// M2-03: compare origin's base tip with the pin before opening
			// the PR. Moved and clean: audited, noted, PR proceeds. Moved
			// with conflicts: the parent is blocked (with its prompt) and no
			// further PR is opened.
			productForDrift := uuid.Nil
			if pid, pErr := uuid.Parse(productIDStr); pErr == nil {
				productForDrift = pid
			}
			drift := applyBaseDrift(msgCtx, prDriftChecker, db, prompts, driftInput{
				WorkID: workID, RepoID: *c.RepositoryID, ProductID: productForDrift, WorkTitle: workTitle,
				RepoName: repoName, ClonePath: repoClonePath, BaseRef: baseBranch,
				Pin: baseSHA, HeadSHA: headSHA, Branch: branch,
			})
			if drift.Stop {
				break subtasks
			}
			if drift.DescriptionLine != "" {
				prDesc += "\n\n" + drift.DescriptionLine
			}

			// M4-06 replay: review the verified branch without a PR. The
			// branch and the PR-less pr.ready commit together (the first
			// commit carries the delivery's ledger row).
			if replay {
				if headSHA == "" {
					log.Printf("pr-coordinator: replay work_id=%s subtask %s has no verified head — not reviewed", workID, c.ID)
					continue
				}
				replayMsg := prReadyMessage{
					WorkID:            workID.String(),
					ProductID:         productIDStr,
					RepositoryID:      repoID,
					PRDescription:     prDesc,
					Branch:            branch,
					FilesChanged:      allFiles,
					Timestamp:         time.Now().UTC().Format(time.RFC3339),
					HeadSHA:           headSHA,
					PartiallyVerified: partiallyVerified,
					BaseSHA:           baseSHA,
					BaseDrift:         drift.Drift,
					Replay:            true,
				}
				stop, rerr := recordPRReady(msgCtx, db, wg, workID, branch, repoName, replayMsg, drift.Events...)
				if rerr != nil {
					unackedErr = rerr
					break subtasks
				}
				replayReviews++
				log.Printf("pr-coordinator: replay work_id=%s repo=%s branch=%s head=%s sent to review without a PR", workID, repoName, branch, headSHA)
				if stop {
					break subtasks
				}
				continue
			}

			// M2-06: a consumer subtask's contracts are checked against its
			// producers' heads before its PR opens. A mismatch (or, under
			// contracts_strict, an unchecked contract) blocks the parent with
			// the specific mismatch and no further PR is opened.
			contractsChecked := applyContractChecks(msgCtx, prContractChecker, wg.DB(), db, prompts, contractInput{
				WorkID: workID, TaskID: c.ID, RepoID: *c.RepositoryID, ProductID: productForDrift, WorkTitle: workTitle,
				RepoName: repoName, ClonePath: repoClonePath, HeadSHA: headSHA, Branch: branch,
				ProducerHead: func(id uuid.UUID) string {
					session.mu.Lock()
					defer session.mu.Unlock()
					return session.outcomes[id.String()].HeadSHA
				},
				OpenedPRs: openedPRs,
				Shipped:   shippedPRs,
			})
			if contractsChecked.Stop {
				break subtasks
			}
			if contractsChecked.DescriptionSection != "" {
				prDesc += "\n\n" + contractsChecked.DescriptionSection
			}
			if line := pairedPRLine(contractsChecked.Producers); line != "" {
				prDesc += "\n\n" + line
			}

			// When the work item's PR gate is set to manual, record the
			// pending change in the database and publish pr.gated instead of
			// creating a real GitHub PR. The dashboard will surface pending
			// changes for human approval.
			//
			// The gate has no automatic approval: a pending row is only
			// ever approved by a person (dashboard ApprovePendingChange or
			// the pr_gate prompt answer), and both re-use the stored
			// pr_title/pr_description verbatim, so the partial-verification
			// marker recorded here is what the approver sees.
			gateReason := manualGateReason(prGateMode, repoKind, repoNoGitHub)
			if gateReason == gateReasonNoGitHub {
				log.Printf("pr-coordinator: work_id=%s repo=%s has a %s remote (not GitHub): GitHub API PR creation skipped; routing branch %s through the manual PR gate", workID, repoName, repoKind, branch)
			}
			if gateReason != "" {
				gatedMsg := prReadyMessage{
					WorkID:            workID.String(),
					ProductID:         productIDStr,
					RepositoryID:      repoID,
					Branch:            branch,
					FilesChanged:      allFiles,
					Timestamp:         time.Now().UTC().Format(time.RFC3339),
					HeadSHA:           headSHA,
					PartiallyVerified: partiallyVerified,
					BaseSHA:           baseSHA,
					BaseDrift:         drift.Drift,
					ContractsChecked:  contractsChecked.Checked,
				}
				productUUID := uuid.Nil
				if pid, pErr := uuid.Parse(productIDStr); pErr == nil {
					productUUID = pid
				}
				question := fmt.Sprintf("Branch %s is ready to create a PR against %s.", branch, baseBranch)
				if headSHA != "" {
					question += fmt.Sprintf("\n\nVerified head SHA: %s", headSHA)
				}
				question += fmt.Sprintf("\n\nFiles changed: %s", strings.Join(allFiles, ", "))
				// V5: the gate shows the verdict, so the operator decides
				// with it (a local remote always ends here).
				if line := verificationPromptLine(outcomes); line != "" {
					question += "\n\n" + line
				}
				if partiallyVerified {
					question = humanReviewBodyMarker(outcomes) + ": " + outcome.Reason + "\n\n" + question
				}
				prGatePrompt := &interactions.Prompt{
					ID:          uuid.New(),
					WorkItemID:  workID,
					ProductID:   productUUID,
					AskingAgent: "pr-coordinator",
					Trigger:     interactions.TriggerApprovalPRGate,
					PromptType:  interactions.PromptTypeApproval,
					Headline:    fmt.Sprintf("PR ready for review: %s", prTitle),
					Question:    question,
					ContextLink: fmt.Sprintf("/work-items/%s", workID),
					ContextPayload: map[string]any{
						"branch":             branch,
						"base_branch":        baseBranch,
						"repo_name":          repoName,
						"pr_title":           prTitle,
						"files":              allFiles,
						"head_sha":           headSHA,
						"partially_verified": partiallyVerified,
						"verification":       verificationPayload(outcomes),
					},
					Actions: []interactions.Action{
						{ID: "approve", Label: "Create PR", Semantic: interactions.SemanticContinue},
						{ID: "reject", Label: "Reject", Semantic: interactions.SemanticCancel},
					},
					State:       interactions.StatePending,
					RequestedAt: time.Now().UTC(),
					PromptedVia: []string{"dashboard"},
				}
				change := gatedChange{
					WorkID: workID, RepoID: repoID, Branch: branch, BaseBranch: baseBranch,
					Files: allFiles, PRTitle: prTitle, PRDesc: prDesc, HeadSHA: headSHA,
					Gated: gatedMsg, Prompt: prGatePrompt, BaseEvents: drift.Events,
				}
				if productUUID == uuid.Nil {
					// user_interactions.product_id is NOT NULL with a
					// foreign key: the prompt row cannot be written, and
					// in the gate's transaction it would roll back the
					// pending change too. The change still shows in the
					// dashboard's pending list.
					log.Printf("pr-coordinator: WARNING work_id=%s has no product — recording the pending change without a pr-gate prompt", workID)
					change.Prompt = nil
				}
				// The pending row, pr.gated and the pr-gate prompt commit
				// together through the outbox, or none of them do. A failed
				// commit blocks the parent, and no further repository is
				// gated against a blocked parent.
				if stop, rerr := recordGatedPR(msgCtx, db, prompts, change, repoName); rerr != nil {
					unackedErr = rerr
					break subtasks
				} else if stop {
					log.Printf("pr-coordinator: work_id=%s blocked after a failed gate commit — not gating the remaining repositories", workID)
					break subtasks
				}

				results = append(results, perRepoResult{repoName: repoName, repoID: repoID, branch: branch})
				shippedPRs[c.ID] = true
				continue
			}

			prURL, prNumber := createGitHubPR(msgCtx, githubServiceURL, repoID, prTitle, prDesc, branch, baseBranch)
			if prURL != "" {
				log.Printf("pr-coordinator: created PR %s (#%d) for work_id=%s repo=%s", prURL, prNumber, workID, repoName)
			} else {
				log.Printf("pr-coordinator: github-service returned no PR for work_id=%s repo=%s branch=%s", workID, repoName, branch)
			}

			// Never publish pr.ready without a real PR URL. If
			// github-service failed to open the PR (auth, conflict, etc.)
			// prURL will be empty; downstream review-agent would then run
			// against a nonexistent PR, produce a meaningless decision,
			// and potentially restart the implementation loop. Fail
			// honestly: log the skip and move on.
			if prURL == "" {
				log.Printf("pr-coordinator: skipping pr.ready publish for repo=%s branch=%s — no PR URL from github-service", repoName, branch)
				// The drift was still observed: keep the audit row. The pool
				// (not the guarded beginner) so it never takes the delivery's
				// guard ahead of a later repository's pr.ready.
				for _, ev := range drift.Events {
					if err := recordDriftEventOwnTx(msgCtx, wg.DB(), ev); err != nil {
						log.Printf("pr-coordinator: record drift event for work_id=%s repo=%s: %v", workID, repoName, err)
					}
				}
				continue
			}
			prMsg := prReadyMessage{
				WorkID:            workID.String(),
				ProductID:         productIDStr,
				RepositoryID:      repoID,
				PRNumber:          prNumber,
				PRURL:             prURL,
				PRDescription:     prDesc,
				Branch:            branch,
				FilesChanged:      allFiles,
				Timestamp:         time.Now().UTC().Format(time.RFC3339),
				HeadSHA:           headSHA,
				PartiallyVerified: partiallyVerified,
				BaseSHA:           baseSHA,
				BaseDrift:         drift.Drift,
				ContractsChecked:  contractsChecked.Checked,
			}
			openedPRs[c.ID] = openedPR{RepoID: repoID, RepoName: repoName, URL: prURL, Number: prNumber}
			shippedPRs[c.ID] = true
			// M2-06: pair the PRs — this consumer's body links its producers;
			// each producer's body is edited to link this one.
			linkProducerPRs(msgCtx, githubServiceURL, contractsChecked.Producers, prURL, repoName)
			// The PR now exists on GitHub. Record its branch on the work
			// item (so the dashboard shows which branch / PR this ticket is
			// on) and enqueue pr.ready in one transaction. If that commit
			// fails, the PR exists without its pr.ready. F2: a transient
			// failure is retried in process (each failed attempt rolled
			// back its guard row); after maxCommitAttempts, or on a
			// permanent error, the work item is blocked for a person,
			// naming the PR URL, and no further PRs are opened.
			stop, rerr := recordPRReady(msgCtx, db, wg, workID, branch, repoName, prMsg, drift.Events...)
			if rerr != nil {
				unackedErr = rerr
				break subtasks
			}
			results = append(results, perRepoResult{repoName: repoName, repoID: repoID, branch: branch, prURL: prURL, prNumber: prNumber})
			if stop {
				log.Printf("pr-coordinator: work_id=%s blocked after a failed pr.ready commit — not opening PRs for the remaining repositories", workID)
				break subtasks
			}
		}

		if unackedErr == nil && replay && replayReviews == 0 {
			// Nothing reviewable: terminate the replay item now.
			unackedErr = recordReplayComplete(msgCtx, db, workID, "no verified branch to review")
		} else if len(results) == 0 && !replay {
			log.Printf("pr-coordinator: work_id=%s — no verified (or opted-in partially verified) subtasks with a repository_id; no PRs created", workID)
		}

		if unackedErr != nil {
			// F2: the block failed too, so the delivery is not
			// acknowledged. A redelivery (after a restart or rebalance)
			// must run the PR loop again, so the in-memory "already
			// published" mark is cleared.
			session.mu.Lock()
			session.prPublished = false
			session.mu.Unlock()
			return fmt.Errorf("work_id=%s: %w", workID, unackedErr)
		}

		wg.CompleteAssignment(msgCtx, assignment.ID, models.AssignmentStatusCompleted) //nolint:errcheck
		t.remove(workID)
		return nil
	}
}

// Why a repository's PR goes through the manual PR gate instead of
// github-service (manualGateReason).
const (
	gateReasonManual   = "manual"    // the work item's pr_gate_mode
	gateReasonNoGitHub = "no-github" // G1: the remote is not on github.com
)

// manualGateReason returns why the PR goes through the manual gate, or ""
// when it is opened through github-service. A repository whose remote is not
// on github.com (local or bare mirror, file://, another host) has no GitHub
// API, so it is always gated, whatever pr_gate_mode says; noGitHub is false
// when the repository row could not be read (kind unknown).
func manualGateReason(prGateMode string, kind gitremote.Kind, noGitHub bool) string {
	switch {
	case prGateMode == "manual":
		return gateReasonManual
	case noGitHub && !kind.IsGitHub():
		return gateReasonNoGitHub
	}
	return ""
}

// createGitHubPR calls the github-service to create a real GitHub pull request.
// Returns the PR URL and number, or empty string on failure.
func createGitHubPR(ctx context.Context, githubServiceURL, repositoryID, title, body, head, base string) (string, int) {
	reqBody := createPRRequest{
		RepositoryID: repositoryID,
		Title:        title,
		Body:         body,
		Head:         head,
		Base:         base,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		log.Printf("pr-coordinator: marshal create-pr request: %v", err)
		return "", 0
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, githubServiceURL+"/pr", bytes.NewReader(data))
	if err != nil {
		log.Printf("pr-coordinator: build create-pr request: %v", err)
		return "", 0
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("pr-coordinator: POST github-service /pr: %v", err)
		return "", 0
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		log.Printf("pr-coordinator: github-service /pr returned %d: %s", resp.StatusCode, respData)
		return "", 0
	}

	var prResp createPRResponse
	if err := json.Unmarshal(respData, &prResp); err != nil {
		log.Printf("pr-coordinator: parse github-service /pr response: %v", err)
		return "", 0
	}

	return prResp.PRURL, prResp.PRNumber
}

// buildPRDescription gathers the actual git diff (never the LLM's account of
// it) and renders the body through renderPRBody, which carries each
// subtask's outcome, verified head SHA and evidence-derived file list.
func buildPRDescription(ctx context.Context, clonePath, baseBranch, headBranch, title string, outcomes []subtaskOutcome, requirements []plan.Criterion) string {
	in := prBodyInput{
		Title:        title,
		Branch:       headBranch,
		BaseBranch:   baseBranch,
		Outcomes:     outcomes,
		Requirements: requirements,
	}

	if clonePath != "" {
		diffCmd := exec.CommandContext(ctx, "git", "diff", "--stat", baseBranch+".."+headBranch)
		diffCmd.Dir = clonePath
		diffOutput, err := diffCmd.CombinedOutput()
		if err == nil {
			in.DiffStat = string(diffOutput)
		} else {
			log.Printf("pr-coordinator: git diff --stat %s..%s in %s: %v: %s", baseBranch, headBranch, clonePath, err, diffOutput)
		}

		diffDetailCmd := exec.CommandContext(ctx, "git", "diff", baseBranch+".."+headBranch)
		diffDetailCmd.Dir = clonePath
		detailOutput, err := diffDetailCmd.CombinedOutput()
		if err == nil {
			in.Diff = string(detailOutput)
		} else {
			log.Printf("pr-coordinator: git diff %s..%s in %s: %v: %s", baseBranch, headBranch, clonePath, err, detailOutput)
		}
	}

	return renderPRBody(in)
}

// loadPolicyForTask resolves the D2 release policy governing a subtask
// through products.LoadReleasePolicyForItem (own product, else parent's).
// Every failure — unparseable task id, missing row, store error — is logged
// and yields the zero policy, which is "not opted in": a lookup problem can
// only make the gate stricter. workID is the parent, used for the log.
// It also returns the subtask's repository id from the same work item row
// for the evidence identity check: nil when the row has none or could not
// be loaded, which skips only the repository comparison.
func loadPolicyForTask(ctx context.Context, wg products.WorkItemLookup, store products.PolicyStore, taskID string, workID uuid.UUID) (evidence.ReleasePolicy, *string) {
	taskUUID, err := uuid.Parse(taskID)
	if err != nil {
		log.Printf("pr-coordinator: implementation.complete: work_item=%s task_id %q is not a UUID: %v — partial verification policy defaults to not opted in", workID, taskID, err)
		return evidence.ReleasePolicy{}, nil
	}
	item, err := wg.GetWorkItem(ctx, taskUUID)
	if err != nil {
		log.Printf("pr-coordinator: implementation.complete: work_item=%s get task %s for product policy: %v — partial verification policy defaults to not opted in", workID, taskUUID, err)
		return evidence.ReleasePolicy{}, nil
	}
	policy, err := products.LoadReleasePolicyForItem(ctx, store, wg, item)
	if err != nil {
		log.Printf("pr-coordinator: implementation.complete: %v — partial verification policy defaults to not opted in", err)
	}
	return policy, repositoryIDString(item.RepositoryID)
}

// pgxExecer is the slice of *pgxpool.Pool insertPendingPRChange needs.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertPendingPRChangeSQL records a gated PR with its verified head SHA
// (migration 046). There is no pre-046 fallback: the insert runs inside the
// gate's transaction, where a failed statement aborts the transaction and no
// retry can follow it, so migration 046 is a deploy prerequisite.
const insertPendingPRChangeSQL = `INSERT INTO pending_pr_changes
		(work_item_id, repository_id, branch, base_branch, files_changed, diff_stats, pr_title, pr_description, head_sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))`

// insertPendingPRChange records a gated PR in db, which is the gate's
// transaction (commitGatedPR).
func insertPendingPRChange(ctx context.Context, db pgxExecer, workID uuid.UUID, repoID, branch, baseBranch string, files []string, prTitle, prDesc, headSHA string) error {
	_, err := db.Exec(ctx, insertPendingPRChangeSQL, workID, repoID, branch, baseBranch, files, "", prTitle, prDesc, headSHA)
	return err
}
