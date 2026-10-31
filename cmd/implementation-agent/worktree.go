package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/logging"
	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/google/uuid"
)

const (
	// worktreeMarkerFile identifies a directory as an agent-owned linked
	// worktree. Only directories carrying it are ever reconciled.
	worktreeMarkerFile = ".ae-worktree.json"

	// envRepoCacheRoot overrides the shared repo-cache mount that clones
	// (/repo-cache/<product>/<repo>/) and their worktrees live under.
	envRepoCacheRoot     = "AE_REPO_CACHE_ROOT"
	defaultRepoCacheRoot = "/repo-cache"

	// envWorktreeReconcile disables startup reconciliation when set to
	// off, 0, or false.
	envWorktreeReconcile = "AE_WORKTREE_RECONCILE"

	// envWorktreeStaleAfter is a Go duration after which a marker written by
	// another host (a sibling replica sharing the repo-cache volume) is
	// considered abandoned and may be reclaimed.
	envWorktreeStaleAfter     = "AE_WORKTREE_STALE_AFTER"
	defaultWorktreeStaleAfter = 24 * time.Hour

	// envWorktreeReconcileTimeout bounds the startup scan so a large backlog
	// cannot keep the container unhealthy indefinitely; leftovers are picked
	// up on the next start.
	envWorktreeReconcileTimeout     = "AE_WORKTREE_RECONCILE_TIMEOUT"
	defaultWorktreeReconcileTimeout = 5 * time.Minute
)

// The metrics registry has no label support, so the {result} label is
// flattened into three counters.
var (
	worktreesReconciledRemoved = metrics.NewCounter("ae_worktrees_reconciled_removed_total", "Abandoned agent worktrees removed at startup")
	worktreesReconciledSkipped = metrics.NewCounter("ae_worktrees_reconciled_skipped_total", "Marker worktrees skipped at startup (owned by this process instance, foreign host and not yet stale, invalid marker, or unsafe to remove)")
	worktreesReconciledFailed  = metrics.NewCounter("ae_worktrees_reconciled_failed_total", "Marker worktrees whose removal failed at startup")
)

// errUnsafeWorktree marks refusals from isSafeToRemove so callers can tell a
// deliberate skip from a git failure.
var errUnsafeWorktree = errors.New("unsafe to remove")

// processInstanceID identifies this process for the lifetime of the process.
// PIDs cannot do that: the agent runs as PID 1 in its container, so after an
// in-place restart the new process has the same hostname and PID as the one
// that crashed.
var processInstanceID = uuid.NewString()

