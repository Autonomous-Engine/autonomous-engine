package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// defaultPullTimeout bounds a clone refresh. The pull runs while the
// indexing slot is held (every later trigger is dropped meanwhile), so a
// stalled remote must not hold it indefinitely.
const defaultPullTimeout = 90 * time.Second

// pullTimeout is KNOWLEDGE_INDEXER_PULL_TIMEOUT (a Go duration such as "2m",
// or whole seconds), else defaultPullTimeout.
func pullTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("KNOWLEDGE_INDEXER_PULL_TIMEOUT"))
	if v == "" {
		return defaultPullTimeout
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultPullTimeout
}

// gitPullCommand builds the refresh command; tests replace it. The low-speed
// settings make git itself abort a transfer that stalls (under 1 byte/s
// for 30s) well before the overall timeout.
var gitPullCommand = func(ctx context.Context, path string) *exec.Cmd {
	return exec.CommandContext(ctx, "git",
		"-c", "http.lowSpeedLimit=1", "-c", "http.lowSpeedTime=30",
		"-C", path, "pull", "--ff-only")
}

// gitPull refreshes the clone at path (git pull --ff-only), bounded by
// pullTimeout. On a timeout the command is killed and an error naming the
// timeout returned; callers log it and index the clone as it is.
func gitPull(path string) error {
	timeout := pullTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := gitPullCommand(ctx, path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// A killed git can leave a child (a remote helper) holding the output
	// pipes; do not wait for it beyond this.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("git pull: timed out after %s (KNOWLEDGE_INDEXER_PULL_TIMEOUT)", timeout)
	}
	if err != nil {
		return fmt.Errorf("git pull: %w: %s", err, string(out))
	}
	return nil
}

// incrementalIndexBranch is the branch an incremental index of the clone
// at indexRepoPath labels its codex rows and vectors with: that clone's
// checked-out branch (never the legacy REPO_PATH's).
func incrementalIndexBranch(indexRepoPath string) string {
	return gitCurrentBranch(indexRepoPath)
}
