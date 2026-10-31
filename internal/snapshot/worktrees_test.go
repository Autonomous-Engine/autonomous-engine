package snapshot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeWorktreeGit simulates the git commands Worktrees runs. `worktree add`
// creates the directory with a .git file so valid() sees a worktree; HEAD
// answers come from heads (path -> sha).
type fakeWorktreeGit struct {
	calls [][]string
	// present reports whether the clone has the commit (cat-file -e);
	// afterFetch is what it reports once a fetch ran.
	present, afterFetch bool
	fetched             bool
	fetchErr            error
	addErr              error
	// heads answers `-C <path> rev-parse HEAD`.
	heads map[string]string
	// addHead overrides the HEAD a freshly added worktree reports.
	addHead string
}

func (f *fakeWorktreeGit) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if len(args) < 3 || args[0] != "-C" {
		return "", errors.New("unexpected git call")
	}
	dir, sub := args[1], args[2]
	switch sub {
	case "rev-parse":
		if len(args) >= 5 && args[3] == "--path-format=absolute" && args[4] == "--git-common-dir" {
			return filepath.Join(dir, ".git") + "\n", nil
		}
		if h, ok := f.heads[dir]; ok {
			return h + "\n", nil
		}
		return "", errors.New("not a git repository")
	case "cat-file":
		if f.present || (f.fetched && f.afterFetch) {
			return "", nil
		}
		return "", errors.New("missing")
	case "fetch":
		if f.fetchErr != nil {
			return "", f.fetchErr
		}
		f.fetched = true
		return "", nil
	case "worktree":
		switch args[3] {
		case "add":
			if f.addErr != nil {
				return "", f.addErr
			}
			path, sha := args[5], args[6]
			if err := os.MkdirAll(path, 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
				return "", err
			}
			if f.addHead != "" {
				sha = f.addHead
			}
			f.heads[path] = sha
			return "", nil
		case "remove":
			path := args[len(args)-1]
			delete(f.heads, path)
			return "", os.RemoveAll(path)
		case "prune":
			return "", nil
		}
	}
	return "", errors.New("unexpected git call " + strings.Join(args, " "))
}

func (f *fakeWorktreeGit) did(sub ...string) bool {
	for _, c := range f.calls {
		if len(c) >= 2+len(sub) && strings.Join(c[2:2+len(sub)], " ") == strings.Join(sub, " ") {
			return true
		}
	}
	return false
}

func newFakeWT(present bool) *fakeWorktreeGit {
	return &fakeWorktreeGit{present: present, heads: map[string]string{}}
}

func TestWorktreePath_IsUnderTheGitDirBySHA12(t *testing.T) {
	got := WorktreePathIn("/repo-cache/p/app/.git", shaA)
	want := filepath.Join("/repo-cache/p/app/.git", "ae-snapshots", shaA[:12])
	if got != want {
		t.Fatalf("WorktreePathIn = %s, want %s", got, want)
	}
}

// wtPath is where the fake runner's clone keeps snapshots.
func wtPath(clone, sha string) string { return WorktreePathIn(filepath.Join(clone, ".git"), sha) }

func TestEnsure_CreatesDetachedWorktreeAtTheSHA(t *testing.T) {
	clone := t.TempDir()
	f := newFakeWT(true)
	w := &Worktrees{Run: f.run}
	snap, err := w.Ensure(context.Background(), clone, "main", shaA)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !snap.Created || snap.Fetched || snap.Path != wtPath(clone, shaA) {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !f.did("worktree", "add", "--detach", snap.Path, shaA) {
		t.Fatalf("expected `worktree add --detach <path> <sha>`, calls: %v", f.calls)
	}
	if f.did("fetch") {
		t.Fatal("a commit already in the clone must not be fetched")
	}
	if _, err := os.Stat(snap.Path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock file left behind: %v", err)
	}
}

func TestEnsure_FetchesTheBaseRefWhenTheSHAIsAbsent(t *testing.T) {
	clone := t.TempDir()
	f := newFakeWT(false)
	f.afterFetch = true
	snap, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), clone, "develop", shaA)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !snap.Fetched || !snap.Created {
		t.Fatalf("snapshot = %+v", snap)
	}
	fetchAt, addAt := -1, -1
	for i, c := range f.calls {
		if len(c) >= 6 && c[2] == "fetch" && c[4] == "origin" && c[5] == "develop" {
			fetchAt = i
		}
		if len(c) >= 4 && c[2] == "worktree" && c[3] == "add" {
			addAt = i
		}
	}
	if fetchAt < 0 || addAt < fetchAt {
		t.Fatalf("expected fetch origin develop before worktree add; calls: %v", f.calls)
	}
}