// worktreeMarker is the JSON body of worktreeMarkerFile.
type worktreeMarker struct {
	TaskID     string    `json:"task_id"`
	WorkID     string    `json:"work_id"`
	ClonePath  string    `json:"clone_path"`
	PID        int       `json:"pid"`
	Hostname   string    `json:"hostname,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// validateWorktreeMarker rejects markers whose fields cannot safely drive a
// removal: an empty or relative clone_path would make `git -C` and
// filepath.Abs resolve against the process cwd.
func validateWorktreeMarker(m worktreeMarker) error {
	if m.TaskID == "" {
		return errors.New("marker task_id is empty")
	}
	if m.ClonePath == "" {
		return errors.New("marker clone_path is empty")
	}
	if !filepath.IsAbs(m.ClonePath) {
		return fmt.Errorf("marker clone_path %q is not absolute", m.ClonePath)
	}
	return nil
}

// reconcileResult counts what reconcileAbandonedWorktrees did. Unmarked
// counts worktree-* directories without a marker (pre-upgrade leftovers or
// foreign directories); they are reported but never touched.
type reconcileResult struct {
	Removed, Skipped, Failed, Unmarked int
}

// worktreePathFor returns the per-task worktree directory: a sibling of the
// clone named after the full task UUID, so concurrent tasks cannot collide.
func worktreePathFor(clonePath string, taskID uuid.UUID) string {
	return filepath.Join(clonePath, "..", "worktree-"+taskID.String())
}

func repoCacheRootFromEnv() string {
	if v := os.Getenv(envRepoCacheRoot); v != "" {
		return v
	}
	return defaultRepoCacheRoot
}

func reconcileEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envWorktreeReconcile))) {
	case "off", "0", "false":
		return false
	}
	return true
}

// worktreeStaleAfterFromEnv returns AE_WORKTREE_STALE_AFTER, falling back to
// the default when unset, unparseable, or non-positive.
func worktreeStaleAfterFromEnv() time.Duration {
	return durationFromEnv(envWorktreeStaleAfter, defaultWorktreeStaleAfter)
}

// worktreeReconcileTimeoutFromEnv returns AE_WORKTREE_RECONCILE_TIMEOUT with
// the same fallback rules.
func worktreeReconcileTimeoutFromEnv() time.Duration {
	return durationFromEnv(envWorktreeReconcileTimeout, defaultWorktreeReconcileTimeout)
}

func durationFromEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// writeWorktreeMarker records ownership of a freshly created worktree. The
// marker name is added to the repository's info/exclude first so the
// handler's `git add -A` can never commit it; if that fails the marker is
// not written at all rather than risk landing in a task branch.
func writeWorktreeMarker(worktreePath, taskID, workID, clonePath string) error {
	if err := excludeMarkerFromGit(worktreePath); err != nil {
		return fmt.Errorf("exclude marker from git: %w", err)
	}
	hostname, _ := os.Hostname()
	m := worktreeMarker{
		TaskID:     taskID,
		WorkID:     workID,
		ClonePath:  clonePath,
		PID:        os.Getpid(),
		Hostname:   hostname,
		InstanceID: processInstanceID,
		CreatedAt:  time.Now().UTC(),
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(worktreePath, worktreeMarkerFile), data, 0o644)
}

func readWorktreeMarker(worktreePath string) (worktreeMarker, error) {
	var m worktreeMarker
	data, err := os.ReadFile(filepath.Join(worktreePath, worktreeMarkerFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse %s: %w", worktreeMarkerFile, err)
	}
	return m, nil
}

// excludeMu serialises appends to info/exclude; linked worktrees share the
// primary clone's info/exclude, and tasks are created concurrently.
var excludeMu sync.Mutex

func excludeMarkerFromGit(worktreePath string) error {
	return excludeFromGit(worktreePath, worktreeMarkerFile)
}

// excludeFromGit appends each pattern not already present to the
// repository's info/exclude (resolved with --git-path, so a linked
// worktree writes the shared file git actually reads).
func excludeFromGit(worktreePath string, patterns ...string) error {
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-path", "info/exclude").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git rev-parse --git-path info/exclude: %s: %w", strings.TrimSpace(string(out)), err)
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktreePath, excludePath)
	}

	excludeMu.Lock()
	defer excludeMu.Unlock()
	existing, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, p := range patterns {
		if !present[p] {
			present[p] = true
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + strings.Join(missing, "\n") + "\n")
	return err
}

// isSafeToRemove reports whether path may be handed to `git worktree remove`.
// It refuses the primary checkout (path equals clonePath, or .git is a
// directory), anything outside repoCacheRoot, and anything that is not a
// linked worktree (.git must be a regular file). Every refusal wraps
// errUnsafeWorktree.
func isSafeToRemove(path, clonePath, repoCacheRoot string) error {
	// Resolve symlinks so a worktree-* entry that links outside the root, or
	// a root/clone reached through a link, is judged by its real location.
	absPath, err := realPath(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", errUnsafeWorktree, path, err)
	}
	absRoot, err := realPath(repoCacheRoot)
	if err != nil {
		return fmt.Errorf("%w: repo cache root %s: %v", errUnsafeWorktree, repoCacheRoot, err)
	}
	absClone, err := realPath(clonePath)
	if err != nil {
		// The clone may be gone; compare against its cleaned absolute form.
		if absClone, err = filepath.Abs(clonePath); err != nil {
			return fmt.Errorf("%w: clone path %s: %v", errUnsafeWorktree, clonePath, err)
		}
	}

	if samePath(absPath, absClone) {
		return fmt.Errorf("%w: %s is the primary checkout", errUnsafeWorktree, path)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s is not under repo cache root %s", errUnsafeWorktree, path, repoCacheRoot)
	}
	info, err := os.Lstat(filepath.Join(absPath, ".git"))
	if err != nil {
		return fmt.Errorf("%w: %s is not a linked worktree: %v", errUnsafeWorktree, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: %s: .git is a directory (primary checkout)", errUnsafeWorktree, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a linked worktree: .git is %s", errUnsafeWorktree, path, info.Mode())
	}
	return nil
}

// realPath returns the absolute, symlink-free form of p.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// removeWorktree is the single removal path for both the handler's cleanup
// and startup reconciliation. It never runs git without isSafeToRemove
// passing, then removes the worktree and prunes stale registrations.
func removeWorktree(ctx context.Context, clonePath, worktreePath, repoCacheRoot string) error {
	if err := isSafeToRemove(worktreePath, clonePath, repoCacheRoot); err != nil {
		return err
	}
	rm := exec.CommandContext(ctx, "git", "-C", clonePath, "worktree", "remove", "--force", worktreePath)
	if out, err := rm.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %s: %w", strings.TrimSpace(string(out)), err)
	}
	prune := exec.CommandContext(ctx, "git", "-C", clonePath, "worktree", "prune")
	if out, err := prune.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree prune: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// reconcileAbandonedWorktrees removes agent-owned worktrees left behind by a
// previous process (crash, kill, OOM) so their disk and registrations are
// reclaimed. Candidates are worktree-* directories directly under
// repoCacheRoot or one level below it (next to /repo-cache/<product>/<repo>/
// clones). Only candidates carrying a valid marker are considered, and a
// marker is removable only when it was written on this host by a different
// process instance (the writer is gone; PIDs are not consulted because a
// restarted container reuses hostname and PID 1) or when it is older than
// AE_WORKTREE_STALE_AFTER — sibling replicas share the repo-cache volume and
// their in-progress worktrees must not be mistaken for abandoned ones.
// Everything else is skipped and logged.
func reconcileAbandonedWorktrees(ctx context.Context, logger *logging.Logger, repoCacheRoot string) reconcileResult {
	var res reconcileResult
	if _, err := os.Stat(repoCacheRoot); err != nil {
		logger.Warn(ctx, "worktree reconcile: repo cache root not accessible, nothing to do", map[string]interface{}{
			"repo_cache_root": repoCacheRoot, "err": err.Error(),
		})
		return res
	}

	hostname, _ := os.Hostname()
	staleAfter := worktreeStaleAfterFromEnv()
	now := time.Now()
	candidates := findWorktreeCandidates(repoCacheRoot)
	for i, path := range candidates {
		if ctx.Err() != nil {
			logger.Warn(ctx, "worktree reconcile: deadline reached, leaving remaining candidates for next start", map[string]interface{}{
				"remaining": len(candidates) - i, "err": ctx.Err().Error(),
			})
			break
		}
		m, err := readWorktreeMarker(path)
		if errors.Is(err, os.ErrNotExist) {
			res.Unmarked++ // no marker: not ours, never touched
			continue
		}
		fields := map[string]interface{}{"path": path}
		if err == nil {
			err = validateWorktreeMarker(m)
		}
		if err != nil {
			res.Skipped++
			fields["err"] = err.Error()
			logger.Warn(ctx, "worktree reconcile: unreadable or invalid marker, skipping", fields)
			continue
		}
		fields["task_id"] = m.TaskID
		fields["work_id"] = m.WorkID
		fields["clone_path"] = m.ClonePath
		fields["owner_pid"] = m.PID
		fields["owner_host"] = m.Hostname
		fields["owner_instance"] = m.InstanceID
		fields["created_at"] = m.CreatedAt

		if m.InstanceID == processInstanceID {
			res.Skipped++
			logger.Info(ctx, "worktree reconcile: owned by this process, skipping", fields)
			continue
		}
		age, stale := "unknown", false
		if !m.CreatedAt.IsZero() {
			age = now.Sub(m.CreatedAt).Round(time.Second).String()
			stale = now.Sub(m.CreatedAt) > staleAfter
		}
		if m.Hostname != hostname && !stale {
			res.Skipped++
			fields["age"] = age
			fields["stale_after"] = staleAfter.String()
			logger.Info(ctx, "worktree reconcile: marker from another host and not yet stale, skipping", fields)
			continue
		}
		// Same host with a different instance ID (or stale): the writer is gone.
		err = removeWorktree(ctx, m.ClonePath, path, repoCacheRoot)
		switch {
		case err == nil:
			res.Removed++
			logger.Info(ctx, "worktree reconcile: removed abandoned worktree", fields)
		case errors.Is(err, errUnsafeWorktree):
			res.Skipped++
			fields["err"] = err.Error()
			logger.Warn(ctx, "worktree reconcile: refused to remove, skipping", fields)
		default:
			res.Failed++
			logger.Error(ctx, "worktree reconcile: removal failed", err, fields)
		}
	}

	worktreesReconciledRemoved.Add(int64(res.Removed))
	worktreesReconciledSkipped.Add(int64(res.Skipped))
	worktreesReconciledFailed.Add(int64(res.Failed))
	logger.Info(ctx, "worktree reconcile complete", map[string]interface{}{
		"repo_cache_root": repoCacheRoot,
		"stale_after":     staleAfter.String(),
		"candidates":      len(candidates),
		"removed":         res.Removed,
		"skipped":         res.Skipped,
		"failed":          res.Failed,
		"unmarked":        res.Unmarked,
	})
	return res
}

// findWorktreeCandidates lists worktree-* directories at depth 1 and 2 under
// root, sorted for deterministic logs.
func findWorktreeCandidates(root string) []string {
	var dirs []string
	for _, pattern := range []string{
		filepath.Join(root, "worktree-*"),
		filepath.Join(root, "*", "worktree-*"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			if info, err := os.Stat(match); err == nil && info.IsDir() {
				dirs = append(dirs, match)
			}
		}
	}
	sort.Strings(dirs)
	return dirs
}
