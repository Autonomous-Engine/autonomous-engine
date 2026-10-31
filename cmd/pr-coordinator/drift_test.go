package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

// driftRepo is a bare remote, a clone of it made at the pin, and helpers to
// push an agent branch and move the remote's main.
type driftRepo struct {
	t      *testing.T
	root   string
	work   string
	remote string
	clone  string
	pin    string
}

func (r *driftRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *driftRepo) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, name), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func newDriftRepo(t *testing.T) *driftRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := &driftRepo{t: t, root: t.TempDir()}
	r.work = filepath.Join(r.root, "work")
	r.remote = filepath.Join(r.root, "remote.git")
	r.clone = filepath.Join(r.root, "clone")
	r.git(r.root, "init", "-q", "-b", "main", r.work)
	r.git(r.work, "config", "core.autocrlf", "false")
	r.write("a.txt", "line 1\nline 2\nline 3\n")
	r.write("b.txt", "b\n")
	r.git(r.work, "add", "-A")
	r.git(r.work, "commit", "-q", "-m", "base")
	r.pin = r.git(r.work, "rev-parse", "HEAD")
	r.git(r.root, "clone", "-q", "--bare", r.work, r.remote)
	r.git(r.root, "clone", "-q", r.remote, r.clone)
	return r
}

// pushAgentBranch commits (file, content) on a branch from the pin and pushes
// it; returns its head.
func (r *driftRepo) pushAgentBranch(branch, file, content string) string {
	r.t.Helper()
	r.git(r.work, "checkout", "-q", "-b", "agent-src", r.pin)
	r.write(file, content)
	r.git(r.work, "commit", "-q", "-am", "agent change")
	head := r.git(r.work, "rev-parse", "HEAD")
	r.git(r.work, "push", "-q", r.remote, "HEAD:refs/heads/"+branch)
	r.git(r.work, "checkout", "-q", "main")
	return head
}

// moveMain commits (file, content) on main and pushes it; returns the new tip.
func (r *driftRepo) moveMain(file, content string) string {
	r.t.Helper()
	r.write(file, content)
	r.git(r.work, "commit", "-q", "-am", "upstream change")
	r.git(r.work, "push", "-q", r.remote, "main")
	return r.git(r.work, "rev-parse", "HEAD")
}

// cloneState is everything a read-only consumer must not change.
func (r *driftRepo) cloneState() string {
	return r.git(r.clone, "for-each-ref") + "|" + r.git(r.clone, "rev-parse", "HEAD") + "|" + r.git(r.clone, "count-objects", "-v")
}

