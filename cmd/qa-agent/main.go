package main

import (
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
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	agentID       = "qa-agent"
	consumerGroup = "qa-agent"
	healthPort    = 8098
)

// qaReadyMessage is received from qa.ready.
type qaReadyMessage struct {
	WorkID             string `json:"work_id"`
	Title              string `json:"title"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Branch             string `json:"branch,omitempty"`

	// HeadSHA is the verified head SHA from the implementation evidence
	// (D1.5), forwarded by the status-orchestrator from the review
	// artifact. QA checks out exactly this commit; when absent (legacy
	// producer) the EVIDENCE_COMPAT_WINDOW rules apply.
	HeadSHA string `json:"head_sha,omitempty"`
	// BaseSHA is the work item's pinned base commit (M2-02), forwarded by
	// the status-orchestrator; empty when legacy-unpinned. The QA report
	// records it (M2-03, base.go); the checkout stays at HeadSHA.
	BaseSHA string `json:"base_sha,omitempty"`
	// BaseDrift is base drift a producer forwarded (pr.ready.base_drift's
	// shape); recorded in the QA report with the item's drift events.
	BaseDrift *baseDrift `json:"base_drift,omitempty"`
}

// qaResultMessage is published to qa.result.
type qaResultMessage struct {
	WorkID string `json:"work_id"`
	Result string `json:"result"`
	Report string `json:"report"`
}

// qaBugsMessage is published to qa.bugs.
type qaBugsMessage struct {
	WorkID            string `json:"work_id"`
	Bugs              string `json:"bugs"`
	ReproductionSteps string `json:"reproduction_steps"`
}

// criterionResult holds the pass/fail verdict for one criterion with real evidence.
type criterionResult struct {
	Criterion string `json:"criterion"`
	Pass      bool   `json:"pass"`
	Evidence  string `json:"evidence"`
}

const qaSystemPrompt = `You are a QA verification engineer. Your ONLY job is to VERIFY that code works — you must NEVER modify, write, or fix any code. You do not have write_file or edit_file tools.

IMPORTANT: You are verifying, not implementing. Do not try to fix issues you find. Report them.

Your verification process:
1. Read the acceptance criteria carefully
2. Use read_file to inspect the changed files and verify they match the criteria
3. Use run_command to run the build (e.g. 'npm run build', 'go build ./...') and verify it succeeds
4. If tests exist, use run_command to execute the test suite (e.g. 'npm test', 'go test ./...')
5. Use search_code to verify imports, function usage, and integration points
6. For each criterion, check if the code implements it correctly based on what you READ

Format your report as:

BUILD CHECK:
<build command and result>

TEST RUN:
<test command output, or "No test suite found">

CRITERIA RESULTS:
- PASS/FAIL: <criterion> — <evidence from code inspection or test output>

OVERALL: PASS or FAIL

If there are no tests, verify criteria through code inspection and build checks. A project with no tests but correct code that builds cleanly can still PASS.`

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("qa-agent: load config: %v", err)
	}

	pool, err := db.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("qa-agent: connect db: %v", err)
	}
	defer pool.Close()

	kb, err := bus.NewKafkaBus(cfg.Kafka.Brokers)
	if err != nil {
		log.Fatalf("qa-agent: connect kafka: %v", err)
	}
	defer kb.Close() //nolint:errcheck

	wg := workgraph.New(pool)
	registry := agentregistry.New(pool)

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

	// D1.5 compatibility window for qa.ready messages without a verified
	// head_sha (legacy producers). Unknown values fall back to strict.
	window := evidence.CompatWindowFromEnv()
	log.Printf("qa-agent: %s=%s", evidence.CompatWindowEnv, window)

	checker := health.New(agentID)
	checker.AddCheck("database", func() error {
		return pool.Ping(ctx)
	})
	checker.AddCheck("kafka", kb.HealthCheck())

	// Outbox dispatcher (M1-04): qa.result, qa.bugs and work.blocked are
	// committed to outbox_events with the writes they announce, and this
	// loop carries them to Kafka. A start failure is fatal; one per process.
	dispatcher, err := outbox.StartForService(ctx, pool, kb, log.Default(), "qa-agent")
	if err != nil {
		log.Fatalf("qa-agent: start outbox dispatcher: %v", err)
	}
	log.Printf("qa-agent: outbox dispatcher started claimed_by=%s", dispatcher.Name())

	// M1-05: deduplicate on the processed-event ledger. The up-front Seen
	// check acknowledges a recorded duplicate before any checkout or LLM call.
	handler := idempotency.NewLedger(pool).Handler("qa-agent", buildHandler(ctx, pool, wg, registry, cs, kb, window))
	if err := kb.Subscribe(bus.TopicQAReady, consumerGroup, handler); err != nil {
		log.Fatalf("qa-agent: subscribe: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", checker.Handler())
	mux.HandleFunc("/metrics", checker.MetricsHandler())
	healthAddr := fmt.Sprintf(":%d", healthPort)
	healthSrv := &http.Server{Addr: healthAddr, Handler: mux}

	go func() {
		log.Printf("qa-agent: health server on %s", healthAddr)
		if err := healthSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("qa-agent: health server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("qa-agent: shutting down...")
	cancel()
	// Let an in-flight outbox publish finish being recorded before exit.
	dispatcher.Wait()
	log.Println("qa-agent: outbox dispatcher stopped")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := healthSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("qa-agent: health server shutdown error: %v", err)
	}
	log.Println("qa-agent: stopped")
}

func buildHandler(
	ctx context.Context,
	pool *pgxpool.Pool,
	wg *workgraph.WorkGraph,
	registry *agentregistry.Registry,
	cs *configstore.ConfigStore,
	kb bus.MessageBus,
	window evidence.CompatWindow,
) bus.Handler {
	logger := logging.New("qa-agent")
	return func(msgCtx context.Context, msg *bus.Message) error {
		// Outcome writes and their events commit together (M1-04). They use
		// the process ctx so a cancelled message ctx cannot drop the event.
		// M1-05: the writer's transactions carry the processed-event guard
		// row (the first to commit records the event); Wrap captures this
		// delivery now, so the process ctx can still be used to commit.
		qw := qaWriter{db: idempotency.Wrap(msgCtx, pool), wg: wg}
		var payload qaReadyMessage
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			log.Printf("qa-agent: unmarshal: %v", err)
			return nil
		}

		workID, err := uuid.Parse(payload.WorkID)
		if err != nil {
			log.Printf("qa-agent: parse work_id %q: %v", payload.WorkID, err)
			return nil
		}

		// Attach work item ID to context for structured log correlation.
		msgCtx = logging.WithWorkItemID(msgCtx, workID.String())

		logger.Info(msgCtx, "processing QA task", map[string]interface{}{
			"work_id":  workID.String(),
			"branch":   payload.Branch,
			"head_sha": payload.HeadSHA,
		})

		// Roadmap 1.2: validate the work item's state before checking out
		// and testing anything. A qa.ready for an item that has since left
		// qa (cancelled, blocked, sent back) is stale; a mismatch blocks it
		// with the observed and expected states (when it can still be
		// blocked), committed with the guard row.
		pre, perr := idempotency.CheckPrecondition(msgCtx, qw.db, idempotency.Precondition{
			Action:      "check out and test the work item",
			AgentID:     agentID,
			AggregateID: workID,
			WorkID:      workID,
			Expected:    []models.WorkItemState{models.WorkItemStateQA},
		})
		if idempotency.IsDuplicate(perr) {
			return nil
		}
		if perr != nil {
			return fmt.Errorf("precondition check for work_id=%s: %w", workID, perr)
		}
		if !pre.OK {
			log.Printf("qa-agent: NOT testing work_id=%s (blocked=%t): %s", workID, pre.Blocked, pre.Reason)
			return nil
		}

		// Record agent assignment for this work item.
		assignment := &models.AgentAssignment{
			WorkItemID:      workID,
			AgentType:       models.AgentTypeQA,
			AgentInstanceID: agentID,
			Status:          models.AssignmentStatusActive,
		}
		if err := wg.AssignAgent(msgCtx, assignment); err != nil {
			log.Printf("qa-agent: assign agent: %v", err)
		}

		// Resolve repository and clone path from the work item.
		clonePath := resolveClonePath(msgCtx, pool, wg, workID)

		// Validate context before running QA.
		repoCheck := guards.CheckRepoAvailable(clonePath)
		if !repoCheck.Valid {
			log.Printf("qa-agent: context validation failed for work_id=%s: %s", workID, repoCheck.Message)
			reason := "Cannot run QA: no repository available. " + repoCheck.Message
			qw.block(ctx, workID, assignment.ID, reason) //nolint:errcheck // logged inside
			return nil
		}

		// D1.5: check out the verified head SHA (detached) so QA tests
		// exactly the commit the evidence was recorded against, not
		// whatever the branch points at now. The branch and SHA come from
		// the qa.ready message (set by the status-orchestrator from the
		// review artifact); a moved branch blocks the item.
		checkout := checkoutVerifiedHead(msgCtx, clonePath, workID, payload.Branch, payload.HeadSHA, window)
		if checkout.Cleanup != nil {
			defer checkout.Cleanup()
		}
		if checkout.BlockReason != "" {
			log.Printf("qa-agent: BLOCKED work_id=%s: %s", workID, checkout.BlockReason)
			qw.block(ctx, workID, assignment.ID, checkout.BlockReason) //nolint:errcheck // logged inside
			return nil
		}
		if checkout.Legacy {
			metrics.EvidenceCompatLegacyFallback.Inc()
		}
		if checkout.Worktree != "" {
			clonePath = checkout.Worktree
		}
		// testedSHA is recorded in the QA report: the verified head SHA
		// (from its branch, or reachable via the merge after the branch was
		// deleted), the tip under the legacy window, or the main clone's
		// HEAD when no feature branch could be checked out (legacy only).
		testedSHA, testedSource := checkout.SHA, checkout.Source
		if testedSHA == "" {
			testedSHA = mainCloneHead(msgCtx, clonePath)
			testedSource = sourceMainClone
		}

		// Resolve LLM adapter per work item: checks agent_definition first, falls back to global.
		resolution, err := registry.Resolve(msgCtx, workID, cs, "qa-agent", "instance-1")
		if err != nil {
			log.Printf("qa-agent: resolve adapter for work_id=%s: %v", workID, err)
			reason := "Cannot create LLM adapter: provider not configured or credentials missing. Configure model credentials in Admin → Credentials."
			qw.block(ctx, workID, assignment.ID, reason) //nolint:errcheck // logged inside
			return nil
		}
		llmAdapter := resolution.Adapter
		// M4-05: tag this QA run's inference rows (role qa, snapshot).
		msgCtx = llm.WithInferenceTags(msgCtx, llm.TagsForResolution(resolution, workID))

		// Build the QA task message with all available context.
		userMessage := fmt.Sprintf(`QA task for: %s

Acceptance criteria to verify:
%s

Repository path: %s
Commit under test: %s (checked out detached; source: %s)
Work ID: %s

Run the test suite and verify each acceptance criterion with actual test output.`,
			payload.Title,
			payload.AcceptanceCriteria,
			clonePath,
			testedSHA,
			testedSource,
			workID.String(),
		)

		executor := tools.NewExecutor(clonePath)

		// Publish each tool call to Kafka so the dashboard's Live Activity
		// panel streams QA progress in real time. Mirrors the pattern in
		// research-agent / implementation-agent. Without this, QA phases
		// (test runs, log inspection) appear as a static "qa" state for
		// minutes at a time with no visible activity.
		workIDForCb := workID.String()
		executor.SetCallback(func(callID, toolName, arguments, result string, isError bool, _ tools.ResultMeta) {
			event := map[string]interface{}{
				"work_item_id":   workIDForCb,
				"task_id":        "",
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
				"task_id":      "",
				"agent_type":   "qa-agent",
				"tool":         toolName,
				"arguments":    arguments,
				"result":       result,
				"is_error":     isError,
				"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
			}
			consolePayload, _ := json.Marshal(consoleEvent)
			kb.Publish(ctx, bus.TopicConsoleStream, workIDForCb, consolePayload) //nolint:errcheck
		})

		req := adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{
				{Role: adapter.RoleSystem, Content: qaSystemPrompt},
				{Role: adapter.RoleUser, Content: userMessage},
			},
			Tools:       tools.QATools(),
			ToolChoice:  "auto",
			MaxTokens:   8192,
			Temperature: 0.1,
		}

		logger.Info(msgCtx, "running tool loop", map[string]interface{}{
			"work_id": workID.String(),
		})

		// Run the tool loop — the LLM will execute real tests.
		resp, err := tools.RunToolLoop(msgCtx, llmAdapter, req, executor, 30, pool, &workID)
		if err != nil {
			if errors.Is(err, tools.ErrToolCallingNotSupported) {
				log.Printf("qa-agent: model does not support tool calling: %v", err)
				qw.block(ctx, workID, assignment.ID, //nolint:errcheck // logged inside
					"Cannot run QA: the configured LLM model does not support tool calling. "+
						"QA requires tool calling to execute test commands and read results. "+
						"Action needed: assign a tool-calling capable model to the QA agent.")
				return nil
			}
			logger.Error(msgCtx, "tool loop failed", err, map[string]interface{}{
				"work_id": workID.String(),
			})
		}

		report := ""
		if resp != nil {
			report = resp.Content
		}

		// Finalize: if the tool loop ended without a structured report,
		// inject a "write your report NOW" message and give the model a
		// few more iterations with no tools (forcing text output).
		trimmed := strings.TrimSpace(report)
		if trimmed == "" || !strings.Contains(strings.ToLower(trimmed), "overall:") {
			log.Printf("qa-agent: no OVERALL marker after tool loop — injecting finalize prompt (work_id=%s)", workID)
			finalizeMsg := adapter.ChatMessage{
				Role: adapter.RoleUser,
				Content: "STOP investigating. You have gathered enough information. " +
					"Write your final QA report NOW in exactly this format:\n\n" +
					"BUILD CHECK:\n<result>\n\nCRITERIA RESULTS:\n- PASS/FAIL: <criterion> — <evidence>\n\nOVERALL: PASS or FAIL\n\n" +
					"Base your verdict on what you have already observed. Do NOT call any more tools. Write the report as plain text.",
			}
			if resp != nil && resp.Content != "" {
				req.Messages = append(req.Messages,
					adapter.ChatMessage{Role: adapter.RoleAssistant, Content: resp.Content},
					finalizeMsg,
				)
			} else {
				req.Messages = append(req.Messages, finalizeMsg)
			}
			req.Tools = nil
			req.ToolChoice = ""
			finalResp, finalErr := llmAdapter.Complete(msgCtx, req)
			if finalErr == nil && finalResp != nil && strings.TrimSpace(finalResp.Content) != "" {
				report = finalResp.Content
				log.Printf("qa-agent: finalize produced report (%d bytes)", len(report))
			}
		}

		trimmed = strings.TrimSpace(report)
		if trimmed == "" || !strings.Contains(strings.ToLower(trimmed), "overall:") {
			reason := "QA report is empty or unparseable (no OVERALL: marker). Manual verification required — cannot auto-approve."
			log.Printf("qa-agent: %s (work_id=%s)", reason, workID)
			qw.block(ctx, workID, assignment.ID, reason) //nolint:errcheck // logged inside
			return nil
		}

		// Parse the overall PASS/FAIL from the report.
		allPass := parseOverallVerdict(report)

		// Parse individual criterion results from the report.
		criteria := extractCriteria(payload.AcceptanceCriteria, "")
		results := parseCriteriaResults(report, criteria)

		// Compile QA report artifact. head_sha is the commit QA actually
		// tested (D1.5); head_sha_source says whether it was the verified
		// evidence SHA (on its branch, or via the merge after the branch was
		// deleted), a legacy branch tip, or the main clone.
		// M2-03: the pinned base this work was built on and the drift
		// status QA received (base.go).
		reportBaseSHA, reportBaseDrift := qaBase(msgCtx, snapshot.NewStore(pool), workID, payload)
		reportData, _ := json.Marshal(map[string]interface{}{
			"work_id":          workID.String(),
			"branch":           checkout.Branch,
			"head_sha":         testedSHA,
			"head_sha_source":  testedSource,
			"base_sha":         reportBaseSHA,
			"base_drift":       reportBaseDrift,
			"criteria_results": results,
			"all_pass":         allPass,
			"llm_report":       report,
		})

		qaArtifact := &models.WorkItemArtifact{
			WorkItemID: workID,
			Type:       models.ArtifactTypeQAReport,
			Content:    reportData,
			AgentID:    agentID,
		}
		// The report artifact, the verdict event and the assignment
		// completion commit in one transaction (M1-04).
		var resultMsg *qaResultMessage
		var bugsMsg *qaBugsMessage
		if allPass {
			logger.Info(msgCtx, "QA PASS", map[string]interface{}{
				"work_id": workID.String(),
			})
			resultMsg = &qaResultMessage{
				WorkID: workID.String(),
				Result: "pass",
				Report: string(reportData),
			}
		} else {
			logger.Warn(msgCtx, "QA FAIL", map[string]interface{}{
				"work_id": workID.String(),
			})
			var bugs []string
			var steps []string
			for _, r := range results {
				if !r.Pass {
					bugs = append(bugs, fmt.Sprintf("FAIL: %s — %s", r.Criterion, r.Evidence))
					steps = append(steps, fmt.Sprintf("1. Test: %s\n   Expected: pass\n   Got: %s", r.Criterion, r.Evidence))
				}
			}
			bugsMsg = &qaBugsMessage{
				WorkID:            workID.String(),
				Bugs:              strings.Join(bugs, "\n"),
				ReproductionSteps: strings.Join(steps, "\n"),
			}
		}

		qw.recordResult(ctx, workID, assignment.ID, qaArtifact, resultMsg, bugsMsg)
		return nil
	}
}

// resolveClonePath looks up the clone path via the work item's repository_id.
// Falls back to /workspace/repo if not found.
func resolveClonePath(ctx context.Context, pool *pgxpool.Pool, wg *workgraph.WorkGraph, workID uuid.UUID) string {
	var clonePath string
	err := pool.QueryRow(ctx, `
		SELECT r.clone_path
		FROM repositories r
		JOIN work_items w ON w.repository_id = r.id
		WHERE w.id = $1
	`, workID).Scan(&clonePath)
	if err != nil || clonePath == "" {
		log.Printf("qa-agent: lookup clone_path for work_id %s: %v — using default", workID, err)
		return "/workspace/repo"
	}
	return clonePath
}

// parseOverallVerdict checks whether the LLM's report explicitly says
// OVERALL: PASS. Absence-of-signal is treated as FAIL, not PASS.
//
// Empty/unparseable reports are treated as FAIL so flaky LLM calls don't let
// broken code ship. Previously the function returned true when there was no
// "OVERALL:" marker and no "fail" keyword, which meant an empty or truncated
// LLM response auto-approved merge.
func parseOverallVerdict(report string) bool {
	lc := strings.ToLower(report)
	idx := strings.Index(lc, "overall:")
	if idx < 0 {
		// No explicit verdict marker → fail honestly.
		return false
	}
	after := lc[idx:]
	window := after[:min(50, len(after))]
	// Require explicit "pass" AND no "fail" in the window; ambiguous verdicts
	// (e.g. "OVERALL: PASS with failing tests") default to fail.
	if strings.Contains(window, "pass") && !strings.Contains(window, "fail") {
		return true
	}
	return false
}

// parseCriteriaResults tries to match LLM output lines to acceptance criteria.
//
// Each criterion defaults to Pass=false ("treat as failure" per principles).
// A criterion only flips to Pass=true when we find explicit PASS evidence in
// the LLM report. Previously every criterion was pre-seeded as Pass=true with
// "Not explicitly tested" evidence, which silently approved work items when
// the LLM report was empty or didn't mention a specific criterion.
func parseCriteriaResults(report string, criteria []string) []criterionResult {
	lc := strings.ToLower(report)
	results := make([]criterionResult, 0, len(criteria))
	for _, criterion := range criteria {
		cr := criterionResult{
			Criterion: criterion,
			Pass:      false,
			Evidence:  "Not explicitly tested — treat as failure.",
		}
		criterionLC := strings.ToLower(criterion)
		idx := strings.Index(lc, criterionLC)
		if idx >= 0 {
			// Grab surrounding context (up to 300 chars) for evidence.
			start := idx
			if start > 100 {
				start = idx - 100
			}
			end := idx + 200
			if end > len(report) {
				end = len(report)
			}
			snippet := report[start:end]
			snippetLC := strings.ToLower(snippet)
			cr.Evidence = strings.TrimSpace(snippet)
			// Only flip to Pass when there's an explicit PASS marker AND no
			// FAIL marker in the snippet. Ambiguous ("PASS with warnings")
			// counts as fail.
			hasPass := strings.Contains(snippetLC, "pass")
			hasFail := strings.Contains(snippetLC, "fail") && !strings.Contains(snippetLC, "not fail")
			if hasPass && !hasFail {
				cr.Pass = true
			}
		}
		results = append(results, cr)
	}
	return results
}

// extractCriteria splits acceptance criteria text into individual criteria.
func extractCriteria(rawCriteria, prdContent string) []string {
	if rawCriteria != "" {
		lines := strings.Split(rawCriteria, "\n")
		var criteria []string
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" {
				criteria = append(criteria, l)
			}
		}
		if len(criteria) > 0 {
			return criteria
		}
	}
	if prdContent != "" {
		return []string{
			"Implementation meets requirements described in PRD",
			"All edge cases are handled correctly",
			"Error paths are tested",
		}
	}
	return []string{"Basic functional verification"}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mainCloneHead returns HEAD of the clone QA is about to run in, for the
// report, or "" when it cannot be read.
func mainCloneHead(ctx context.Context, clonePath string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = clonePath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return gitref.Normalise(string(out))
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
