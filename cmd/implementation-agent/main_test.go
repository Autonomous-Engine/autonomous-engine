package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateIsolatedWorktree(t *testing.T) {
	repo := initTestRepository(t)
	worktree := filepath.Join(t.TempDir(), "task-worktree")

	if err := createIsolatedWorktree(context.Background(), repo, worktree, "agent/work/task", "main"); err != nil {
		t.Fatalf("createIsolatedWorktree() error = %v", err)
	}

	branch := gitOutput(t, worktree, "branch", "--show-current")
	if branch != "agent/work/task" {
		t.Fatalf("worktree branch = %q, want %q", branch, "agent/work/task")
	}
	if mainBranch := gitOutput(t, repo, "branch", "--show-current"); mainBranch != "main" {
		t.Fatalf("shared clone branch = %q, want main", mainBranch)
	}
}

func TestCreateIsolatedWorktreeFailureDoesNotChangeSharedClone(t *testing.T) {
	repo := initTestRepository(t)
	// A file at the requested worktree path makes both creation attempts fail.
	worktree := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(worktree, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := createIsolatedWorktree(context.Background(), repo, worktree, "agent/work/task", "main")
	if err == nil {
		t.Fatal("createIsolatedWorktree() error = nil, want failure")
	}
	if !strings.Contains(err.Error(), "retry:") {
		t.Fatalf("error %q does not include retry diagnostics", err)
	}
	if branch := gitOutput(t, repo, "branch", "--show-current"); branch != "main" {
		t.Fatalf("shared clone branch = %q after failure, want main", branch)
	}
}

func initTestRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	gitRun(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README.md")
	gitRun(t, repo, "commit", "-m", "initial")
	return repo
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
