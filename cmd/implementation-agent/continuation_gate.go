package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/failures"
)

// Test gate after every continuation commit (R11 S3). A live run's (attempt 11)
// continuation added the test as its last step and committed; the final
// gate then failed `npm test` on that test (it set the env after the config
// module had read it). The pre-commit test + repair cycle ran only in the
// main loop, so the agent never had a chance to repair it. Now a
// continuation pass that produced edits runs the same pre-commit checks and
// bounded repair loop before its commit; when they still fail, the pass's
// uncommitted edits are reverted and the failure recorded, never committed.

// precommitInputs are the pre-commit repair loop's inputs for this
// attempt: the impact-selected fast checks when there are any, else the
// full test command. detailKey names its state in the run's detail.
func precommitInputs(in generationInputs, detailKey string) (repairInputs, string) {
	rin := repairInputs{
		logger: in.logger, taskID: in.taskID, workID: in.workID, worktree: in.worktreePath, command: in.testCommand,
		runChecks: func(c context.Context) ([]byte, error) { return runTests(c, in.worktreePath, in.testCommand) },
		llm:       in.llmAdapter, executor: in.executor, pool: in.pool, run: in.run, outputs: nil,
		budget: in.budget, plannedFiles: in.task.FilesToChange, detailKey: detailKey,
	}
	source := "full test command"
	if in.fast != nil {
		rin.runChecks, rin.results, rin.lang = in.fast.run, in.fast.failureResults, in.fast.language
		source = "impact-selected fast checks"
	}
	return rin, source
}

// continuationFailure is a continuation pass whose edits failed the
// pre-commit gate and were reverted.
type continuationFailure struct {
	Pass     int      `json:"pass"`
	Stop     string   `json:"stop"`
	Detail   string   `json:"detail,omitempty"`
	Repairs  int      `json:"repairs"`
	Reverted []string `json:"reverted"`
	// Fingerprints are the last failing run's failure records.
	Fingerprints []string `json:"fingerprints,omitempty"`
}

// summaryNote is the line the implementation summary carries for it.
func (f continuationFailure) summaryNote() string {
	return fmt.Sprintf("Continuation pass %d was not committed: its changes to %s failed the pre-commit checks (%s after %d repair attempt(s): %s) and were reverted.",
		f.Pass, strings.Join(f.Reverted, ", "), f.Stop, f.Repairs, truncate(f.Detail, 300))
}

// addRevertedContinuationChecks records each reverted continuation in the
// evidence as a structured check (R11 review): a test check, not_run on the
// verified head (the pass's code is not there), whose reason names the
// pass, the stop, the repairs, the failure fingerprints and the files not
// committed. A not_run check caps the outcome at partially_verified, so the
// verifier and review see the gap as data.
func addRevertedContinuationChecks(in finalizeInputs, ev *evidence.ImplementationEvidence) {
	for _, f := range in.continuationFailures {
		var fps []string
		for _, fp := range f.Fingerprints {
			fps = append(fps, failures.Short(fp))
		}
		reason := fmt.Sprintf("continuation %d reverted, not committed: pre-commit checks %s after %d repair attempt(s)", f.Pass, f.Stop, f.Repairs)
		if len(fps) > 0 {
			reason += " [" + strings.Join(fps, ", ") + "]"
		}
		reason += "; files not changed: " + strings.Join(f.Reverted, ", ")
		ev.Checks = append(ev.Checks, evidence.CheckResult{
			Kind:       evidence.CheckKindTest,
			Command:    in.testCommand,
			Status:     evidence.CheckNotRun,
			SkipReason: reason,
		})
	}
}

// gateContinuation runs the pre-commit checks and the bounded repair loop
// over continuation pass's uncommitted edits. It returns the loop's result
// and, when the checks did not pass for a reason other than the budget,
// the recorded failure after reverting the pass. before are the paths
// already uncommitted before the pass (they are not reverted).