func TestDriftCheck_UnmovedBase(t *testing.T) {
	r := newDriftRepo(t)
	head := r.pushAgentBranch("agent/w/1", "a.txt", "line 1\nagent\nline 3\n")
	d, err := (&driftChecker{}).Check(context.Background(), "app", r.clone, "main", r.pin, head, "agent/w/1")
	if err != nil || d.Moved || d.New != r.pin {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

func TestDriftCheck_MovedCleanlyAndCloneUntouched(t *testing.T) {
	r := newDriftRepo(t)
	head := r.pushAgentBranch("agent/w/1", "a.txt", "line 1\nagent\nline 3\n")
	tip := r.moveMain("b.txt", "b moved\n")
	before := r.cloneState()
	d, err := (&driftChecker{TempDir: t.TempDir()}).Check(context.Background(), "app", r.clone, "main", r.pin, head, "agent/w/1")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Moved || !d.Clean || d.Old != r.pin || d.New != tip || len(d.Conflicts) != 0 {
		t.Fatalf("d = %+v, want moved %s→%s and clean", d, r.pin, tip)
	}
	if after := r.cloneState(); after != before {
		t.Fatalf("the shared clone changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestDriftCheck_MovedWithConflictsNamesThePaths(t *testing.T) {
	r := newDriftRepo(t)
	head := r.pushAgentBranch("agent/w/1", "a.txt", "line 1\nagent\nline 3\n")
	r.moveMain("a.txt", "line 1\nupstream\nline 3\n")
	d, err := (&driftChecker{TempDir: t.TempDir()}).Check(context.Background(), "app", r.clone, "main", r.pin, head, "agent/w/1")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Moved || d.Clean || len(d.Conflicts) != 1 || d.Conflicts[0] != "a.txt" {
		t.Fatalf("d = %+v, want a conflict in a.txt", d)
	}
}

func TestDriftCheck_HeadNotOnTheBranchIsAnError(t *testing.T) {
	r := newDriftRepo(t)
	r.pushAgentBranch("agent/w/1", "a.txt", "x\n")
	r.moveMain("b.txt", "moved\n")
	_, err := (&driftChecker{TempDir: t.TempDir()}).Check(context.Background(), "app", r.clone, "main", r.pin, strings.Repeat("e", 40), "agent/w/1")
	if err == nil || !strings.Contains(err.Error(), "not on origin") {
		t.Fatalf("err = %v", err)
	}
}

func TestDriftCheck_UnreachableRemoteIsAnError(t *testing.T) {
	r := newDriftRepo(t)
	r.git(r.clone, "remote", "set-url", "origin", filepath.Join(r.root, "gone.git"))
	if _, err := (&driftChecker{}).Check(context.Background(), "app", r.clone, "main", r.pin, "", "agent/w/1"); err == nil {
		t.Fatal("an unreadable remote must be reported, not treated as unmoved")
	}
}

func TestApplyBaseDrift_UnpinnedIsNotChecked(t *testing.T) {
	ran := false
	checker := &driftChecker{Run: func(context.Context, ...string) (string, error) { ran = true; return "", nil }}
	if d := applyBaseDrift(context.Background(), checker, nil, nil, driftInput{}); d.Stop || d.Drift != nil || len(d.Events) != 0 || ran {
		t.Fatalf("unpinned: %+v ran=%v", d, ran)
	}
}

// An unevaluable drift check fails closed: the parent is blocked with the
// redacted cause and one escalation prompt in one transaction, no drift
// event is invented, and the caller opens no PR (Stop).
func TestApplyBaseDrift_UnevaluableBlocksWithOnePrompt(t *testing.T) {
	const origin = "https://x-access-token:ghp_secret@github.com/o/app"
	checker := &driftChecker{Run: func(_ context.Context, args ...string) (string, error) {
		switch {
		case len(args) >= 5 && args[2] == "config":
			return origin + "\n", nil
		case len(args) >= 3 && args[2] == "ls-remote":
			return "", errors.New("exit status 128: fatal: Authentication failed for '" + origin + "'")
		}
		return "", errors.New("unexpected git call: " + strings.Join(args, " "))
	}}
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	in := driftInput{WorkID: uuid.New(), RepoID: uuid.New(), ProductID: uuid.New(), WorkTitle: "T", RepoName: "app",
		ClonePath: "/repo-cache/p/app", BaseRef: "main", Pin: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Branch: "agent/w/1"}

	d := applyBaseDrift(context.Background(), checker, db, interactions.NewPublisher(nil), in)
	if !d.Stop || d.Drift != nil || len(d.Events) != 0 {
		t.Fatalf("decision = %+v, want Stop (no PR)", d)
	}
	if db.begins != 1 || tx.commits != 1 {
		t.Fatalf("begins=%d commits=%d, want one committed transaction", db.begins, tx.commits)
	}
	prompts, events, reason := 0, 0, ""
	for i, sql := range tx.sqls {
		switch {
		case strings.Contains(sql, "INSERT INTO user_interactions"):
			prompts++
		case strings.Contains(sql, "base_sha_events"):
			events++
		}
		for _, a := range tx.args[i] {
			if str, ok := a.(string); ok && strings.Contains(str, "base drift could not be evaluated") {
				reason = str
			}
		}
	}
	if prompts != 1 || events != 0 {
		t.Fatalf("prompts=%d drift events=%d, want 1 and 0", prompts, events)
	}
	if !strings.HasPrefix(reason, "base drift could not be evaluated for app: ") || !strings.Contains(reason, "Authentication failed") {
		t.Fatalf("block reason = %q", reason)
	}
	for _, args := range tx.args {
		for _, a := range args {
			if str, ok := a.(string); ok && strings.Contains(str, "ghp_secret") {
				t.Fatalf("credential leaked into the block: %q", str)
			}
		}
	}
}

// The base moved (the tip was read) but the scratch fetch fails: the block
// still carries the drift_detected event (old = pin, new = observed tip) in
// the same transaction. An ls-remote failure (above) has no tip and writes
// no event.
func TestApplyBaseDrift_MovedButUncheckableAuditsTheMove(t *testing.T) {
	const origin = "https://x-access-token:ghp_secret@github.com/o/app"
	pin, tip := strings.Repeat("a", 40), strings.Repeat("c", 40)
	checker := &driftChecker{TempDir: t.TempDir(), Run: func(_ context.Context, args ...string) (string, error) {
		switch {
		case len(args) >= 5 && args[2] == "config":
			return origin + "\n", nil
		case len(args) >= 3 && args[2] == "ls-remote":
			return tip + "\trefs/heads/main\n", nil
		case len(args) >= 3 && args[2] == "rev-parse":
			return "/repo-cache/p/app/.git\n", nil
		case len(args) >= 1 && args[0] == "init":
			return "", nil
		case len(args) >= 3 && args[2] == "fetch":
			return "", errors.New("exit status 128: fatal: unable to access '" + origin + "'")
		}
		return "", errors.New("unexpected git call: " + strings.Join(args, " "))
	}}
	tx := &fakeTx{}
	db := &fakeBeginner{tx: tx}
	in := driftInput{WorkID: uuid.New(), RepoID: uuid.New(), ProductID: uuid.New(), WorkTitle: "T", RepoName: "app",
		ClonePath: "/repo-cache/p/app", BaseRef: "main", Pin: pin, HeadSHA: strings.Repeat("b", 40), Branch: "agent/w/1"}

	d := applyBaseDrift(context.Background(), checker, db, interactions.NewPublisher(nil), in)
	if !d.Stop || len(d.Events) != 0 {
		t.Fatalf("decision = %+v, want Stop", d)
	}
	if db.begins != 1 || tx.commits != 1 {
		t.Fatalf("begins=%d commits=%d, want one transaction", db.begins, tx.commits)
	}
	var eventArgs []any
	prompts := 0
	for i, sql := range tx.sqls {
		if strings.Contains(sql, "INSERT INTO base_sha_events") {
			eventArgs = tx.args[i]
		}
		if strings.Contains(sql, "INSERT INTO user_interactions") {
			prompts++
		}
	}
	if eventArgs == nil || prompts != 1 {
		t.Fatalf("event=%v prompts=%d, want one drift event and one prompt", eventArgs, prompts)
	}
	// RecordEventTx args: id, work_item_id, repository_id, kind, old, new, detail, created_at.
	if eventArgs[3] != "drift_detected" || eventArgs[4] != pin || eventArgs[5] != tip {
		t.Fatalf("event args = %v, want drift_detected old=pin new=tip", eventArgs)
	}
	if detail, _ := eventArgs[6].(string); !strings.Contains(detail, `"evaluated":false`) || strings.Contains(detail, "ghp_secret") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestApplyBaseDrift_CleanDriftIsRecordedAndDescribed(t *testing.T) {
	r := newDriftRepo(t)
	head := r.pushAgentBranch("agent/w/1", "a.txt", "line 1\nagent\nline 3\n")
	tip := r.moveMain("b.txt", "moved\n")
	in := driftInput{WorkID: uuid.New(), RepoID: uuid.New(), RepoName: "app", ClonePath: r.clone, BaseRef: "main", Pin: r.pin, HeadSHA: head, Branch: "agent/w/1"}
	d := applyBaseDrift(context.Background(), &driftChecker{TempDir: t.TempDir()}, nil, nil, in)
	if d.Stop || d.Drift == nil || !d.Drift.Clean || d.Drift.Old != r.pin || d.Drift.New != tip {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Events) != 1 || d.Events[0].OldSHA != r.pin || d.Events[0].NewSHA != tip || d.Events[0].WorkItemID != in.WorkID {
		t.Fatalf("events = %+v", d.Events)
	}
	want := "base moved from " + r.pin[:12] + " to " + tip[:12] + "; merges cleanly"
	if !strings.Contains(d.DescriptionLine, want) {
		t.Fatalf("description %q lacks %q", d.DescriptionLine, want)
	}
}

func TestLegacyMergeTreeConflicts(t *testing.T) {
	out := `changed in both
  base   100644 1111111111111111111111111111111111111111 a.txt
  our    100644 2222222222222222222222222222222222222222 a.txt
  their  100644 3333333333333333333333333333333333333333 a.txt
@@ -1,3 +1,7 @@
 line 1
+<<<<<<< .our
+upstream
+=======
+agent
+>>>>>>> .their
 line 3
changed in both
  base   100644 4444444444444444444444444444444444444444 c.txt
  our    100644 5555555555555555555555555555555555555555 c.txt
  their  100644 6666666666666666666666666666666666666666 c.txt
@@ -1 +1 @@
-x
+y
`
	got := legacyMergeTreeConflicts(out)
	if len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("conflicts = %v, want [a.txt]", got)
	}
}

func TestDriftBlockReasonNamesEverything(t *testing.T) {
	reason := driftBlockReason("app", "main", "agent/w/1", driftResult{Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40), Conflicts: []string{"a.txt", "src/x.go"}})
	for _, want := range []string{"main moved from aaaaaaaaaaaa", "to bbbbbbbbbbbb", "agent/w/1", "app", "a.txt, src/x.go"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
}
