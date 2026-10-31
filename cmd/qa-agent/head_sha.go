package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/gitref"
	"github.com/google/uuid"
)

// D1.5: QA tests the verified head SHA carried on qa.ready (the SHA the
// implementation evidence was recorded against and the review examined),
// checked out detached — never the branch tip by name. If the remote branch
// now points elsewhere the branch moved after verification; QA blocks
// instead of silently testing unverified code. If the branch is gone (the
// usual state after a merge with branch auto-delete) the verified SHA is
// still accepted when it is reachable from an origin/* ref — the merge
// commit keeps it "remotely resolvable" — and blocked only when it is not.
//
// The rule itself is gitref.ResolveTarget, shared with review-agent. The
// compatibility window (EVIDENCE_COMPAT_WINDOW) governs a qa.ready without
// head_sha: legacy tests the tip with a warning, strict blocks.

// Sources recorded as head_sha_source in the QA report.
const (
	sourceEvidence           = "evidence"
	sourceEvidenceBranchGone = "evidence_branch_deleted"
	sourceLegacyTip          = "legacy_branch_tip"
	sourceMainClone          = "main_clone"
)

const (
	// qaWorktreePrefix names QA worktrees next to the clone. The slot is
	// per attempt (work item id + random suffix): qa-agent runs as several
	// replicas sharing the repo cache, and a redelivered qa.ready on a
	// second replica must never land in — or reclaim — the first replica's
	// live worktree.
	qaWorktreePrefix = "qa-worktree-"

	// worktreeMarkerFile records ownership of a QA worktree (same name as
	// the implementation agent's marker so an existing info/exclude entry
	// covers it). Reclaim only ever removes slots whose marker is ours or
	// stale, or markerless slots older than the stale threshold.
	worktreeMarkerFile = ".ae-worktree.json"

	// envWorktreeStaleAfter is a Go duration after which a slot owned by
	// another instance (or without a marker) is considered abandoned.
	// Shared with the implementation agent.
	envWorktreeStaleAfter     = "AE_WORKTREE_STALE_AFTER"
	defaultWorktreeStaleAfter = 24 * time.Hour
)

var errUnsafeWorktree = errors.New("refusing to remove path")

// processInstanceID identifies this process for the lifetime of the
// process. PIDs cannot: the agent runs as PID 1 in its container, so a
// restarted replica has the same hostname and PID as the one that died.
var processInstanceID = uuid.NewString()