func TestEnsure_UnreachableSHAFailsWithoutCreating(t *testing.T) {
	clone := t.TempDir()
	f := newFakeWT(false) // still absent after the fetch
	_, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), clone, "main", shaA)
	if err == nil || !strings.Contains(err.Error(), "still not in") {
		t.Fatalf("err = %v", err)
	}
	if f.did("worktree", "add") {
		t.Fatal("no worktree may be created for an unreachable commit")
	}
}

func TestEnsure_FetchFailureIsAnError(t *testing.T) {
	f := newFakeWT(false)
	f.fetchErr = errors.New("Authentication failed")
	_, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), t.TempDir(), "main", shaA)
	if err == nil || !strings.Contains(err.Error(), "fetching origin main failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsure_ReusesAValidWorktreeAndRefreshesItsAge(t *testing.T) {
	clone := t.TempDir()
	f := newFakeWT(true)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	w := &Worktrees{Run: f.run, Now: func() time.Time { return now }}
	first, err := w.Ensure(context.Background(), clone, "main", shaA)
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(first.Path, old, old); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	second, err := w.Ensure(context.Background(), clone, "main", shaA)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Path != first.Path {
		t.Fatalf("second = %+v, want reuse of %s", second, first.Path)
	}
	if f.did("worktree", "add") || f.did("fetch") {
		t.Fatalf("reuse must not add or fetch: %v", f.calls)
	}
	info, _ := os.Stat(first.Path)
	if !info.ModTime().Equal(now) {
		t.Fatalf("mtime = %v, want refreshed to %v", info.ModTime(), now)
	}
}

func TestEnsure_HeadMismatchAfterCreationIsAnError(t *testing.T) {
	f := newFakeWT(true)
	f.addHead = shaB
	_, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), t.TempDir(), "main", shaA)
	if err == nil || !strings.Contains(err.Error(), "HEAD is not") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsure_ReplacesABrokenLeftover(t *testing.T) {
	clone := t.TempDir()
	path := wtPath(clone, shaA)
	if err := os.MkdirAll(path, 0o755); err != nil { // no .git: interrupted creation
		t.Fatal(err)
	}
	f := newFakeWT(true)
	snap, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), clone, "main", shaA)
	if err != nil || !snap.Created {
		t.Fatalf("Ensure = %+v, %v", snap, err)
	}
}

// An existing snapshot whose HEAD cannot be read (a transient git failure)
// is an error, never a reason to remove it: another task may be reading it.
func TestEnsure_UnreadableHeadIsAnErrorNotARemoval(t *testing.T) {
	clone := t.TempDir()
	path := wtPath(clone, shaA)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeWT(true) // no heads entry: rev-parse HEAD fails
	_, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), clone, "main", shaA)
	if err == nil || !strings.Contains(err.Error(), "read HEAD of existing") {
		t.Fatalf("err = %v", err)
	}
	if f.did("worktree", "remove") || f.did("worktree", "add") {
		t.Fatalf("an unreadable snapshot must be left alone: %v", f.calls)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot was removed: %v", err)
	}
}