func gateContinuation(ctx context.Context, in generationInputs, sink outputStorer, pass int, before map[string]bool) (repairResult, *continuationFailure) {
	// Stage so the checks (and the fast-check diff) see the new files; a
	// refused symlink is the commit's business (commitContinuation).
	_ = stageAll(ctx, in.worktreePath)
	rin, source := precommitInputs(in, fmt.Sprintf("continuation_%d_repair", pass))
	rin.outputs = sink
	in.logger.Info(ctx, "running tests before the continuation commit", map[string]interface{}{"task_id": in.taskID.String(), "continuation": pass, "command": in.testCommand, "source": source})
	rr := runRepairLoop(ctx, rin)
	if rr.passed || rr.budget != nil {
		return rr, nil
	}
	changed := passPaths(ctx, in.worktreePath, before)
	revErr := revertUncommitted(ctx, in.worktreePath, before)
	f := &continuationFailure{Pass: pass, Stop: rr.stop, Detail: rr.stopDetail, Repairs: rr.state.Repairs, Reverted: changed, Fingerprints: fingerprintsOf(rr.lastRecords)}
	note := fmt.Sprintf("continuation %d reverted: pre-commit checks %s", pass, rr.stop)
	if revErr != nil {
		note += fmt.Sprintf(" (revert incomplete: %v)", revErr)
	}
	in.run.Append(ctx, controller.EventRow{Kind: controller.KindNote, Note: note, Detail: mustJSON(f)})
	in.run.MergeDetail(ctx, fmt.Sprintf("continuation_%d_reverted", pass), f)
	in.logger.Warn(ctx, "continuation failed its pre-commit checks — reverted, not committed", map[string]interface{}{
		"task_id": in.taskID.String(), "continuation": pass, "stop": rr.stop, "detail": truncate(rr.stopDetail, 300), "reverted": changed,
	})
	return rr, f
}

// untrackedPaths are the worktree's untracked, not ignored files.
func untrackedPaths(ctx context.Context, worktree string) map[string]bool {
	out := map[string]bool{}
	s, _ := guardGit(ctx, worktree, "ls-files", "--others", "--exclude-standard", "-z")
	for _, p := range strings.Split(s, "\x00") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// uncommittedPaths are the files that differ from HEAD (staged or not) and
// the untracked ones, sorted as git lists them.
func uncommittedPaths(ctx context.Context, worktree string) []string {
	var out []string
	seen := map[string]bool{}
	tracked, _ := guardGit(ctx, worktree, "diff", "HEAD", "--name-only", "-z")
	for _, p := range strings.Split(tracked, "\x00") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for p := range untrackedPaths(ctx, worktree) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// uncommittedSet is uncommittedPaths as a set: what differs from HEAD
// before a pass (R11 review: those paths are not the pass's to revert).
func uncommittedSet(ctx context.Context, worktree string) map[string]bool {
	out := map[string]bool{}
	for _, p := range uncommittedPaths(ctx, worktree) {
		out[p] = true
	}
	return out
}

// passPaths are the uncommitted paths the pass changed: those not already
// uncommitted before it.
func passPaths(ctx context.Context, worktree string, before map[string]bool) []string {
	var out []string
	for _, p := range uncommittedPaths(ctx, worktree) {
		if !before[p] {
			out = append(out, p)
		}
	}
	return out
}

// revertUncommitted reverts the paths the pass changed, and only those: a
// file in HEAD is restored from HEAD (index and worktree), a new one is
// unstaged and removed. Paths already uncommitted before the pass (in
// before) are left as they are, only unstaged again. It returns what could
// not be reverted.
func revertUncommitted(ctx context.Context, worktree string, before map[string]bool) error {
	var errs []string
	git := func(args ...string) {
		if _, err := guardGit(ctx, worktree, append([]string{"--literal-pathspecs"}, args...)...); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, p := range uncommittedPaths(ctx, worktree) {
		if before[p] {
			if _, err := guardGit(ctx, worktree, "cat-file", "-e", "HEAD:"+p); err == nil {
				git("reset", "-q", "HEAD", "--", p)
			} else {
				git("rm", "-q", "--cached", "--ignore-unmatch", "--", p)
			}
			continue
		}
		if _, err := guardGit(ctx, worktree, "cat-file", "-e", "HEAD:"+p); err == nil {
			git("checkout", "HEAD", "--", p)
			continue
		}
		git("rm", "-q", "--cached", "--ignore-unmatch", "--", p)
		if err := os.Remove(filepath.Join(worktree, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
