package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

// M2-03: the worktree starts at the pinned base SHA (fetching it when the
// clone lacks it), its HEAD is asserted, and the integration base is the pin.

// pinnedFixture: a bare remote, a clone of it, and the remote moved one commit
// ahead after the clone was made, so the clone's local main is behind the pin.
func pinnedFixture(t *testing.T) (clone, oldTip, pin string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	work := initRepoAt(t, filepath.Join(root, "work"))
	remote := filepath.Join(root, "remote.git")
	gitRun(t, root, "clone", "-q", "--bare", work, remote)
	clone = filepath.Join(root, "clone")
	gitRun(t, root, "clone", "-q", remote, clone)
	gitRun(t, clone, "config", "user.email", "test@example.com")
	gitRun(t, clone, "config", "user.name", "Test")
	oldTip = gitOutput(t, clone, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(work, "upstream.txt"), []byte("moved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "upstream.txt")
	gitRun(t, work, "commit", "-q", "-m", "upstream moved")
	gitRun(t, work, "push", "-q", remote, "main")
	pin = gitOutput(t, remote, "rev-parse", "main")
	return clone, oldTip, pin
}

func TestCreatePinnedWorktree_StartsAtThePinAfterFetching(t *testing.T) {
	clone, oldTip, pin := pinnedFixture(t)
	if commitPresent(context.Background(), clone, pin) {
		t.Fatal("fixture: the clone must not have the pin yet")
	}
	taskID := uuid.New()
	path := worktreePathFor(clone, taskID)
	branch := "agent/work/" + taskID.String()[:8]
	if err := createPinnedWorktree(context.Background(), clone, path, branch, "main", pin); err != nil {
		t.Fatalf("createPinnedWorktree: %v", err)
	}
	if head := gitOutput(t, path, "rev-parse", "HEAD"); head != pin {
		t.Fatalf("worktree HEAD = %s, want the pin %s", head, pin)
	}
	if b := gitOutput(t, path, "rev-parse", "--abbrev-ref", "HEAD"); b != branch {
		t.Fatalf("worktree branch = %s, want %s", b, branch)
	}
	// The shared clone's own checkout and local main are untouched.
	if head := gitOutput(t, clone, "rev-parse", "main"); head != oldTip {
		t.Fatalf("local main moved to %s", head)
	}
	// The integration base is the pin, so finalize records no drift.
	logger := logging.New("test")
	if base := resolveBaseSHA(context.Background(), logger, path, pin, taskID.String()); base != pin {
		t.Fatalf("resolveBaseSHA = %s, want the pin %s", base, pin)
	}
	// Before M2-03 the worktree started from the local base branch: its
	// merge-base would have been the stale tip, which finalize audits as drift.
	if legacy := resolveBaseSHA(context.Background(), logger, path, "main", taskID.String()); legacy != oldTip {
		t.Fatalf("legacy merge-base = %s, want the stale local tip %s", legacy, oldTip)
	}
	in := finalizeInputs{taskID: taskID, worktreePath: path, baseSHA: pin, logger: logger,
		task: implementationTaskMessage{RepositoryID: uuid.NewString(), BaseSHA: pin, BaseRef: "main"}}
	if out := applyPinnedBase(context.Background(), in); len(out.baseEvents) != 0 {
		t.Fatalf("a worktree at the pin must not be drift: %+v", out.baseEvents)
	}
}

func TestCreatePinnedWorktree_FetchFailureIsFineWhenThePinIsPresent(t *testing.T) {
	clone, oldTip, _ := pinnedFixture(t)
	gitRun(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	taskID := uuid.New()
	path := worktreePathFor(clone, taskID)
	if err := createPinnedWorktree(context.Background(), clone, path, "agent/w/"+taskID.String()[:8], "main", oldTip); err != nil {
		t.Fatalf("createPinnedWorktree: %v", err)
	}
	if head := gitOutput(t, path, "rev-parse", "HEAD"); head != oldTip {
		t.Fatalf("HEAD = %s, want %s", head, oldTip)
	}
}

func TestCreatePinnedWorktree_UnreachablePinBlocksWithoutAWorktree(t *testing.T) {
	clone, _, _ := pinnedFixture(t)
	taskID := uuid.New()
	path := worktreePathFor(clone, taskID)
	missing := strings.Repeat("ab", 20)
	err := createPinnedWorktree(context.Background(), clone, path, "agent/w/"+taskID.String()[:8], "main", missing)
	if err == nil || !strings.Contains(err.Error(), "not reachable from origin main") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("no worktree may exist for an unreachable pin: %v", statErr)
	}
	if err := createPinnedWorktree(context.Background(), clone, path, "b", "main", "main"); err == nil {
		t.Fatal("a branch name is not a pin")
	}
}

func TestAssertWorktreeAt_MismatchIsAnError(t *testing.T) {
	clone, oldTip, pin := pinnedFixture(t)
	taskID := uuid.New()
	path := worktreePathFor(clone, taskID)
	if err := createIsolatedWorktree(context.Background(), clone, path, "agent/w/"+taskID.String()[:8], "main"); err != nil {
		t.Fatal(err)
	}
	if err := assertWorktreeAt(context.Background(), path, oldTip); err != nil {
		t.Fatalf("HEAD is %s: %v", oldTip, err)
	}
	err := assertWorktreeAt(context.Background(), path, pin)
	if err == nil || !strings.Contains(err.Error(), "is not the pinned base") {
		t.Fatalf("err = %v", err)
	}
}

type stubPins struct {
	pin   snapshot.Pin
	found bool
	err   error
	calls int
}

func (s *stubPins) Get(context.Context, uuid.UUID, uuid.UUID) (snapshot.Pin, bool, error) {
	s.calls++
	return s.pin, s.found, s.err
}

func TestTaskPin(t *testing.T) {
	ctx := context.Background()
	taskID := uuid.New()
	repo := uuid.NewString()
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)

	// The message's pin wins and needs no lookup; its ref falls back to the
	// repository base branch.
	s := &stubPins{pin: snapshot.Pin{BaseSHA: other, BaseRef: "x"}, found: true}
	if got, ref := taskPin(ctx, implementationTaskMessage{RepositoryID: repo, BaseSHA: sha}, taskID, s, "develop"); got != sha || ref != "develop" || s.calls != 0 {
		t.Fatalf("message pin: %s %s calls=%d", got, ref, s.calls)
	}
	// No message pin: the subtask's own row.
	if got, ref := taskPin(ctx, implementationTaskMessage{RepositoryID: repo}, taskID, s, "develop"); got != other || ref != "x" {
		t.Fatalf("row pin: %s %s", got, ref)
	}
	// Legacy-unpinned and unreadable rows start from the base branch.
	if got, _ := taskPin(ctx, implementationTaskMessage{RepositoryID: repo}, taskID, &stubPins{}, "main"); got != "" {
		t.Fatalf("legacy: %s", got)
	}
	if got, _ := taskPin(ctx, implementationTaskMessage{RepositoryID: repo}, taskID, &stubPins{err: errors.New("db down")}, "main"); got != "" {
		t.Fatalf("lookup error: %s", got)
	}
	if got, _ := taskPin(ctx, implementationTaskMessage{RepositoryID: "not-a-uuid"}, taskID, s, "main"); got != "" {
		t.Fatalf("bad repo id: %s", got)
	}
}
