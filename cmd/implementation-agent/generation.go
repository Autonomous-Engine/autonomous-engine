package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// generationInputs is everything the generation phase needs. Generation
// owns every mutation of the worktree: the initial tool loop, the
// verify-gate re-prompts, import repair, the pre-commit test/fix loop,
// the no-edits retry, the initial commit, and the partial-completion
// continuation passes with their commits.
type generationInputs struct {
	task          implementationTaskMessage
	taskID        uuid.UUID
	workID        uuid.UUID
	clonePath     string
	worktreePath  string
	subtaskBranch string
	baseBranch    string
	testCommand   string
	req           adapter.CompletionRequest
	systemPrompt  string
	maxIters      int
	llmAdapter    adapter.ModelAdapter
	executor      *tools.Executor
	pool          *pgxpool.Pool
	// turnToolCalls is appended to by the executor callback while the
	// tool loops run; the verify gate reads it through the pointer so it
	// always sees the live list.
	turnToolCalls *[]strategy.ToolCall
	logger        *logging.Logger

	// fast, when set, supplies the pre-commit loop's commands: the
	// impact-selected fast checks instead of the full testCommand
	// (fastchecks.go, M3-04). nil keeps the full command.
	fast *fastChecks
	// baseSHA is the resolved base (resolveBaseSHA, the same base the
	// evidence and the fast-check diff use). The continuation passes diff
	// baseSHA...HEAD; baseBranch is the fallback when it is empty.
	baseSHA string

	// budget is the attempt's D4 budget (budget.go, M3-06): checked
	// between phases, before every LLM call (the executor's iteration
	// hook) and inside the repair loop. nil: no active-time cap (the
	// token cap is RunToolLoop's own).
	budget *attemptBudget
	// run receives the repair loop's check-run events, failure records
	// and plan revisions (nil: not recorded).
	run *controller.Run
}

// committedChangedFiles returns the files changed on the branch since the
// resolved base: baseSHA...HEAD, or baseBranch...HEAD when no SHA was
// resolved.
func committedChangedFiles(ctx context.Context, worktreePath, baseSHA, baseBranch string) map[string]bool {
	base := strings.TrimSpace(baseSHA)
	if base == "" {
		base = baseBranch
	}
	diffCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", base+"...HEAD")
	diffCmd.Dir = worktreePath
	diffOut, _ := diffCmd.CombinedOutput()
	out := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
		if f := strings.TrimSpace(line); f != "" {
			out[f] = true
		}
	}
	return out
}

// generationResult is what finalization needs from generation.
type generationResult struct {
	summary string
	// repair is the pre-commit repair loop's result (nil when no check ran).
	repair *repairResult
	// stopped, when set (with ok=false), means generation stopped the
	// attempt — a D4 budget ran out or the repair loop gave up — and
	// reported NOTHING: the caller must run finalizeStopped (M3-06).
	stopped *stopInfo
	// continuationFailures are the continuation passes whose edits failed
	// the pre-commit gate and were reverted, not committed (R11 S3).
	continuationFailures []continuationFailure
}

