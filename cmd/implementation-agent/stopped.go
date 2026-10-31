package main

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// A stopped attempt (M3-06): a D4 budget ran out, or the repair loop gave
// up (repeated_failure, repairs_exhausted). The attempt never reaches the
// final gate and never pushes. Its work is committed to the agent branch
// locally (preserved, not released), and the outcome is the FAILURE path:
// failed evidence (the gate's test check not_run; the last failing
// pre-commit checks when they describe the committed tree; for a budget, a
// failed "implementation budget (D4)" check), the work item blocked with
// an escalation prompt that lists the gathered failure records (top 5
// fingerprints), implementation.complete Success=false and the assignment
// failed — committed in one transaction like every other outcome.

const (
	// budgetSkipReason is the gate checks' skip_reason on a budget stop.
	budgetSkipReason = "budget exhausted"
	// budgetCheckCommand names the run-level failure check of a budget stop.
	budgetCheckCommand = "implementation budget: "
	// escalationTopFingerprints is how many failure records the
	// escalation prompt lists.
	escalationTopFingerprints = 5

	triggerBudgetExhausted interactions.Trigger = "budget:exhausted"
	triggerRepairStopped   interactions.Trigger = "repair:stopped"
)

// stopInfo says why generation stopped the attempt.
type stopInfo struct {
	// reason is stopBudgetExhausted, stopRepeatedFailure or
	// stopRepairsExhausted.
	reason string
	detail string
	budget *budgetExhaustedError
	// repair is the pre-commit loop's result when it ran.
	repair *repairResult
	// stuck are the tool loops' LoopStuck failures (stopLoopStuck, R10).
	stuck []tools.LoopStuck
}

// text is the operator-facing reason: "budget_exhausted: tokens 512000 of
// 500000 tokens used", "repeated_failure: 3f2a9c1b0d4e failed again …".
func (s stopInfo) text() string {
	if s.budget != nil {
		return s.budget.Error()
	}
	if s.detail == "" {
		return s.reason
	}
	return s.reason + ": " + s.detail
}

func budgetStopInfo(e *budgetExhaustedError, rr *repairResult) *stopInfo {
	return &stopInfo{reason: stopBudgetExhausted, detail: e.detail, budget: e, repair: rr}
}

// stopContext is what finalizeStopped needs beyond finalizeInputs.
type stopContext struct {
	productID *uuid.UUID
	workID    uuid.UUID
	budget    *attemptBudget
	// attempt names the preserved ref refs/ae/wip/<task8>/<attempt>.
	attempt int
}

