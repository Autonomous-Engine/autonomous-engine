package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Snapshot worktrees (M2-03 ruling): one read-only, detached worktree per
// (repository, base_sha) at <git-common-dir>/ae-snapshots/<sha12> — for an
// ordinary clone <clone_path>/.git/ae-snapshots/<sha12> — created with
// `git -C <clone> worktree add --detach <path> <sha>` (after `git fetch origin
// <base_ref>` when the clone does not have the commit). They are shared by
// every work item pinned to the same commit: research reads the pinned tree
// there instead of the shared clone's working tree, which knowledge-indexer
// and github-service move underneath in-flight work.
//
// Prune (service startup): a snapshot is removed only when no non-terminal
// work item's pin references its commit AND it has not been created or
// reused for MaxAge (default 7 days). Reuse refreshes the directory's mtime,
// which is the age Prune reads.

// SnapshotDirName is the directory under a clone's git dir (git rev-parse
// --git-common-dir) that holds its snapshot worktrees. Living inside .git
// keeps them out of the clone's status and out of every directory walker
// (knowledge-indexer, Codex extractors, LSP scans all skip .git).
const SnapshotDirName = "ae-snapshots"

// DefaultSnapshotMaxAge is how long an unreferenced snapshot is kept.
const DefaultSnapshotMaxAge = 7 * 24 * time.Hour

// SnapshotDirIn is the snapshot directory of a repository whose common git
// dir is gitDir (<gitDir>/ae-snapshots).
func SnapshotDirIn(gitDir string) string {
	return filepath.Join(gitDir, SnapshotDirName)
}

// WorktreePathIn is the snapshot worktree at sha of a repository whose
// common git dir is gitDir (<gitDir>/ae-snapshots/<sha12>).
func WorktreePathIn(gitDir, sha string) string {
	return filepath.Join(SnapshotDirIn(gitDir), short(sha))
}

// SnapshotDir resolves clonePath's snapshot directory through
// `git rev-parse --git-common-dir`, so a clone that is itself a linked
// worktree still uses its repository's git dir.
func (w *Worktrees) SnapshotDir(ctx context.Context, clonePath string) (string, error) {
	out, err := w.git(ctx, "-C", clonePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("snapshot worktree: locate the git dir of %s: %w", clonePath, err)
	}
	gitDir := strings.TrimSpace(out)
	if gitDir == "" {
		return "", fmt.Errorf("snapshot worktree: %s has no git dir", clonePath)
	}
	return SnapshotDirIn(filepath.FromSlash(gitDir)), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Short is the 12-character form of a commit id used in paths, prompts and
// PR descriptions.
func Short(sha string) string { return short(sha) }

var snapshotNamePattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// Worktrees creates, reuses and prunes snapshot worktrees. The zero value is
// usable: real git, time.Now, 7-day max age.
type Worktrees struct {
	// Run runs git (GitRunner when nil). Tests inject a fake.
	Run Runner
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// MaxAge is how long an unreferenced snapshot survives Prune
	// (DefaultSnapshotMaxAge when zero).
	MaxAge time.Duration
	// Timeout bounds each git command (DefaultTimeout when zero). A fetch
	// or checkout of a large repository can take a while; callers may raise
	// it.
	Timeout time.Duration
	// LockWait bounds how long Ensure waits for another process creating
	// the same snapshot (2 minutes when zero).
	LockWait time.Duration
}

// Snapshot is a snapshot worktree Ensure returned.
type Snapshot struct {
	Path string
	SHA  string
	// Created is true when this call created the worktree, false when an
	// existing one was reused.
	Created bool
	// Fetched is true when the commit was absent from the clone and was
	// fetched from origin first.
	Fetched bool
}

// in-process serialisation per snapshot path; the lock file below covers
// sibling replicas sharing the repo-cache volume.
var snapshotLocks sync.Map // path -> *sync.Mutex

func (w *Worktrees) git(ctx context.Context, args ...string) (string, error) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := w.Run
	if run == nil {
		run = GitRunner
	}
	return run(cctx, args...)
}