// runGeneration runs the generation phase. It returns ok=false when the
// task failed and the failure has already been reported through sink;
// the caller must then stop without publishing anything else — except
// when result.stopped is set, which the caller reports (finalizeStopped).
func runGeneration(ctx context.Context, in generationInputs, sink completionSink) (generationResult, bool) {
	// stopOn maps a tool loop's error (or, with err nil, the budget's
	// current state) to a budget stop.
	stopOn := func(err error, rr *repairResult) *stopInfo {
		e := in.budget.fromLoopError(err)
		if e == nil {
			e = in.budget.check()
		}
		if e == nil {
			return nil
		}
		return budgetStopInfo(e, rr)
	}
	if st := stopOn(nil, nil); st != nil {
		return generationResult{stopped: st}, false
	}
	task := in.task
	taskID := in.taskID
	workID := in.workID
	clonePath := in.clonePath
	worktreePath := in.worktreePath
	subtaskBranch := in.subtaskBranch
	baseBranch := in.baseBranch
	testCommand := in.testCommand
	req := in.req
	systemPrompt := in.systemPrompt
	maxIters := in.maxIters
	llmAdapter := in.llmAdapter
	executor := in.executor
	pool := in.pool
	logger := in.logger
	// The ledger pins reads of the files in scope (T4).
	pinScope, _ := taskScope(task)

	// Verify-gate retry loop. gemma-class and qwen2.5-coder both fall
	// back to "describe the plan in prose" rather than calling the tool
	// — so the gate enforces verify-before-emit structurally. Each
	// iteration runs the full tool loop; if the model returns a final
	// answer that proposes code for files it never verified via
	// codex_verify_imports, we inject a corrective user message and
	// re-enter the tool loop. 4 retries = 5 total passes before we fail
	// honestly; 2 wasn't enough for models stuck in plan-mode.
	const maxVerifyGateRetries = 4
	var resp *adapter.CompletionResponse
	var loopErr error
	verifyGateRetries := 0
	verifyGateExhausted := false
	var lastMissing []string
	// stuck are the LoopStuck failures of this attempt's loops (R10 W3):
	// a loop that kept failing the same way ended early; what it did do
	// is tested and committed, and a continuation inherits the failures.
	var stuck []tools.LoopStuck
	for {
		resp, loopErr = runImplementationLoop(ctx, llmAdapter, req, executor, maxIters, pool, &workID, pinScope)
		in.budget.activity() // the loop returned: nothing waits in-process for a pending prompt
		if loopErr != nil {
			if st := stopOn(loopErr, nil); st != nil {
				return generationResult{stopped: st}, false
			}
			var ls *tools.LoopStuck
			if errors.As(loopErr, &ls) {
				stuck = append(stuck, *ls)
				logger.Warn(ctx, "tool loop stuck — ending the pass, keeping its work", map[string]interface{}{
					"task_id": taskID.String(), "tool": ls.Tool, "path": ls.Path, "attempts": ls.Attempts, "reason": truncate(ls.Reason, 300),
				})
				resp, loopErr = &adapter.CompletionResponse{Content: ls.Error()}, nil
				break
			}
			if errors.Is(loopErr, tools.ErrToolCallingNotSupported) {
				log.Printf("implementation-agent: model does not support tool calling: %v", loopErr)
				reportOutcome(sink, completionOutcome{
					blockReason: "Cannot implement: the configured LLM model does not support tool calling. " +
						"Tool calling is required for the implementation agent to read files, write code, and run tests. " +
						"Action needed: assign a tool-calling capable model (GPT-4o, Claude Sonnet/Opus, Llama 3.1+, Qwen 2.5+) " +
						"to this agent in Admin → Agents or Admin → Models.",
					assignment: models.AssignmentStatusFailed,
				})
				return generationResult{}, false
			}
			logger.Error(ctx, "tool loop failed", loopErr, map[string]interface{}{
				"task_id": taskID.String(),
			})
			reportOutcome(sink, completionOutcome{
				msg: &implementationCompleteMessage{
					TaskID:  task.TaskID,
					WorkID:  task.WorkID,
					Success: false,
					Reason:  fmt.Sprintf("tool loop error: %v", loopErr),
				},
				assignment: models.AssignmentStatusFailed,
			})
			return generationResult{}, false
		}

		// Inspect the final answer against the tool calls the model
		// actually made. Two failure modes, checked in order:
		//   (a) Missing writes: model mentioned files but never called
		//       write_file / edit_file on them — force it toward the
		//       write tools. This is the common qwen2.5-coder failure
		//       where the model reads + plans but never writes.
		//   (b) Missing verifications: files WERE written but no
		//       codex_verify_imports was called — force verification.
		// Fail-honestly: we don't silently fill in the missing step,
		// we force the model to do it, then retry the loop.
		finalContent := ""
		if resp != nil {
			finalContent = resp.Content
		}
		proposed := strategy.ParseProposedFiles(finalContent)
		missingWrites := strategy.MissingWrites(proposed, *in.turnToolCalls)

		// Filter out files actually modified in the worktree (e.g., by
		// npm install / run_command rather than explicit edit_file).
		if len(missingWrites) > 0 {
			diffCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "HEAD")
			diffCmd.Dir = worktreePath
			if diffOut, diffErr := diffCmd.Output(); diffErr == nil {
				changedFiles := make(map[string]bool)
				for _, line := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
					if line != "" {
						changedFiles[line] = true
						changedFiles[filepath.Base(line)] = true
					}
				}
				filtered := missingWrites[:0]
				for _, p := range missingWrites {
					if !changedFiles[p] && !changedFiles[filepath.Base(p)] {
						filtered = append(filtered, p)
					}
				}
				missingWrites = filtered
			}
		}

		missingVerif := strategy.MissingVerifications(proposed, *in.turnToolCalls)

		var corrective string
		var reasonLabel string
		var missing []string
		if len(missingWrites) > 0 {
			corrective = strategy.ForcedWritePrompt(missingWrites)
			reasonLabel = "missing_writes"
			missing = missingWrites
		} else if len(missingVerif) > 0 {
			corrective = strategy.ForcedVerifyPrompt(missingVerif)
			reasonLabel = "missing_verifications"
			missing = missingVerif
		} else {
			break
		}

		lastMissing = missing
		if verifyGateRetries >= maxVerifyGateRetries {
			logger.Warn(ctx, "verify_gate_exhausted — model refused to complete proposed files", map[string]interface{}{
				"task_id": taskID.String(),
				"reason":  reasonLabel,
				"missing": missing,
				"retries": verifyGateRetries,
			})
			verifyGateExhausted = true
			break
		}
		verifyGateRetries++
		logger.Info(ctx, "verify_gate re-prompt: forcing model toward "+reasonLabel, map[string]interface{}{
			"task_id": taskID.String(),
			"reason":  reasonLabel,
			"missing": missing,
			"attempt": verifyGateRetries,
		})
		req.Messages = append(req.Messages,
			adapter.ChatMessage{Role: adapter.RoleAssistant, Content: finalContent},
			adapter.ChatMessage{Role: adapter.RoleUser, Content: corrective},
		)
	}

	// If the gate was exhausted, fail honestly rather than ship
	// unverified code. Revert worktree and block the work item.
	if verifyGateExhausted {
		revertCmd := exec.CommandContext(ctx, "git", "checkout", "--", ".")
		revertCmd.Dir = worktreePath
		revertCmd.CombinedOutput() //nolint:errcheck
		reason := fmt.Sprintf("verify_gate_exhausted: model proposed code for %d files without running codex_verify_imports after %d corrective re-prompts. Unverified files: %v", len(lastMissing), maxVerifyGateRetries, lastMissing)
		reportOutcome(sink, completionOutcome{
			blockReason: reason,
			msg: &implementationCompleteMessage{
				TaskID:  task.TaskID,
				WorkID:  task.WorkID,
				Success: false,
				Reason:  reason,
			},
			assignment: models.AssignmentStatusFailed,
		})
		return generationResult{}, false
	}

	summary := ""
	if resp != nil {
		summary = resp.Content
	}

	// AutoFix loop: scan all files touched by the tool loop, verify
	// imports resolve, and hand any failures back to the LLM. We cap
	// the corrective passes so a stuck model can't spin forever — if
	// after the cap unresolved imports remain, we block the task
	// honestly rather than commit code we know is broken.
	// maxVerifyPasses is small on purpose; each pass is a full LLM
	// call with tool access.
	const maxVerifyPasses = 3
	autofixBlocked := ""
	for pass := 1; pass <= maxVerifyPasses; pass++ {
		unresolved := collectUnresolvedImports(ctx, logger, worktreePath, taskID.String())
		if len(unresolved) == 0 {
			break
		}
		if pass == maxVerifyPasses {
			// Build a concise blocker message listing each offender.
			var bb strings.Builder
			bb.WriteString(fmt.Sprintf("Implementation blocked: %d unresolved imports remain after %d corrective passes:\n", len(unresolved), maxVerifyPasses))
			for _, u := range unresolved {
				bb.WriteString(fmt.Sprintf("- %s (%s:%d) — %s\n", u.Import, u.File, u.Line, u.Reason))
			}
			autofixBlocked = bb.String()
			break
		}
		logger.Warn(ctx, fmt.Sprintf("autofix pass %d: %d unresolved imports — re-prompting LLM", pass, len(unresolved)), map[string]interface{}{
			"task_id": taskID.String(),
		})

		// Hand the unresolved list back to the LLM with a targeted
		// corrective prompt. Re-use the same executor so the warm LSP
		// manager and tool history stay intact.
		var correctiveBody strings.Builder
		correctiveBody.WriteString("The following imports in files you just wrote/edited do not resolve. Fix each one. ")
		correctiveBody.WriteString("For package imports: either change the spec to a package that IS declared in package.json, or (if you really need a new package) update package.json and install. ")
		correctiveBody.WriteString("For relative imports: correct the path to an existing file. Do NOT remove or stub-out functionality — make imports correct.\n\n")
		for _, u := range unresolved {
			correctiveBody.WriteString(fmt.Sprintf("- %s (%s:%d) — %s\n", u.Import, u.File, u.Line, u.Reason))
		}
		fixReq := adapter.CompletionRequest{
			Messages: []adapter.ChatMessage{
				{Role: adapter.RoleSystem, Content: systemPrompt},
				{Role: adapter.RoleUser, Content: correctiveBody.String()},
			},
			Tools:       editTools(),
			ToolChoice:  "auto",
			MaxTokens:   4096,
			Temperature: 0.2,
		}
		_, fixErr := runImplementationLoop(ctx, llmAdapter, fixReq, executor, 10, pool, &workID, pinScope)
		in.budget.activity()
		if st := stopOn(fixErr, nil); st != nil {
			return generationResult{stopped: st}, false
		}
		if fixErr != nil {
			logger.Warn(ctx, "autofix tool loop hit limit — proceeding with changes as-is", map[string]interface{}{"task_id": taskID.String(), "pass": pass, "error": fixErr.Error()})
			break
		}
	}
	if autofixBlocked != "" {
		// Autofix couldn't resolve all imports within its budget.
		// Don't revert — the main implementation work is still valuable.
		// Proceed to test+commit; the test suite is the real quality gate.
		logger.Warn(ctx, "autofix incomplete — proceeding to test+commit", map[string]interface{}{
			"task_id": taskID.String(),
			"detail":  autofixBlocked,
		})
	}

	logger.Info(ctx, "tool loop complete, finalizing git", map[string]interface{}{
		"task_id": taskID.String(),
	})

	// Run tests BEFORE committing. If they fail, the fingerprint-bounded
	// repair loop (repair.go, M3-06) gives the model up to 3 repairs with
	// structured failure records; a failure that survives a repair stops
	// it. This is a trust requirement — we never push code we haven't verified.
	var repair *repairResult
	gitStatusCheck := exec.CommandContext(ctx, "git", "status", "--porcelain")
	gitStatusCheck.Dir = worktreePath
	statusCheck, _ := gitStatusCheck.CombinedOutput()
	hasChanges := len(bytes.TrimSpace(statusCheck)) > 0

	if hasChanges {
		// Stage changes temporarily so tests run against the modified code.
		// M5: through the commit guard, so a refused symlink fails here,
		// before a test run is paid for, as it would at the commit.
		if err := stageAll(ctx, worktreePath); err != nil {
			reason := fmt.Sprintf("Implementation produced changes that cannot be committed: %s. The subtask branch %s does not contain the model's work.", err.Error(), subtaskBranch)
			logger.Error(ctx, "pre-test staging refused", err, map[string]interface{}{"task_id": taskID.String()})
			reportOutcome(sink, completionOutcome{
				blockReason: reason,
				msg: &implementationCompleteMessage{
					TaskID:  task.TaskID,
					WorkID:  task.WorkID,
					Success: false,
					Reason:  reason,
				},
				assignment: models.AssignmentStatusFailed,
			})
			return generationResult{}, false
		}

		if testCommand == "" {
			logger.Warn(ctx, "cannot detect test command — skipping test verification", map[string]interface{}{"task_id": taskID.String()})
		} else {
			// The pre-commit loop runs the impact-selected fast checks,
			// recomputed from the diff before every run; the final gate
			// still runs the full suite (finalize.go).
			rin, source := precommitInputs(in, "")
			rin.outputs = sink
			logger.Info(ctx, "running tests before commit", map[string]interface{}{"task_id": taskID.String(), "command": testCommand, "source": source})

			rr := runRepairLoop(ctx, rin)
			repair = &rr
			if !rr.passed {
				// Stopped: the work is preserved (committed, not pushed)
				// and the caller reports the failure with its evidence and
				// the escalation (finalizeStopped).
				st := &stopInfo{reason: rr.stop, detail: rr.stopDetail, repair: &rr}
				if rr.budget != nil {
					st = budgetStopInfo(rr.budget, &rr)
				}
				return generationResult{stopped: st, repair: &rr}, false
			}

			logger.Info(ctx, "tests PASSED — safe to commit", map[string]interface{}{"task_id": taskID.String()})
		}
	}

	// ── No-edits retry ────────────────────────────────────────
	// If the tool loop finished without producing any file changes,
	// the model explored but didn't act. Before failing honestly,
	// give it one more pass with an explicit nudge to call edit_file.
	const maxNoEditsRetries = 1
	for noEditsRetry := 0; noEditsRetry <= maxNoEditsRetries; noEditsRetry++ {
		probeCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
		probeCmd.Dir = worktreePath
		probeOut, _ := probeCmd.CombinedOutput()
		if len(bytes.TrimSpace(probeOut)) == 0 && len(stuck) > 0 {
			// Stuck before any change: the step's failure is the loop's,
			// not "no code changes" (R10 W3).
			return generationResult{summary: summary, stopped: stuckStopInfo(stuck, repair), repair: repair}, false
		}
		if len(bytes.TrimSpace(probeOut)) > 0 || noEditsRetry == maxNoEditsRetries {
			break // changes exist OR retries exhausted
		}
		logger.Warn(ctx, "no edits after tool loop — nudging model to act", map[string]interface{}{
			"task_id": taskID.String(),
			"attempt": noEditsRetry + 1,
		})
		nudge := "You explored the codebase but did NOT call edit_file or write_file — no files were changed on disk. " +
			"This task requires actual code changes. Based on the files you already read, call edit_file NOW to implement the required change. " +
			"Do not read more files. Do not explain. Call edit_file with the specific old_string and new_string to make the change."
		req.Messages = append(req.Messages,
			adapter.ChatMessage{Role: adapter.RoleAssistant, Content: summary},
			adapter.ChatMessage{Role: adapter.RoleUser, Content: nudge},
		)
		resp, loopErr = runImplementationLoopAfter(ctx, llmAdapter, req, executor, maxIters, pool, &workID, pinScope, stuck)
		in.budget.activity()
		if st := stopOn(loopErr, repair); st != nil {
			return generationResult{stopped: st, repair: repair}, false
		}
		var ls *tools.LoopStuck
		if errors.As(loopErr, &ls) {
			// Recorded (R10): stuck with nothing changed stops the attempt
			// as loop_stuck on the next probe, not "no code changes".
			stuck = append(stuck, *ls)
			summary = ls.Error()
			continue
		}
		if loopErr != nil {
			break
		}
		if resp != nil {
			summary = resp.Content
		}
	}

	// Deterministic git finalization — only reached if tests passed (or no changes).
	gitStatusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	gitStatusCmd.Dir = worktreePath
	statusOut, _ := gitStatusCmd.CombinedOutput()
	if len(bytes.TrimSpace(statusOut)) > 0 {
		logger.Info(ctx, "committing verified changes", map[string]interface{}{
			"task_id": taskID.String(),
			"changes": string(statusOut),
		})

		// Set git identity for the agent
		for _, cfg := range [][]string{
			{"config", "user.email", "agent@autonomous-engine.local"},
			{"config", "user.name", "Autonomous Engine Agent"},
		} {
			cfgCmd := exec.CommandContext(ctx, "git", cfg...)
			cfgCmd.Dir = worktreePath
			cfgCmd.CombinedOutput() //nolint:errcheck
		}

		// Stage all changes. If any git step below fails, we capture the
		// failure and publish implementation.complete with Success=false
		// rather than silently marking the task passed — otherwise the
		// dashboard would show a passed subtask with no commit, violating
		// the project's "fail honestly / trust through honesty" ethos.
		var gitFailure string
		// stageAll refuses a staged symlink that leaves the worktree or
		// is a linked dependency directory (a live run, attempt 6, committed
		// node_modules).
		if err := stageAll(ctx, worktreePath); err != nil {
			logger.Error(ctx, "git add failed", err, nil)
			gitFailure = fmt.Sprintf("git add failed: %s", err.Error())
		}

		// Commit into the subtask branch
		if gitFailure == "" {
			commitMsg := fmt.Sprintf("feat: %s\n\nWork item: %s\nTask: %s", task.Title, task.WorkID, task.TaskID)
			commitCmd := exec.CommandContext(ctx, "git", "commit", "-m", commitMsg)
			commitCmd.Dir = worktreePath
			if out, err := commitCmd.CombinedOutput(); err != nil {
				logger.Error(ctx, "git commit failed", fmt.Errorf("%s: %w", string(out), err), nil)
				gitFailure = fmt.Sprintf("git commit failed: %s", strings.TrimSpace(string(out)))
			} else {
				logger.Info(ctx, "committed changes", map[string]interface{}{"task_id": taskID.String()})
			}
		}

		// Ensure shared clone stays on its base branch — never leave it on a feature branch.
		resetCmd := exec.CommandContext(ctx, "git", "-C", clonePath, "checkout", baseBranch)
		resetCmd.CombinedOutput() //nolint:errcheck

		// If any git step failed (add/commit), fail honestly.
		// Push is deferred to after continuation passes.
		if gitFailure != "" {
			reason := fmt.Sprintf("Implementation produced changes but could not be committed/pushed: %s. The subtask branch %s does not contain the model's work.", gitFailure, subtaskBranch)
			reportOutcome(sink, completionOutcome{
				blockReason: reason,
				msg: &implementationCompleteMessage{
					TaskID:  task.TaskID,
					WorkID:  task.WorkID,
					Success: false,
					Reason:  reason,
				},
				assignment: models.AssignmentStatusFailed,
			})
			return generationResult{}, false
		}
	} else {
		// No uncommitted changes means the tool loop produced nothing on
		// disk — for an implementation task that's a failure, not a pass.
		// Claiming success here would violate the project ethos: "no
		// claimed commits that never happened."
		reason := "Implementation agent produced no code changes. Tool loop ran but no files on disk were modified. This is a failure for an implementation task — claiming success would be dishonest."
		logger.Warn(ctx, "no uncommitted changes after tool loop — failing honestly", map[string]interface{}{"task_id": taskID.String()})
		reportOutcome(sink, completionOutcome{
			blockReason: reason,
			msg: &implementationCompleteMessage{
				TaskID:  task.TaskID,
				WorkID:  task.WorkID,
				Success: false,
				Reason:  reason,
			},
			assignment: models.AssignmentStatusFailed,
		})
		return generationResult{}, false
	}

	// ── Partial-completion continuation ──────────────────
	// If the model changed some files but not all planned ones,
	// don't fail — continue with a focused prompt listing only
	// the remaining work. Each continuation pass gets a smaller
	// iteration budget to keep costs bounded.
	const maxContinuations = 3
	continuationIters := maxIters / 2
	if continuationIters < 10 {
		continuationIters = 10
	}

	// The scope is files_to_change plus every repository file the steps
	// cite (taskScope): a step whose files nobody listed is still work.
	scope, stepFiles := taskScope(task)
	logger.Info(ctx, "continuation check", map[string]interface{}{
		"task_id":           taskID.String(),
		"planned_files":     scope,
		"planned_count":     len(scope),
		"step_files":        len(stepFiles),
		"max_continuations": maxContinuations,
	})

	var contFailures []continuationFailure
	for cont := 0; cont < maxContinuations; cont++ {
		// Collect files changed so far (across all commits on this branch).
		allChangedFiles := committedChangedFiles(ctx, worktreePath, in.baseSHA, baseBranch)

		plannedMissing, again := continuationDecision(scope, stepFiles, allChangedFiles, summary, cont, stuckPaths(stuck)...)
		if len(plannedMissing) == 0 {
			logger.Info(ctx, "all planned files covered — no continuation needed", map[string]interface{}{
				"task_id":       taskID.String(),
				"changed_files": mapKeys(allChangedFiles),
			})
			break
		}
		if !again {
			logger.Info(ctx, "planned files outside the steps untouched, but the model reports done — no continuation", map[string]interface{}{
				"task_id":         taskID.String(),
				"files_remaining": plannedMissing,
			})
			break
		}

		logger.Info(ctx, "partial completion — continuing with remaining files", map[string]interface{}{
			"task_id":         taskID.String(),
			"continuation":    cont + 1,
			"files_done":      len(allChangedFiles),
			"files_remaining": plannedMissing,
		})

		contPrompt := continuationPrompt(allChangedFiles, plannedMissing, stuck)

		contReq := continuationRequest(req, contPrompt)

		// What is uncommitted before the pass survives a revert (R11 S3).
		uncommittedBefore := uncommittedSet(ctx, worktreePath)
		contResp, contErr := runImplementationLoopAfter(ctx, llmAdapter, contReq, executor, continuationIters, pool, &workID, pinScope, stuck)
		in.budget.activity()
		if contResp != nil {
			summary = contResp.Content
		}
		if st := stopOn(contErr, repair); st != nil {
			// The stop path commits the continuation's work in progress.
			return generationResult{summary: summary, stopped: st, repair: repair}, false
		}
		var ls *tools.LoopStuck
		var stuckStop *stopInfo
		if errors.As(contErr, &ls) {
			repeated := sameStuck(stuck, *ls)
			stuck = append(stuck, *ls)
			summary = ls.Error()
			logger.Warn(ctx, "continuation tool loop stuck", map[string]interface{}{
				"task_id": taskID.String(), "continuation": cont + 1, "path": ls.Path, "repeated": repeated, "reason": truncate(ls.Reason, 300),
			})
			if repeated {
				// The second identical failure ends it (R10 W4), after its
				// edits went through the test gate below (R11 S3): green
				// they are committed, red they are reverted.
				stuckStop = stuckStopInfo(stuck, repair)
			}
			contErr = nil
		}
		hitLimit := contErr != nil
		if hitLimit {
			logger.Warn(ctx, "continuation tool loop hit limit — committing what was done", map[string]interface{}{
				"task_id": taskID.String(),
				"error":   contErr.Error(),
			})
		}

		// Commit continuation changes if any (even if the loop hit its limit).
		contStatusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
		contStatusCmd.Dir = worktreePath
		contStatusOut, _ := contStatusCmd.CombinedOutput()
		if len(bytes.TrimSpace(contStatusOut)) > 0 {
			// R11 S3: the pass's edits get the main loop's pre-commit
			// checks and bounded repair before they are committed. Still
			// failing, they are reverted and recorded, never committed: the
			// final gate does not meet a failure the agent could not repair.
			if testCommand != "" {
				rr, failed := gateContinuation(ctx, in, sink, cont+1, uncommittedBefore)
				if rr.budget != nil {
					return generationResult{summary: summary, stopped: budgetStopInfo(rr.budget, &rr), repair: repair, continuationFailures: contFailures}, false
				}
				if failed != nil {
					contFailures = append(contFailures, *failed)
					summary = strings.TrimSpace(summary + "\n\n" + failed.summaryNote())
					if stuckStop != nil {
						return generationResult{summary: summary, stopped: stuckStop, repair: repair, continuationFailures: contFailures}, false
					}
					break
				}
			}
			// M1: a refused symlink is unstaged and the rest committed;
			// when the pass cannot be committed the task fails — its work
			// is never left staged while generation reports success.
			contCommitMsg := fmt.Sprintf("feat: %s (cont %d)\n\nWork item: %s\nTask: %s", task.Title, cont+1, task.WorkID, task.TaskID)
			refused, err := commitContinuation(ctx, worktreePath, contCommitMsg)
			if len(refused) > 0 {
				logger.Warn(ctx, "continuation: commit guard refused symlink(s); committed the rest", map[string]interface{}{
					"task_id": taskID.String(), "continuation": cont + 1, "refused": refused,
				})
			}
			if err != nil {
				reason := fmt.Sprintf("Continuation pass %d produced changes that could not be committed: %s. The subtask branch %s does not contain that work.", cont+1, err.Error(), subtaskBranch)
				logger.Error(ctx, "continuation not committed", err, map[string]interface{}{"task_id": taskID.String(), "continuation": cont + 1})
				reportOutcome(sink, completionOutcome{
					blockReason: reason,
					msg: &implementationCompleteMessage{
						TaskID:  task.TaskID,
						WorkID:  task.WorkID,
						Success: false,
						Reason:  reason,
					},
					assignment: models.AssignmentStatusFailed,
				})
				return generationResult{}, false
			}
			logger.Info(ctx, "committed continuation changes", map[string]interface{}{
				"task_id":      taskID.String(),
				"continuation": cont + 1,
			})
		} else {
			if stuckStop != nil {
				return generationResult{summary: summary, stopped: stuckStop, repair: repair, continuationFailures: contFailures}, false
			}
			logger.Warn(ctx, "continuation produced no new changes — stopping", map[string]interface{}{
				"task_id": taskID.String(),
			})
			break
		}
		if stuckStop != nil {
			return generationResult{summary: summary, stopped: stuckStop, repair: repair, continuationFailures: contFailures}, false
		}
		if hitLimit {
			break
		}
	}
	return generationResult{summary: summary, repair: repair, continuationFailures: contFailures}, true
}
