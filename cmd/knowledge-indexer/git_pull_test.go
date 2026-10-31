package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGitPullHelperProcess is not a test: gitPull tests run it as a
// stand-in for git that blocks like a stalled remote.
func TestGitPullHelperProcess(t *testing.T) {
	if os.Getenv("AE_GIT_PULL_HELPER") != "block" {
		t.Skip("helper process")
	}
	time.Sleep(10 * time.Minute)
	os.Exit(0)
}

// A stalled pull is killed at the timeout and reported, instead of holding
// the indexing slot; the plan still indexes the clone as it is.
func TestGitPullTimesOutOnAStalledRemote(t *testing.T) {
	t.Setenv("KNOWLEDGE_INDEXER_PULL_TIMEOUT", "300ms")
	t.Setenv("AE_GIT_PULL_HELPER", "block")
	orig := gitPullCommand
	t.Cleanup(func() { gitPullCommand = orig })
	gitPullCommand = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitPullHelperProcess$")
	}

	start := time.Now()
	err := gitPull(t.TempDir())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("gitPull returned after %s; the timeout did not bound it", elapsed)
	}

	var indexed []string
	plan := triggerPlan{steps: []indexStep{{path: "/clones/a", refresh: true}}}
	if err := runTriggerPlan(plan, gitPull, func(st indexStep) error { indexed = append(indexed, st.path); return nil }); err != nil {
		t.Fatalf("runTriggerPlan: %v", err)
	}
	if len(indexed) != 1 || indexed[0] != "/clones/a" {
		t.Fatalf("indexed %v after a timed-out refresh, want the clone as it is", indexed)
	}
}

func TestGitPullCommandIsBoundedAndFastForwardOnly(t *testing.T) {
	cmd := gitPullCommand(context.Background(), "/clones/a")
	got := strings.Join(cmd.Args[1:], " ")
	want := "-c http.lowSpeedLimit=1 -c http.lowSpeedTime=30 -C /clones/a pull --ff-only"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestPullTimeout(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"": defaultPullTimeout, "2m": 2 * time.Minute, "45": 45 * time.Second,
		"-5s": defaultPullTimeout, "junk": defaultPullTimeout, "0": defaultPullTimeout,
	} {
		t.Setenv("KNOWLEDGE_INDEXER_PULL_TIMEOUT", v)
		if got := pullTimeout(); got != want {
			t.Errorf("KNOWLEDGE_INDEXER_PULL_TIMEOUT=%q: %s, want %s", v, got, want)
		}
	}
}

// The incremental index labels rows and vectors with the indexed clone's
// branch, not the legacy REPO_PATH's ("main" fallback when it is absent).
func TestIncrementalIndexBranchIsTheIndexedClones(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	clone := filepath.Join(t.TempDir(), "clone")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", clone}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "release-7")
	run("-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base")

	if got := incrementalIndexBranch(clone); got != "release-7" {
		t.Fatalf("branch = %q, want the indexed clone's release-7", got)
	}
	if repoPath == clone {
		t.Fatal("fixture clone must differ from the legacy REPO_PATH")
	}

	// runIncrementalIndex needs the stores to run end to end; pin that it
	// labels with the indexed clone, not the legacy constant.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (s *server) runIncrementalIndex(")
	if i < 0 {
		t.Fatal("runIncrementalIndex not found")
	}
	body = body[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	if strings.Contains(body, "gitCurrentBranch(repoPath)") || strings.Count(body, "incrementalIndexBranch(indexRepoPath)") != 1 {
		t.Fatal("runIncrementalIndex must label rows and vectors with incrementalIndexBranch(indexRepoPath)")
	}
}
