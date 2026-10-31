package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// A failing `git update-ref` in preserveCommit (here: the ref's lock file
// is held) must not lose the stopped outcome: the work in progress is
// still committed on the agent branch, the failure outcome is still
// reported, and the evidence simply names no preserved ref.
func TestStoppedAttemptReportedWhenPreserveCommitFails(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	in := f.inputs()
	in.task.Title = "stop with a held wip ref"
	writeFile(t, f.worktree, "feature.txt", "value=5\n")

	const attempt = 1
	ref := wipRefName(shortID(in.taskID), attempt)
	gitDir := gitOutput(t, f.worktree, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(f.worktree, gitDir)
	}
	lock := filepath.Join(gitDir, filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Sanity: the held lock makes update-ref fail.
	if _, err := preserveCommit(context.Background(), f.worktree, ref, f.baseSHA); err == nil {
		t.Fatal("preserveCommit succeeded with the ref lock held; the fixture does not exercise the failure")
	}

	sink := &recordingSink{}
	finalizeStopped(context.Background(), in, stopInfo{reason: stopRepeatedFailure, detail: "the same failure again"},
		stopContext{workID: uuid.New(), attempt: attempt}, sink)

	if len(sink.published) != 1 {
		t.Fatalf("published %d messages, want the stopped outcome", len(sink.published))
	}
	msg := sink.published[0]
	if msg.Success || msg.Verified || !strings.HasPrefix(msg.Reason, "repeated_failure: ") {
		t.Fatalf("message = success %v verified %v reason %q", msg.Success, msg.Verified, msg.Reason)
	}
	if strings.Contains(msg.Reason, "preserved at") {
		t.Fatalf("reason claims a preserved ref that was never written: %q", msg.Reason)
	}
	if len(sink.blocked) != 1 || len(sink.statuses) != 1 || sink.statuses[0] != models.AssignmentStatusFailed {
		t.Fatalf("blocked %q statuses %v, want one block and a failed assignment", sink.blocked, sink.statuses)
	}
	ev := msg.Evidence
	if ev == nil || ev.Outcome != evidence.OutcomeFailed {
		t.Fatalf("evidence = %+v, want failed", ev)
	}
	for _, r := range ev.ArtifactRefs {
		if strings.HasPrefix(r, "git_ref:") {
			t.Fatalf("evidence names a ref that was not written: %v", ev.ArtifactRefs)
		}
	}
	head := gitOutput(t, f.worktree, "rev-parse", "HEAD")
	if head == f.baseSHA || ev.HeadSHA != head {
		t.Fatalf("head %s (base %s, evidence %s): the work in progress was not committed", head, f.baseSHA, ev.HeadSHA)
	}
	if got := strings.TrimSpace(gitOutput(t, f.worktree, "show", "HEAD:feature.txt")); got != "value=5" {
		t.Fatalf("committed feature.txt = %q", got)
	}
	if subj := gitOutput(t, f.worktree, "log", "-1", "--format=%s"); !strings.HasPrefix(subj, "wip: ") {
		t.Fatalf("head commit subject %q, want the WIP commit", subj)
	}
	if pushed := f.remoteSHA(t); pushed != "" {
		t.Fatalf("stopped work was pushed (%s)", pushed)
	}
}

// Finalize pushes exactly one explicit refspec, <verified sha> to the agent
// branch; refs/ae/wip/* (an earlier stopped attempt's preserved work) never
// reaches origin.
func TestFinalizeNeverPushesWIPRefs(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	// An earlier stopped attempt's preserved work.
	wip := f.commit(t, "feature.txt", "value=9\n", "wip: earlier attempt")
	gitRun(t, f.worktree, "update-ref", wipRefName("deadbeef", 1), wip)
	gitRun(t, f.worktree, "reset", "--hard", f.baseSHA)
	head := f.commit(t, "feature.txt", "value=2\n", "feat: passes")

	in := f.inputs()
	args := pushArgs(in, head)
	if want := []string{"push", "--force-with-lease", "origin", head + ":refs/heads/" + f.branch}; strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("push args = %q, want %q", args, want)
	}
	for _, a := range args {
		if strings.Contains(a, "refs/ae") || strings.Contains(a, "*") || a == "--all" || a == "--mirror" || a == "--tags" {
			t.Fatalf("push args %q would send more than the agent branch", args)
		}
	}

	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("finalize failed: %+v", sink.published)
	}
	if got := f.remoteSHA(t); got != head {
		t.Fatalf("origin %s = %s, want %s", f.branch, got, head)
	}
	if refs := strings.TrimSpace(gitOutput(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/ae")); refs != "" {
		t.Fatalf("origin received WIP refs: %s", refs)
	}
	// Nothing but the agent branch (and main, pushed by the fixture).
	refs := strings.Fields(gitOutput(t, f.remote, "for-each-ref", "--format=%(refname)"))
	for _, r := range refs {
		if r != "refs/heads/main" && r != "refs/heads/"+f.branch {
			t.Fatalf("origin received %s (all refs: %v)", r, refs)
		}
	}
}
