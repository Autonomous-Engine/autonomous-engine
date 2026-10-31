package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M2-03: the subtask's worktree starts at its pinned base commit, never at
// whatever the shared clone's local base branch happens to point to.
//
//	git -C <clone> fetch origin <base_ref>
//	git -C <clone> worktree add -b <agent-branch> <path> <base_sha>
//	assert git -C <path> rev-parse HEAD == base_sha   (mismatch blocks)
//
// The fetch keeps the clone's object store current; a failed fetch is only
// fatal when the pinned commit is not already in the clone. A legacy-unpinned
// subtask keeps starting from the local base branch (finalize counts it).

// fetchTimeout bounds the base-ref fetch before worktree creation.
const fetchTimeout = 2 * time.Minute

// taskPin is the subtask's pinned base: the message's base_sha (planner
// subtasks), else the subtask's own pin row (subtasks built outside the
// planner: dashboard retries). ref is the branch the pin was resolved from,
// falling back to the repository's base branch. "" for a legacy-unpinned
// subtask or when the row cannot be read (finalize logs/counts either case).
func taskPin(ctx context.Context, task implementationTaskMessage, taskID uuid.UUID, pins pinLookup, baseBranch string) (sha, ref string) {
	if snapshot.ValidSHA(task.BaseSHA) {
		ref = task.BaseRef
		if ref == "" {
			ref = baseBranch
		}
		return task.BaseSHA, ref
	}
	repoID, err := uuid.Parse(task.RepositoryID)
	if pins == nil || err != nil {
		return "", ""
	}
	p, found, err := pins.Get(ctx, taskID, repoID)
	if err != nil {
		log.Printf("implementation-agent: task_id=%s: read pin: %v — starting from %s", taskID, err, baseBranch)
		return "", ""
	}
	if !found {
		return "", ""
	}
	ref = p.BaseRef
	if ref == "" {
		ref = baseBranch
	}
	return p.BaseSHA, ref
}

// createPinnedWorktree fetches baseRef, creates the subtask branch's worktree
// at pin and asserts its HEAD is pin. On a mismatch the worktree is removed
// and an error returned: the caller blocks the subtask.
func createPinnedWorktree(ctx context.Context, clonePath, worktreePath, branch, baseRef, pin string) error {
	if !snapshot.ValidSHA(pin) {
		return fmt.Errorf("pinned base %q is not a full commit id", pin)
	}
	fetchErr := fetchBaseRef(ctx, clonePath, baseRef)
	if !commitPresent(ctx, clonePath, pin) {
		if fetchErr != nil {
			return fmt.Errorf("pinned base %s is not in the clone and fetching origin %s failed: %v", pin, baseRef, fetchErr)
		}
		return fmt.Errorf("pinned base %s is not reachable from origin %s after fetch", pin, baseRef)
	}
	if fetchErr != nil {
		log.Printf("implementation-agent: fetch origin %s in %s failed (%v) — pinned base %s is already in the clone, continuing", baseRef, clonePath, fetchErr, snapshot.Short(pin))
	}
	if err := createIsolatedWorktree(ctx, clonePath, worktreePath, branch, pin); err != nil {
		return err
	}
	if err := assertWorktreeAt(ctx, worktreePath, pin); err != nil {
		if rmErr := removeWorktree(context.Background(), clonePath, worktreePath, repoCacheRootFromEnv()); rmErr != nil {
			log.Printf("implementation-agent: remove mismatched worktree %s: %v", worktreePath, rmErr)
		}
		return err
	}
	return nil
}

// assertWorktreeAt fails unless the worktree's HEAD is exactly pin.
func assertWorktreeAt(ctx context.Context, worktreePath, pin string) error {
	head, err := gitRead(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("cannot read the new worktree's HEAD: %w", err)
	}
	if head != pin {
		return fmt.Errorf("worktree HEAD %s is not the pinned base %s", head, pin)
	}
	return nil
}

func fetchBaseRef(ctx context.Context, clonePath, baseRef string) error {
	if baseRef == "" || strings.HasPrefix(baseRef, "-") {
		return fmt.Errorf("no usable base ref %q", baseRef)
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(fctx, "git", "fetch", "--quiet", "origin", baseRef)
	cmd.Dir = clonePath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func commitPresent(ctx context.Context, clonePath, sha string) bool {
	_, err := gitRead(ctx, clonePath, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// pruneSnapshotsOnStartup removes snapshot worktrees (<clone>/.git/ae-snapshots)
// that no non-terminal pin references and nobody used for 7 days. Both
// research-agent and implementation-agent own the clones' snapshot dirs, so
// both prune at startup; runs once in the background, bounded.
func pruneSnapshotsOnStartup(ctx context.Context, pool *pgxpool.Pool) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if _, err := snapshot.PruneRepositories(pctx, pool, &snapshot.Worktrees{}, agentID); err != nil {
		log.Printf("implementation-agent: snapshot prune: %v", err)
	}
}