func (w *Worktrees) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Ensure returns the snapshot worktree of clonePath at sha, creating it when
// absent. baseRef is the branch the pin was resolved from; it is fetched from
// origin only when the clone lacks the commit. The returned worktree's HEAD is
// verified to be sha; anything else is an error (never a silent fallback to
// the clone's working tree).
func (w *Worktrees) Ensure(ctx context.Context, clonePath, baseRef, sha string) (Snapshot, error) {
	if !ValidSHA(sha) {
		return Snapshot{}, fmt.Errorf("snapshot worktree: %q is not a full commit id", sha)
	}
	if strings.TrimSpace(clonePath) == "" {
		return Snapshot{}, errors.New("snapshot worktree: repository has no clone path")
	}
	if baseRef != "" && (strings.HasPrefix(baseRef, "-") || strings.ContainsAny(baseRef, " \t\n")) {
		return Snapshot{}, fmt.Errorf("snapshot worktree: invalid base ref %q", baseRef)
	}
	dir, err := w.SnapshotDir(ctx, clonePath)
	if err != nil {
		return Snapshot{SHA: sha}, err
	}
	path := filepath.Join(dir, short(sha))
	snap := Snapshot{Path: path, SHA: sha}

	mu, _ := snapshotLocks.LoadOrStore(path, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return snap, fmt.Errorf("snapshot worktree: create %s: %w", dir, err)
	}
	unlock, err := w.lockFile(ctx, path+".lock")
	if err != nil {
		return snap, err
	}
	defer unlock()

	reuse, replace, err := w.existing(ctx, path, sha)
	if err != nil {
		return snap, err
	}
	if reuse {
		w.touch(path)
		return snap, nil
	}
	if replace {
		// A leftover that is provably not a worktree at sha (no .git, or a
		// HEAD that reads as a different commit — an interrupted creation):
		// remove it before creating the real one.
		w.git(ctx, "-C", clonePath, "worktree", "remove", "--force", path) //nolint:errcheck
		if _, statErr := os.Stat(path); statErr == nil {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				return snap, fmt.Errorf("snapshot worktree: remove broken %s: %w", path, rmErr)
			}
		}
		w.git(ctx, "-C", clonePath, "worktree", "prune") //nolint:errcheck
	}

	if !w.hasCommit(ctx, clonePath, sha) {
		if baseRef == "" {
			return snap, fmt.Errorf("snapshot worktree: commit %s is not in %s and no base ref is known to fetch", short(sha), clonePath)
		}
		if _, ferr := w.git(ctx, "-C", clonePath, "fetch", "--quiet", "origin", baseRef); ferr != nil {
			return snap, fmt.Errorf("snapshot worktree: commit %s is not in %s and fetching origin %s failed: %w", short(sha), clonePath, baseRef, ferr)
		}
		snap.Fetched = true
		if !w.hasCommit(ctx, clonePath, sha) {
			return snap, fmt.Errorf("snapshot worktree: commit %s is still not in %s after fetching origin %s (the pinned commit is no longer reachable from the branch)", short(sha), clonePath, baseRef)
		}
	}
	if _, err := w.git(ctx, "-C", clonePath, "worktree", "add", "--detach", path, sha); err != nil {
		return snap, fmt.Errorf("snapshot worktree: git worktree add --detach %s %s: %w", path, short(sha), err)
	}
	if !w.valid(ctx, path, sha) {
		return snap, fmt.Errorf("snapshot worktree: %s was created but its HEAD is not %s", path, short(sha))
	}
	snap.Created = true
	return snap, nil
}

// valid reports whether path is a worktree whose HEAD is sha.
// existing classifies what is at path: reuse (a worktree whose HEAD is
// sha), replace (a directory with no .git, or a HEAD that reads as a
// DIFFERENT commit), or nothing (absent). A HEAD that cannot be read is an
// error, never a reason to delete: a transient failure must not remove a
// snapshot another task is reading.
func (w *Worktrees) existing(ctx context.Context, path, sha string) (reuse, replace bool, err error) {
	if _, serr := os.Stat(path); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("snapshot worktree: stat %s: %w", path, serr)
	}
	if _, serr := os.Stat(filepath.Join(path, ".git")); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return false, true, nil
		}
		return false, false, fmt.Errorf("snapshot worktree: stat %s/.git: %w", path, serr)
	}
	head, herr := w.git(ctx, "-C", path, "rev-parse", "HEAD")
	if herr != nil {
		return false, false, fmt.Errorf("snapshot worktree: read HEAD of existing %s: %w", path, herr)
	}
	head = strings.TrimSpace(head)
	if head == sha {
		return true, false, nil
	}
	if ValidSHA(head) {
		return false, true, nil
	}
	return false, false, fmt.Errorf("snapshot worktree: existing %s has an unreadable HEAD %q", path, head)
}