// worktreeMarker is the JSON body of worktreeMarkerFile.
type worktreeMarker struct {
	Agent      string    `json:"agent"`
	WorkID     string    `json:"work_id"`
	ClonePath  string    `json:"clone_path"`
	Hostname   string    `json:"hostname,omitempty"`
	InstanceID string    `json:"instance_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// reclaimPolicy is what decides whether a foreign slot may be removed. It
// is a value so tests can pin the instance id, threshold and clock.
type reclaimPolicy struct {
	InstanceID string
	StaleAfter time.Duration
	Now        time.Time
}

func reclaimPolicyFromEnv() reclaimPolicy {
	return reclaimPolicy{InstanceID: processInstanceID, StaleAfter: worktreeStaleAfterFromEnv(), Now: time.Now()}
}

// worktreeStaleAfterFromEnv returns AE_WORKTREE_STALE_AFTER, falling back to
// the default when unset, unparseable, or non-positive.
func worktreeStaleAfterFromEnv() time.Duration {
	if v := os.Getenv(envWorktreeStaleAfter); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultWorktreeStaleAfter
}

// checkoutResult is what checkoutVerifiedHead hands the handler.
type checkoutResult struct {
	// Worktree is the detached worktree to run QA in. Empty means QA runs
	// in the main clone as it did before (legacy fallback paths only).
	Worktree string
	// SHA is the commit actually checked out in Worktree (from git
	// rev-parse HEAD there), recorded in the QA report as the SHA tested.
	// Empty when Worktree is empty.
	SHA string
	// Branch is the branch QA resolved (from the message or the
	// agent/<work-id>/* search).
	Branch string
	// Source labels SHA for the report: sourceEvidence,
	// sourceEvidenceBranchGone, sourceLegacyTip or sourceMainClone.
	Source string
	// Legacy is true when the message carried no head_sha and the tip was
	// used under the legacy window.
	Legacy bool
	// BlockReason, when non-empty, means QA must not run: the handler
	// blocks the work item with it.
	BlockReason string
	// Cleanup removes the worktree; nil when none was created.
	Cleanup func()
}

// checkoutVerifiedHead fetches origin, resolves the branch (knownBranch from
// the qa.ready message, else the first origin/agent/<workID>/* branch),
// compares the remote tip with headSHA under the compatibility window, and
// creates a detached worktree at the resolved commit in a fresh per-attempt
// slot, after reclaiming abandoned slots next to the clone under policy.
//
// With a head SHA present every failure is a block: QA must test that
// commit or nothing. Without one (legacy window) the pre-D1.5 fallbacks are
// kept: fetch failure or no branch → QA in the main clone; worktree failure
// → QA in the main clone.
func checkoutVerifiedHead(ctx context.Context, clonePath string, workID uuid.UUID, knownBranch, headSHA string, window evidence.CompatWindow) checkoutResult {
	return checkoutVerifiedHeadWithPolicy(ctx, clonePath, workID, knownBranch, headSHA, window, reclaimPolicyFromEnv())
}

func checkoutVerifiedHeadWithPolicy(ctx context.Context, clonePath string, workID uuid.UUID, knownBranch, headSHA string, window evidence.CompatWindow, policy reclaimPolicy) checkoutResult {
	// Validate once at entry: a malformed head_sha is never compared with
	// anything or handed to git; it reads as absent from here on (block in
	// strict, tip with a warning in legacy) and is logged.
	rawHead := headSHA
	headSHA, headOK := gitref.Sanitize(headSHA)
	if !headOK {
		log.Printf("qa-agent: WARNING work_id=%s qa.ready head_sha %q is not a well-formed object name — ignoring it", workID, rawHead)
	}

	// A strict-window message without a usable SHA blocks before any git
	// work.
	if headSHA == "" {
		if pre := gitref.ResolveTarget("qa.ready", rawHead, "", window); pre.Blocked {
			return checkoutResult{BlockReason: pre.Reason}
		}
	}

	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "--prune", "origin")
	fetchCmd.Dir = clonePath
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		log.Printf("qa-agent: git fetch failed: %s: %v", string(out), err)
		if headSHA != "" {
			return checkoutResult{BlockReason: fmt.Sprintf("cannot fetch origin to verify head %s: %v", headSHA, err)}
		}
		// Legacy window, no SHA: QA in the main clone, as before.
		return checkoutResult{Legacy: true, Source: sourceMainClone}
	}

	branch := knownBranch
	if branch != "" {
		log.Printf("qa-agent: using PR branch %s from qa.ready message", branch)
	} else {
		branch = findAgentBranch(ctx, clonePath, workID)
		if branch == "" {
			if headSHA != "" {
				return checkoutResult{BlockReason: fmt.Sprintf("qa.ready names verified head %s but no branch; cannot confirm the branch still points at it", headSHA)}
			}
			log.Printf("qa-agent: no feature branch found matching origin/agent/%s/* — running QA on main", workID)
			return checkoutResult{Legacy: true, Source: sourceMainClone}
		}
	}
	log.Printf("qa-agent: found feature branch %s for work_id=%s", branch, workID)

	tip, tipErr := gitref.RemoteTip(ctx, clonePath, branch)
	if tipErr != nil {
		log.Printf("qa-agent: resolve remote tip of %s: %v", branch, tipErr)
		if headSHA != "" {
			return checkoutResult{BlockReason: fmt.Sprintf("cannot verify that branch %s still points at verified head %s: %v", branch, headSHA, tipErr)}
		}
	}
	target := gitref.ResolveTarget("qa.ready", rawHead, tip, window)
	source := sourceEvidence
	if target.BranchGone {
		// The branch was deleted after verification (normally: merged and
		// auto-deleted). D1.5 only requires the verified SHA to be remotely
		// resolvable; the fetch above brought in every origin ref, so
		// accept the SHA when some origin/* ref still contains it.
		reachable, refs := gitref.ReachableFromRemote(ctx, clonePath, headSHA)
		if !reachable {
			return checkoutResult{Branch: branch, BlockReason: fmt.Sprintf("verified head %s is not reachable from any remote ref after branch %s was deleted (%s)", headSHA, branch, target.Reason)}
		}
		log.Printf("qa-agent: work_id=%s branch %s is gone from origin but verified head %s is reachable via %s — testing it", workID, branch, headSHA, strings.Join(refs, ", "))
		target = gitref.Resolution{SHA: headSHA}
		source = sourceEvidenceBranchGone
	}
	if target.Blocked {
		return checkoutResult{Branch: branch, BlockReason: target.Reason}
	}
	if target.Legacy {
		source = sourceLegacyTip
		log.Printf("qa-agent: WARNING work_id=%s branch=%s: %s", workID, branch, target.Reason)
	}

	// Detached checkout of the exact commit; only a legacy message whose
	// remote tip could not be read falls back to the origin/<branch> ref.
	ref := target.SHA
	if ref == "" {
		ref = "origin/" + branch
	}

	// Abandoned slots (a killed run never reaches Cleanup) must not pile up
	// or wedge the item; live slots of sibling replicas must not be
	// touched. Failures here are logged, never fatal: our own slot is fresh.
	reclaimAbandonedWorktrees(ctx, clonePath, policy)

	worktreePath, err := newQAWorktreePath(clonePath, workID)
	if err != nil {
		if target.SHA != "" && !target.Legacy {
			return checkoutResult{Branch: branch, BlockReason: fmt.Sprintf("cannot allocate a QA worktree slot to check out verified head %s: %v", target.SHA, err)}
		}
		log.Printf("qa-agent: allocate worktree slot: %v — falling back to main", err)
		return checkoutResult{Branch: branch, Legacy: target.Legacy, Source: sourceMainClone}
	}
	wtCmd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", worktreePath, ref)
	wtCmd.Dir = clonePath
	if out, err := wtCmd.CombinedOutput(); err != nil {
		if target.SHA != "" && !target.Legacy {
			return checkoutResult{Branch: branch, BlockReason: fmt.Sprintf("cannot check out verified head %s of branch %s: %v: %s", target.SHA, branch, err, strings.TrimSpace(string(out)))}
		}
		log.Printf("qa-agent: git worktree add failed: %s: %v — falling back to main", string(out), err)
		return checkoutResult{Branch: branch, Legacy: target.Legacy, Source: sourceMainClone}
	}
	cleanup := func() {
		if err := removeWorktree(context.Background(), clonePath, worktreePath); err != nil {
			log.Printf("qa-agent: remove QA worktree %s: %v", worktreePath, err)
		}
	}
	if err := writeWorktreeMarker(worktreePath, workID, clonePath, policy); err != nil {
		// Without a marker a sibling replica could only remove this slot
		// after the stale threshold; log and carry on.
		log.Printf("qa-agent: write worktree marker in %s: %v", worktreePath, err)
	}

	// Record what is actually at HEAD in the worktree; for a verified SHA
	// this must be that SHA.
	headCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	headCmd.Dir = worktreePath
	headOut, headErr := headCmd.Output()
	tested := gitref.Normalise(string(headOut))
	if headErr != nil || tested == "" {
		cleanup()
		return checkoutResult{Branch: branch, BlockReason: fmt.Sprintf("cannot read HEAD of QA worktree for branch %s: %v", branch, headErr)}
	}
	if target.SHA != "" && tested != target.SHA {
		cleanup()
		return checkoutResult{Branch: branch, BlockReason: fmt.Sprintf("QA worktree is at %s, not the resolved head %s", tested, target.SHA)}
	}

	log.Printf("qa-agent: created QA worktree at %s on branch %s at %s (detached, source=%s)", worktreePath, branch, tested, source)
	return checkoutResult{Worktree: worktreePath, SHA: tested, Branch: branch, Source: source, Legacy: target.Legacy, Cleanup: cleanup}
}

// newQAWorktreePath allocates a fresh per-attempt slot next to the clone:
// qa-worktree-<work id>-<8 random hex>. The random suffix keeps two
// replicas (or a redelivery racing a live run) apart; the full work id keeps
// the directory attributable.
func newQAWorktreePath(clonePath string, workID uuid.UUID) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random slot suffix: %w", err)
	}
	return filepath.Join(clonePath, "..", qaWorktreePrefix+workID.String()+"-"+hex.EncodeToString(b[:])), nil
}

// writeWorktreeMarker records ownership of a freshly created worktree.
func writeWorktreeMarker(worktreePath string, workID uuid.UUID, clonePath string, policy reclaimPolicy) error {
	hostname, _ := os.Hostname()
	absClone, err := filepath.Abs(clonePath)
	if err != nil {
		absClone = clonePath
	}
	m := worktreeMarker{
		Agent:      agentID,
		WorkID:     workID.String(),
		ClonePath:  absClone,
		Hostname:   hostname,
		InstanceID: policy.InstanceID,
		CreatedAt:  policy.Now.UTC(),
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

// reclaimDecision is why reclaimAbandonedWorktrees did or did not remove a
// slot; exposed for tests and logs.
type reclaimDecision string

const (
	reclaimOwn           reclaimDecision = "own_instance"
	reclaimStaleMarker   reclaimDecision = "stale_marker"
	reclaimStaleUnmarked reclaimDecision = "stale_unmarked"
	keepForeignLive      reclaimDecision = "foreign_live"
	keepUnmarkedYoung    reclaimDecision = "unmarked_young"
	keepUnreadable       reclaimDecision = "unreadable"
)

// decideReclaim is the pure ownership rule for one qa-worktree-* slot:
// remove when the marker is from this instance or older than StaleAfter;
// remove a markerless slot only when its directory is older than
// StaleAfter; keep everything else.
func decideReclaim(policy reclaimPolicy, m worktreeMarker, markerErr error, dirModTime time.Time) (remove bool, why reclaimDecision) {
	switch {
	case markerErr == nil:
		if m.InstanceID != "" && m.InstanceID == policy.InstanceID {
			return true, reclaimOwn
		}
		if !m.CreatedAt.IsZero() && policy.Now.Sub(m.CreatedAt) > policy.StaleAfter {
			return true, reclaimStaleMarker
		}
		return false, keepForeignLive
	case errors.Is(markerErr, os.ErrNotExist):
		if !dirModTime.IsZero() && policy.Now.Sub(dirModTime) > policy.StaleAfter {
			return true, reclaimStaleUnmarked
		}
		return false, keepUnmarkedYoung
	default:
		// Unreadable or malformed marker: someone's slot, age unknown.
		// Treat like a markerless slot and only reclaim once old.
		if !dirModTime.IsZero() && policy.Now.Sub(dirModTime) > policy.StaleAfter {
			return true, reclaimStaleUnmarked
		}
		return false, keepUnreadable
	}
}

// reclaimAbandonedWorktrees prunes registrations whose directories are gone
// and removes qa-worktree-* siblings of the clone that decideReclaim allows.
// It returns the paths removed. Every removal goes through isSafeToRemove.
func reclaimAbandonedWorktrees(ctx context.Context, clonePath string, policy reclaimPolicy) []string {
	pruneCmd := exec.CommandContext(ctx, "git", "worktree", "prune")
	pruneCmd.Dir = clonePath
	if out, err := pruneCmd.CombinedOutput(); err != nil {
		log.Printf("qa-agent: git worktree prune: %v: %s", err, strings.TrimSpace(string(out)))
	}

	parent := filepath.Dir(filepath.Clean(clonePath))
	entries, err := os.ReadDir(parent)
	if err != nil {
		log.Printf("qa-agent: list worktree slots under %s: %v", parent, err)
		return nil
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), qaWorktreePrefix) {
			continue
		}
		path := filepath.Join(parent, e.Name())
		var modTime time.Time
		if info, err := e.Info(); err == nil {
			modTime = info.ModTime()
		}
		m, merr := readWorktreeMarker(path)
		remove, why := decideReclaim(policy, m, merr, modTime)
		if !remove {
			log.Printf("qa-agent: leaving QA worktree %s alone (%s; owner_instance=%q created_at=%s dir_mtime=%s stale_after=%s)", path, why, m.InstanceID, m.CreatedAt.Format(time.RFC3339), modTime.Format(time.RFC3339), policy.StaleAfter)
			continue
		}
		if err := removeWorktree(ctx, clonePath, path); err != nil {
			log.Printf("qa-agent: reclaim QA worktree %s (%s): %v", path, why, err)
			continue
		}
		log.Printf("qa-agent: reclaimed abandoned QA worktree %s (%s; owner_instance=%q created_at=%s)", path, why, m.InstanceID, m.CreatedAt.Format(time.RFC3339))
		removed = append(removed, path)
	}
	return removed
}

// removeWorktree unregisters and deletes worktreePath. It refuses anything
// isSafeToRemove rejects, so a misconfigured path can never delete the
// primary clone or an unrelated directory.
func removeWorktree(ctx context.Context, clonePath, worktreePath string) error {
	if err := isSafeToRemove(worktreePath, clonePath); err != nil {
		return err
	}
	if worktreeRegistered(ctx, clonePath, worktreePath) {
		rmCmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", worktreePath)
		rmCmd.Dir = clonePath
		if out, err := rmCmd.CombinedOutput(); err != nil {
			log.Printf("qa-agent: git worktree remove --force %s: %v: %s", worktreePath, err, strings.TrimSpace(string(out)))
		}
	}
	if _, err := os.Lstat(worktreePath); err == nil {
		// Not registered (or git could not remove it): a plain leftover
		// directory. The safety check above already bounded what it can be.
		if err := os.RemoveAll(worktreePath); err != nil {
			return fmt.Errorf("remove leftover worktree directory %s: %w", worktreePath, err)
		}
	}
	// Drop the registration if the directory is gone but git still lists it.
	pruneCmd := exec.CommandContext(ctx, "git", "worktree", "prune")
	pruneCmd.Dir = clonePath
	pruneCmd.CombinedOutput() //nolint:errcheck
	return nil
}

// worktreeRegistered reports whether git lists worktreePath as a linked
// worktree of clonePath.
func worktreeRegistered(ctx context.Context, clonePath, worktreePath string) bool {
	listCmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	listCmd.Dir = clonePath
	out, err := listCmd.Output()
	if err != nil {
		return false
	}
	want, err := filepath.Abs(worktreePath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		if samePath(p, want) {
			return true
		}
	}
	return false
}

// isSafeToRemove reports whether path may be removed as a QA worktree. It
// mirrors cmd/implementation-agent's guard: the path must be a sibling of
// the clone named qa-worktree-*, must not be the primary checkout (neither
// the clone path nor a directory whose .git is a directory), and must not
// resolve outside the clone's parent through a symlink. Every refusal wraps
// errUnsafeWorktree.
func isSafeToRemove(path, clonePath string) error {
	absPath, err := realPath(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", errUnsafeWorktree, path, err)
	}
	absClone, err := realPath(clonePath)
	if err != nil {
		if absClone, err = filepath.Abs(clonePath); err != nil {
			return fmt.Errorf("%w: clone path %s: %v", errUnsafeWorktree, clonePath, err)
		}
	}
	if samePath(absPath, absClone) {
		return fmt.Errorf("%w: %s is the primary checkout", errUnsafeWorktree, path)
	}
	if !strings.HasPrefix(filepath.Base(absPath), qaWorktreePrefix) {
		return fmt.Errorf("%w: %s is not a %s* directory", errUnsafeWorktree, path, qaWorktreePrefix)
	}
	if !samePath(filepath.Dir(absPath), filepath.Dir(absClone)) {
		return fmt.Errorf("%w: %s is not a sibling of the clone %s", errUnsafeWorktree, path, clonePath)
	}
	// A linked worktree has a .git *file*; a .git directory is a primary
	// checkout and must never be deleted here. A missing .git (plain
	// leftover directory) is allowed: the name and location checks above
	// bound it to our own worktree slot.
	if info, err := os.Lstat(filepath.Join(absPath, ".git")); err == nil && info.IsDir() {
		return fmt.Errorf("%w: %s: .git is a directory (primary checkout)", errUnsafeWorktree, path)
	}
	return nil
}

// realPath returns the absolute, symlink-free form of p. When p does not
// exist, its cleaned absolute form is returned so callers can still reason
// about where it would be.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(abs), nil
		}
		return "", err
	}
	return resolved, nil
}

// samePath compares two paths after cleaning, case-insensitively on
// case-insensitive filesystems (Windows), which is where git reports
// worktree paths with forward slashes and mixed drive-letter case.
func samePath(a, b string) bool {
	a = filepath.Clean(filepath.FromSlash(a))
	b = filepath.Clean(filepath.FromSlash(b))
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// findAgentBranch returns the first origin/agent/<workID>/* branch (without
// the origin/ prefix), or "" when none exists. Pre-D1.5 fallback for
// producers that omit branch on qa.ready.
func findAgentBranch(ctx context.Context, clonePath string, workID uuid.UUID) string {
	branchCmd := exec.CommandContext(ctx, "git", "branch", "-r", "--list", fmt.Sprintf("origin/agent/%s/*", workID.String()))
	branchCmd.Dir = clonePath
	branchOut, err := branchCmd.CombinedOutput()
	if err != nil || len(bytes.TrimSpace(branchOut)) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(branchOut)), "\n")
	return strings.TrimPrefix(strings.TrimSpace(lines[0]), "origin/")
}