// finalizeStopped commits the work in progress to the agent branch (not
// pushed) and reports the stopped attempt's failure outcome. It never
// returns success.
func finalizeStopped(ctx context.Context, in finalizeInputs, st stopInfo, sc stopContext, sink completionSink) {
	in = applyPinnedBase(ctx, in)
	in = applyVerificationPolicy(ctx, in)
	startedAt := time.Now().UTC()
	reasonText := st.text()
	committed, commitErr := commitWorkInProgress(ctx, in, reasonText)
	head, _ := gitRead(ctx, in.worktreePath, "rev-parse", "HEAD")
	files := sortedKeys(committedChangedFiles(ctx, in.worktreePath, in.baseSHA, in.baseBranch))

	ev := &evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        in.task.WorkID,
		TaskID:        in.task.TaskID,
		RepositoryID:  optionalString(in.task.RepositoryID),
		BaseSHA:       in.baseSHA,
		HeadSHA:       head,
		Branch:        in.subtaskBranch,
		ChangedFiles:  files,
		StartedAt:     startedAt,
		ArtifactRefs:  []string{},
	}
	skip := budgetSkipReason
	switch {
	case st.reason == stopLoopStuck:
		skip = "loop stuck: " + truncate(st.detail, 200)
	case st.budget == nil:
		skip = "repair stopped: " + st.reason
	}
	// The final gate never ran.
	ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindTest, Command: in.testCommand, Status: evidence.CheckNotRun, SkipReason: skip})
	// The last failing pre-commit run, when nothing has changed the tree
	// since (it is then exactly the committed head's tree).
	if rr := st.repair; rr != nil && !rr.passed && rr.lastRunCurrent && len(rr.lastResults) > 0 {
		var failed []evidence.CheckResult
		if in.fast != nil {
			failed = in.fast.failedChecks(rr.lastRawRef)
		}
		if len(failed) == 0 {
			failed = []evidence.CheckResult{{Kind: evidence.CheckKindTest, Command: in.testCommand, ExitCode: firstExit(rr.lastResults), Status: evidence.CheckFailed, OutputRef: rr.lastRawRef}}
		}
		ev.Checks = append(ev.Checks, failed...)
		if rr.lastRawRef != "" {
			ev.ArtifactRefs = append(ev.ArtifactRefs, rr.lastRawRef)
		}
	}
	if st.budget != nil {
		ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindOther, Command: budgetCheckCommand + st.budget.kind, Status: evidence.CheckFailed})
	}
	if !hasFailedCheck(ev.Checks) {
		ev.Checks = append(ev.Checks, evidence.CheckResult{Kind: evidence.CheckKindOther, Command: "pre-commit repair loop", Status: evidence.CheckFailed})
	}

	where := fmt.Sprintf("work in progress committed (not pushed) on branch %s at %s in %s", in.subtaskBranch, shortSHA(head), in.clonePath)
	switch {
	case commitErr != nil:
		where = fmt.Sprintf("work in progress could NOT be committed (%v); the worktree is removed when the attempt ends", commitErr)
	case !committed && len(files) == 0:
		where = "no work in progress to preserve"
	default:
		// A retry recreates the agent branch (createIsolatedWorktree
		// deletes it); the WIP commit survives under refs/ae/wip/.
		ref, err := preserveCommit(ctx, in.worktreePath, wipRefName(shortID(in.taskID), sc.attempt), head)
		if err != nil {
			in.logger.Warn(ctx, "cannot preserve the work in progress under refs/ae/wip", map[string]interface{}{"task_id": in.taskID.String(), "err": err.Error()})
			break
		}
		ev.ArtifactRefs = append(ev.ArtifactRefs, "git_ref:"+ref)
		where += "; preserved at " + ref
	}
	reason := fmt.Sprintf("%s — %s", reasonText, where)
	sealed := sealEvidence(ctx, in, ev, reason)

	var tracker *failures.Tracker
	if st.repair != nil {
		tracker = st.repair.tracker
	}
	if len(st.stuck) > 0 && (tracker == nil || tracker.Runs() == 0) {
		tracker = stuckTracker(st.stuck)
	}
	prompt := stopEscalationPrompt(in, st, sc, tracker, head, where)

	in.logger.Warn(ctx, "attempt stopped — reporting failure and blocking", map[string]interface{}{
		"task_id": in.taskID.String(), "reason": reasonText, "head_sha": head, "branch": in.subtaskBranch, "committed": committed,
	})
	msg := implementationCompleteMessage{
		TaskID:       in.task.TaskID,
		WorkID:       in.task.WorkID,
		FilesChanged: files,
		TestResults:  "not_run: " + skip,
		Summary:      in.summary,
		HeadSHA:      head,
		BaseSHA:      in.baseSHA,
		Branch:       in.subtaskBranch,
		Success:      false,
		Verified:     false,
		Reason:       reason,
		Evidence:     sealed,
	}
	reportOutcome(sink, completionOutcome{
		evidence:      persistableEvidence(in, sealed),
		evidenceStore: in.evidenceStore,
		prompt:        escalationPrompt(prompt, sc.productID),
		blockReason:   reason,
		msg:           &msg,
		assignment:    models.AssignmentStatusFailed,
		baseEvents:    in.baseEvents,
	})
}

// wipRefPrefix namespaces preserved work in progress. Refs under it are
// never pushed (only the agent branch is) and survive the agent branch
// being deleted and recreated by a retry.
const wipRefPrefix = "refs/ae/wip/"