func (w *Worktrees) valid(ctx context.Context, path, sha string) bool {
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return false
	}
	head, err := w.git(ctx, "-C", path, "rev-parse", "HEAD")
	return err == nil && strings.TrimSpace(head) == sha
}

func (w *Worktrees) hasCommit(ctx context.Context, clonePath, sha string) bool {
	_, err := w.git(ctx, "-C", clonePath, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// touch refreshes the snapshot's age (Prune reads the directory mtime).
func (w *Worktrees) touch(path string) {
	t := w.now()
	_ = os.Chtimes(path, t, t)
}

// lockFile takes an exclusive lock file shared by every process using the
// repo-cache volume. A lock older than staleLock is taken over (its holder
// died mid-creation).
func (w *Worktrees) lockFile(ctx context.Context, path string) (func(), error) {
	wait := w.LockWait
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	return lockFileWait(ctx, path, wait)
}

func lockFileWait(ctx context.Context, path string, wait time.Duration) (func(), error) {
	const staleLock = 10 * time.Minute
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("snapshot worktree: lock %s: %w", path, err)
		}
		if info, serr := os.Stat(path); serr == nil && time.Since(info.ModTime()) > staleLock {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("snapshot worktree: %s is held by another process for more than %s", path, wait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// PruneResult is what Prune did with one clone's snapshots.
type PruneResult struct {
	Removed []string
	// Kept maps a kept snapshot path to why ("referenced", "recent").
	Kept   map[string]string
	Failed map[string]string
}

// Prune removes clonePath's snapshot worktrees whose commit is not in
// referenced AND whose age (directory mtime) exceeds MaxAge. A snapshot whose
// HEAD cannot be read is judged by age alone. Nothing outside the
// snapshot directory (<git-dir>/ae-snapshots) is ever touched.
func (w *Worktrees) Prune(ctx context.Context, clonePath string, referenced map[string]bool) (PruneResult, error) {
	res := PruneResult{Kept: map[string]string{}, Failed: map[string]string{}}
	if _, err := os.Stat(clonePath); errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	dir, err := w.SnapshotDir(ctx, clonePath)
	if err != nil {
		return res, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("snapshot prune: read %s: %w", dir, err)
	}
	maxAge := w.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultSnapshotMaxAge
	}
	refShort := make(map[string]bool, len(referenced))
	for sha, ok := range referenced {
		if ok {
			refShort[short(sha)] = true
		}
	}
	now := w.now()
	removedAny := false
	for _, e := range entries {
		if !e.IsDir() || !snapshotNamePattern.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		head := ""
		if out, herr := w.git(ctx, "-C", path, "rev-parse", "HEAD"); herr == nil {
			head = strings.TrimSpace(out)
		}
		if referenced[head] || refShort[e.Name()] {
			res.Kept[path] = "referenced"
			continue
		}
		if info, ierr := e.Info(); ierr == nil && now.Sub(info.ModTime()) < maxAge {
			res.Kept[path] = "recent"
			continue
		}
		removed, why := w.pruneOne(ctx, clonePath, path, now, maxAge)
		switch {
		case removed:
			removedAny = true
			res.Removed = append(res.Removed, path)
		case why == "recent" || why == "busy":
			res.Kept[path] = why
		default:
			res.Failed[path] = why
		}
	}
	if removedAny {
		w.git(ctx, "-C", clonePath, "worktree", "prune") //nolint:errcheck
	}
	sort.Strings(res.Removed)
	return res, nil
}

// pruneOne removes one stale snapshot under its <sha12>.lock (the lock Ensure
// holds while creating or reusing it), re-reading the mtime under the lock so
// a reuse that raced the first check keeps the snapshot. A lock held by
// someone else means the snapshot is in use: it is kept ("busy").
func (w *Worktrees) pruneOne(ctx context.Context, clonePath, path string, now time.Time, maxAge time.Duration) (bool, string) {
	unlock, err := lockFileWait(ctx, path+".lock", time.Second)
	if err != nil {
		return false, "busy"
	}
	defer unlock()
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "recent" // gone already; nothing to do
		}
		return false, err.Error()
	}
	if now.Sub(info.ModTime()) < maxAge {
		return false, "recent"
	}
	if _, rerr := w.git(ctx, "-C", clonePath, "worktree", "remove", "--force", path); rerr != nil {
		// Not a registered worktree (or already unregistered): remove the
		// directory, which is ours by construction.
		if _, serr := os.Stat(path); serr == nil {
			if err := os.RemoveAll(path); err != nil {
				return false, fmt.Sprintf("%v; remove: %v", rerr, err)
			}
		}
	}
	return true, ""
}

// TerminalStates are the work item states whose pins no longer hold a
// snapshot alive: finished subtasks (passed, failed) and finished or
// abandoned items (released, cancelled, rejected). blocked and awaiting_user
// items keep theirs; a retried failed subtask re-creates its snapshot on
// demand.
var TerminalStates = []string{"passed", "failed", "released", "cancelled", "rejected"}

// referencedSQL: a pin keeps its snapshot alive only while its work item is
// non-terminal AND it has no parent or its parent is non-terminal. M2-02
// copies the parent's pin to every subtask, so without the parent condition
// a subtask of a released parent would hold the snapshot forever.
const referencedSQL = `
SELECT DISTINCT p.base_sha
  FROM work_item_base_shas p
  JOIN work_items w ON w.id = p.work_item_id
  LEFT JOIN work_items parent ON parent.id = w.parent_id
 WHERE p.repository_id = $1
   AND w.state::text <> ALL($2::text[])
   AND (parent.id IS NULL OR parent.state::text <> ALL($2::text[]))`

// ReferencedSHAs returns the commits pinned for repositoryID by work items
// that are non-terminal and whose parent (if any) is non-terminal.
func ReferencedSHAs(ctx context.Context, db Querier, repositoryID uuid.UUID) (map[string]bool, error) {
	rows, err := db.Query(ctx, referencedSQL, repositoryID, TerminalStates)
	if err != nil {
		return nil, fmt.Errorf("snapshot: referenced pins of repository %s: %w", repositoryID, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}

// PruneRepositories prunes the snapshot worktrees of every repository with a
// clone on this host. A repository whose referenced pins cannot be read is
// skipped (never pruned blind). Each removal is logged with service as the
// prefix; the return is every removed path.
func PruneRepositories(ctx context.Context, db Querier, w *Worktrees, service string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT id, clone_path FROM repositories WHERE COALESCE(clone_path, '') <> ''`)
	if err != nil {
		return nil, fmt.Errorf("snapshot prune: list repositories: %w", err)
	}
	type repo struct {
		id   uuid.UUID
		path string
	}
	var repos []repo
	for rows.Next() {
		var r repo
		if err := rows.Scan(&r.id, &r.path); err != nil {
			rows.Close()
			return nil, fmt.Errorf("snapshot prune: scan repository: %w", err)
		}
		repos = append(repos, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var removed []string
	for _, r := range repos {
		if _, err := os.Stat(r.path); err != nil {
			continue
		}
		dir, err := w.SnapshotDir(ctx, r.path)
		if err != nil {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		ref, err := ReferencedSHAs(ctx, db, r.id)
		if err != nil {
			log.Printf("%s: snapshot prune: repository %s: %v — skipped", service, r.id, err)
			continue
		}
		res, err := w.Prune(ctx, r.path, ref)
		if err != nil {
			log.Printf("%s: snapshot prune: repository %s: %v", service, r.id, err)
			continue
		}
		for _, p := range res.Removed {
			log.Printf("%s: snapshot prune: removed %s (unreferenced by any non-terminal pin, older than %s)", service, p, maxAgeOf(w))
		}
		for p, why := range res.Failed {
			log.Printf("%s: snapshot prune: could not remove %s: %s", service, p, why)
		}
		removed = append(removed, res.Removed...)
	}
	log.Printf("%s: snapshot prune: %d snapshot worktree(s) removed", service, len(removed))
	return removed, nil
}

func maxAgeOf(w *Worktrees) time.Duration {
	if w.MaxAge > 0 {
		return w.MaxAge
	}
	return DefaultSnapshotMaxAge
}