// An existing directory whose HEAD reads as a DIFFERENT commit is replaced.
func TestEnsure_DifferentHeadIsReplaced(t *testing.T) {
	clone := t.TempDir()
	path := wtPath(clone, shaA)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeWT(true)
	f.heads[path] = shaB
	snap, err := (&Worktrees{Run: f.run}).Ensure(context.Background(), clone, "main", shaA)
	if err != nil || !snap.Created || !f.did("worktree", "remove") {
		t.Fatalf("snap=%+v err=%v calls=%v", snap, err, f.calls)
	}
}

// Prune takes the snapshot's lock: a snapshot whose lock is held (being
// created or reused right now) is kept as busy.
func TestPrune_KeepsASnapshotWhoseLockIsHeld(t *testing.T) {
	clone := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	f := newFakeWT(true)
	path := wtPath(clone, shaB)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	f.heads[path] = shaB
	old := now.Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(path, old, old)
	if err := os.WriteFile(path+".lock", []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&Worktrees{Run: f.run, Now: func() time.Time { return now }}).Prune(context.Background(), clone, nil)
	if err != nil || len(res.Removed) != 0 || res.Kept[path] != "busy" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("busy snapshot removed: %v", err)
	}
}

func TestEnsure_RejectsBadInput(t *testing.T) {
	w := &Worktrees{Run: newFakeWT(true).run}
	for name, c := range map[string][3]string{
		"short sha":  {t.TempDir(), "main", shaA[:12]},
		"no clone":   {"", "main", shaA},
		"option ref": {t.TempDir(), "--upload-pack=x", shaA},
	} {
		if _, err := w.Ensure(context.Background(), c[0], c[1], c[2]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPrune_RemovesOnlyUnreferencedAndOld(t *testing.T) {
	clone := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	f := newFakeWT(true)
	w := &Worktrees{Run: f.run, Now: func() time.Time { return now }}
	mk := func(sha string, age time.Duration) string {
		p := wtPath(clone, sha)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		f.heads[p] = sha
		ts := now.Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}
	shaC := strings.Repeat("c", 40)
	shaD := strings.Repeat("d", 40)
	referencedOld := mk(shaA, 30*24*time.Hour)
	unreferencedOld := mk(shaB, 8*24*time.Hour)
	unreferencedRecent := mk(shaC, 6*24*time.Hour)
	// A directory that is not a snapshot name is never touched.
	other := filepath.Join(SnapshotDirIn(filepath.Join(clone, ".git")), "keep-me")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-365 * 24 * time.Hour)
	_ = os.Chtimes(other, old, old)

	res, err := w.Prune(context.Background(), clone, map[string]bool{shaA: true, shaD: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != unreferencedOld {
		t.Fatalf("removed = %v, want only %s", res.Removed, unreferencedOld)
	}
	if res.Kept[referencedOld] != "referenced" || res.Kept[unreferencedRecent] != "recent" {
		t.Fatalf("kept = %v", res.Kept)
	}
	for _, p := range []string{referencedOld, unreferencedRecent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive: %v", p, err)
		}
	}
	if _, err := os.Stat(unreferencedOld); !os.IsNotExist(err) {
		t.Errorf("%s should be gone: %v", unreferencedOld, err)
	}
	if !f.did("worktree", "prune") {
		t.Error("expected `git worktree prune` after a removal")
	}
}

func TestPrune_NoSnapshotDirIsANoOp(t *testing.T) {
	f := newFakeWT(true)
	res, err := (&Worktrees{Run: f.run}).Prune(context.Background(), t.TempDir(), nil)
	// Only the git-dir lookup runs; nothing is removed or pruned.
	if err != nil || len(res.Removed) != 0 || len(f.calls) != 1 || f.did("worktree") {
		t.Fatalf("res=%+v err=%v calls=%v", res, err, f.calls)
	}
}

// realGitFixture builds remote.git, a clone of it, and moves the remote one
// commit ahead so the clone lacks the new tip.
func realGitFixture(t *testing.T) (clone, remote, oldTip, newTip string, git func(wd string, args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git = func(wd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	work := filepath.Join(dir, "work")
	remote = filepath.Join(dir, "remote.git")
	clone = filepath.Join(dir, "clone")
	git(dir, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "one")
	git(dir, "clone", "-q", "--bare", work, remote)
	git(dir, "clone", "-q", remote, clone)
	oldTip = git(clone, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "commit", "-q", "-am", "two")
	git(work, "push", "-q", remote, "main")
	newTip = git(remote, "rev-parse", "main")
	return clone, remote, oldTip, newTip, git
}

// A clone that is itself a linked worktree keeps its snapshots under the
// repository's common git dir (git rev-parse --git-common-dir).
func TestEnsure_RealGitCloneThatIsAWorktreeUsesTheCommonGitDir(t *testing.T) {
	clone, _, oldTip, _, git := realGitFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	git(clone, "worktree", "add", "-q", "--detach", linked, oldTip)
	snap, err := (&Worktrees{}).Ensure(context.Background(), linked, "main", oldTip)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := filepath.ToSlash(filepath.Join(clone, ".git", "ae-snapshots", oldTip[:12]))
	if filepath.ToSlash(snap.Path) != want {
		t.Fatalf("snapshot path = %s, want %s", snap.Path, want)
	}
	if head := git(snap.Path, "rev-parse", "HEAD"); head != oldTip {
		t.Fatalf("HEAD = %s", head)
	}
}

func TestEnsure_RealGitFetchesAndPinsAndLeavesTheCloneAlone(t *testing.T) {
	clone, _, oldTip, newTip, git := realGitFixture(t)
	w := &Worktrees{}
	snap, err := w.Ensure(context.Background(), clone, "main", newTip)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !snap.Created || !snap.Fetched {
		t.Fatalf("snapshot = %+v", snap)
	}
	if head := git(snap.Path, "rev-parse", "HEAD"); head != newTip {
		t.Fatalf("snapshot HEAD = %s, want pin %s", head, newTip)
	}
	data, err := os.ReadFile(filepath.Join(snap.Path, "a.txt"))
	if err != nil || strings.TrimSpace(string(data)) != "2" {
		t.Fatalf("snapshot content = %q, %v", data, err)
	}
	// The clone's own checkout is untouched and its status stays clean.
	if head := git(clone, "rev-parse", "HEAD"); head != oldTip {
		t.Fatalf("clone HEAD moved to %s", head)
	}
	if st := git(clone, "status", "--porcelain", "--ignored"); st != "" {
		t.Fatalf("clone status not clean (even counting ignored files): %q", st)
	}
	if want := filepath.ToSlash(filepath.Join(clone, ".git", "ae-snapshots", newTip[:12])); filepath.ToSlash(snap.Path) != want {
		t.Fatalf("snapshot path = %s, want %s", snap.Path, want)
	}
	if list := git(clone, "worktree", "list", "--porcelain"); !strings.Contains(list, "ae-snapshots/"+newTip[:12]) {
		t.Fatalf("snapshot not registered as a worktree:\n%s", list)
	}
	if b := git(snap.Path, "rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
		t.Fatalf("snapshot is on branch %q, want detached", b)
	}

	again, err := w.Ensure(context.Background(), clone, "main", newTip)
	if err != nil || again.Created || again.Path != snap.Path {
		t.Fatalf("second Ensure = %+v, %v (want reuse)", again, err)
	}

	// Prune: referenced keeps it even when old; unreferenced and old removes it.
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(snap.Path, old, old); err != nil {
		t.Fatal(err)
	}
	res, err := w.Prune(context.Background(), clone, map[string]bool{newTip: true})
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("referenced prune = %+v, %v", res, err)
	}
	res, err = w.Prune(context.Background(), clone, nil)
	if err != nil || len(res.Removed) != 1 {
		t.Fatalf("unreferenced prune = %+v, %v", res, err)
	}
	if list := git(clone, "worktree", "list", "--porcelain"); strings.Contains(list, SnapshotDirName) {
		t.Fatalf("pruned worktree still registered:\n%s", list)
	}
}