// wipRefName is the preserved ref of a task attempt's work:
// refs/ae/wip/<task8>/<attempt>.
func wipRefName(task8 string, attempt int) string {
	return fmt.Sprintf("%s%s/%d", wipRefPrefix, task8, attempt)
}

// runAttempt is run's attempt number (0 without a run).
func runAttempt(run *controller.Run) int {
	if run == nil {
		return 0
	}
	return run.Attempt
}

type wipRefKey struct{}

// withWIPRef tells createIsolatedWorktree which ref to preserve an
// existing local-only agent branch at (the previous attempt's ref).
func withWIPRef(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, wipRefKey{}, ref)
}

// wipRefFrom is the ref set by withWIPRef, or one derived from the branch
// (agent/<work>/<task8> → refs/ae/wip/<task8>/prev) when none was set.
func wipRefFrom(ctx context.Context, branch string) string {
	if ref, ok := ctx.Value(wipRefKey{}).(string); ok && ref != "" {
		return ref
	}
	seg := branch
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		seg = branch[i+1:]
	}
	return wipRefPrefix + seg + "/prev"
}

// preserveLocalBranch points ref at branch's tip when the branch exists
// and its tip is on no remote-tracking ref (local-only work a delete would
// lose). It returns the ref written ("" when nothing needed preserving).
// An existing ref at a different commit is not overwritten: the tip is
// kept at ref-<sha8> instead.
func preserveLocalBranch(ctx context.Context, dir, branch, ref string) (string, error) {
	tip, err := gitRead(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil || tip == "" {
		return "", nil // no such branch
	}
	if remote, _ := gitRead(ctx, dir, "for-each-ref", "--contains", tip, "--format=%(refname)", "refs/remotes"); strings.TrimSpace(remote) != "" {
		return "", nil // pushed: the remote keeps it
	}
	return preserveCommit(ctx, dir, ref, tip)
}

// preserveCommit points ref at sha, never moving an existing ref that
// points elsewhere (it then writes ref-<sha8>).
func preserveCommit(ctx context.Context, dir, ref, sha string) (string, error) {
	if len(sha) < 8 {
		return "", fmt.Errorf("preserve %s: no commit to preserve (sha %q)", ref, sha)
	}
	if cur, err := gitRead(ctx, dir, "rev-parse", "--verify", "--quiet", ref); err == nil && cur != "" {
		if cur == sha {
			return ref, nil
		}
		ref = ref + "-" + sha[:8]
	}
	cmd := exec.CommandContext(ctx, "git", "update-ref", ref, sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git update-ref %s: %v: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return ref, nil
}

// commitWorkInProgress commits everything in the worktree to the agent
// branch. It never pushes.
func commitWorkInProgress(ctx context.Context, in finalizeInputs, reason string) (bool, error) {
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = in.worktreePath
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	if err := stageAll(ctx, in.worktreePath); err != nil {
		return false, err
	}
	status, err := run("status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	msg := fmt.Sprintf("wip: %s (stopped, not verified, not pushed)\n\n%s\n\nWork item: %s\nTask: %s", in.task.Title, truncate(reason, 500), in.task.WorkID, in.task.TaskID)
	if _, err := run("-c", "user.email=agent@autonomous-engine.local", "-c", "user.name=Autonomous Engine Agent", "commit", "-q", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

// stopEscalationPrompt is the prompt raised with the block. Its question
// lists the top failure fingerprints (most frequent first) and the budget.
func stopEscalationPrompt(in finalizeInputs, st stopInfo, sc stopContext, tracker *failures.Tracker, head, where string) *interactions.Prompt {
	trigger := triggerRepairStopped
	headline := "Implementation stopped: " + strings.ReplaceAll(st.reason, "_", " ")
	if st.budget != nil {
		trigger = triggerBudgetExhausted
		headline = fmt.Sprintf("Implementation stopped: %s budget exhausted", strings.ReplaceAll(st.budget.kind, "_", " "))
	}
	var top []failures.Entry
	if tracker != nil {
		top = tracker.Top(escalationTopFingerprints)
	}
	var q strings.Builder
	fmt.Fprintf(&q, "The implementation attempt for task %q stopped: %s.\n\n", in.task.Title, st.text())
	fmt.Fprintf(&q, "Nothing was pushed or released; %s.\n\n", where)
	q.WriteString(formatTopFailures(top))
	if sc.budget != nil {
		s := sc.budget.snapshot()
		fmt.Fprintf(&q, "\nBudget: tokens %d / %d · active %s / %s (%s awaiting user input excluded).\n",
			s.TokensUsed, s.TokenLimit, secondsDur(s.ActiveSeconds), secondsDur(s.ActiveLimitSeconds), secondsDur(s.PausedSeconds))
	}
	if st.budget != nil && st.budget.kind == budgetKindTokens {
		q.WriteString("\nThe token budget is per work item, not per attempt: raise WORK_ITEM_TOKEN_BUDGET before Retry, or the new attempt stops at its first model call.")
	}
	q.WriteString("\nRetry starts a new attempt (a fresh worktree from the base and a fresh active-time budget; the preserved ref keeps this attempt's work); cancel ends the work item.")

	fps := make([]map[string]any, 0, len(top))
	for _, e := range top {
		fps = append(fps, map[string]any{
			"fingerprint": e.Record.Fingerprint, "count": e.Count, "category": string(e.Record.Category),
			"file": e.Record.File, "line": e.Record.Line, "test": e.Record.Test, "message": truncate(e.Record.Message, 300),
			"ownership": string(e.Record.Ownership), "last_call_id": e.LastCallID,
		})
	}
	payload := map[string]any{
		"stop_reason": st.reason, "stop_detail": st.detail, "task_id": in.taskID.String(),
		"branch": in.subtaskBranch, "head_sha": head, "fingerprints": fps,
	}
	if st.budget != nil {
		payload["budget_kind"] = st.budget.kind
	}
	if sc.budget != nil {
		payload["budget"] = sc.budget.snapshot()
	}
	return &interactions.Prompt{
		ID:             uuid.New(),
		WorkItemID:     sc.workID,
		AskingAgent:    agentID,
		Trigger:        trigger,
		PromptType:     interactions.PromptTypeEscalation,
		Headline:       headline,
		Question:       q.String(),
		ContextPayload: payload,
		Actions: []interactions.Action{
			{ID: "retry", Label: "Retry with a fresh attempt", Semantic: interactions.SemanticRetry},
			{ID: "cancel", Label: "Cancel this work item", Semantic: interactions.SemanticCancel},
		},
		State:       interactions.StatePending,
		RequestedAt: time.Now().UTC(),
		PromptedVia: []string{"dashboard"},
	}
}

// formatTopFailures renders the escalation prompt's failure list:
//
//	Top failure records (fingerprint · runs · category · location · message):
//	1. 3f2a9c1b0d4e · 2 runs · test_assert · calc_test.go:7 (TestAdd) · Add(2, 3) = -1, want 5
func formatTopFailures(top []failures.Entry) string {
	if len(top) == 0 {
		return "No failure records were gathered before the attempt stopped.\n"
	}
	var b strings.Builder
	b.WriteString("Top failure records (fingerprint · runs · category · location · message):\n")
	for i, e := range top {
		loc := e.Record.Location()
		if loc == "" {
			loc = "-"
		}
		if e.Record.Test != "" {
			loc += " (" + e.Record.Test + ")"
		}
		runs := "run"
		if e.Count != 1 {
			runs = "runs"
		}
		fmt.Fprintf(&b, "%d. %s · %d %s · %s · %s · %s\n", i+1, failures.Short(e.Record.Fingerprint), e.Count, runs, e.Record.Category, loc, truncate(firstLine(e.Record.Message), 200))
	}
	return b.String()
}

func secondsDur(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(no commit)"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
