package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
)

// repoFixture is a bare origin, a pusher clone that creates commits, and the
// service clone that checkoutVerifiedHead operates on.
type repoFixture struct {
	origin, pusher, clone string
	workID                uuid.UUID
	branch                string
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	f := &repoFixture{
		origin: filepath.Join(root, "origin.git"),
		pusher: filepath.Join(root, "pusher"),
		clone:  filepath.Join(root, "clone"),
		workID: uuid.New(),
	}
	f.branch = "agent/" + f.workID.String() + "/feature"

	git(t, root, "init", "--bare", "-b", "main", f.origin)
	git(t, root, "clone", "-q", f.origin, f.pusher)
	writeFile(t, filepath.Join(f.pusher, "a.txt"), "one\n")
	git(t, f.pusher, "add", ".")
	git(t, f.pusher, "commit", "-q", "-m", "one")
	git(t, f.pusher, "push", "-q", "origin", "HEAD:main")

	// The service clone is taken before the feature branch exists so the
	// helper's own fetch is what brings the verified commit in.
	git(t, root, "clone", "-q", f.origin, f.clone)

	git(t, f.pusher, "checkout", "-q", "-b", f.branch)
	writeFile(t, filepath.Join(f.pusher, "b.txt"), "two\n")
	git(t, f.pusher, "add", ".")
	git(t, f.pusher, "commit", "-q", "-m", "two")
	git(t, f.pusher, "push", "-q", "origin", f.branch)
	return f
}

// push adds a commit to the feature branch and returns its SHA.
func (f *repoFixture) push(t *testing.T, name string) string {
	t.Helper()
	writeFile(t, filepath.Join(f.pusher, name), name+"\n")
	git(t, f.pusher, "add", ".")
	git(t, f.pusher, "commit", "-q", "-m", name)
	git(t, f.pusher, "push", "-q", "origin", f.branch)
	return f.head(t)
}

// mergeAndDeleteBranch merges the feature branch into main (no-ff, like a
// GitHub merge), pushes main and deletes the branch on origin.
func (f *repoFixture) mergeAndDeleteBranch(t *testing.T) {
	t.Helper()
	git(t, f.pusher, "checkout", "-q", "main")
	git(t, f.pusher, "merge", "-q", "--no-ff", "-m", "merge "+f.branch, f.branch)
	git(t, f.pusher, "push", "-q", "origin", "main")
	git(t, f.pusher, "push", "-q", "origin", "--delete", f.branch)
}

func (f *repoFixture) head(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, f.pusher, "rev-parse", "HEAD"))
}

// slots lists qa-worktree-* directories next to the clone.
func (f *repoFixture) slots(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(f.clone), qaWorktreePrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func worktreeHead(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
}

var testPolicy = reclaimPolicy{InstanceID: "test-instance", StaleAfter: 24 * time.Hour, Now: time.Now()}

func checkout(f *repoFixture, branch, sha string, window evidence.CompatWindow) checkoutResult {
	return checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, branch, sha, window, testPolicy)
}

func TestCheckoutVerifiedHead_ExactSHA(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)

	// Upper-cased with surrounding whitespace: normalised once at entry.
	res := checkout(f, f.branch, " "+strings.ToUpper(verified)+"\n", evidence.CompatWindowStrict)
	if res.BlockReason != "" {
		t.Fatalf("unexpected block: %s", res.BlockReason)
	}
	if res.Cleanup != nil {
		defer res.Cleanup()
	}
	if res.Worktree == "" || res.Legacy || res.Source != sourceEvidence {
		t.Fatalf("expected a non-legacy evidence worktree, got %+v", res)
	}
	if res.SHA != verified {
		t.Fatalf("SHA tested = %s, want %s", res.SHA, verified)
	}
	if got := worktreeHead(t, res.Worktree); got != verified {
		t.Fatalf("worktree HEAD = %s, want %s", got, verified)
	}
	if _, err := os.Stat(filepath.Join(res.Worktree, "b.txt")); err != nil {
		t.Fatalf("worktree must contain the feature commit: %v", err)
	}
	// Detached: no branch name at HEAD.
	if ref := strings.TrimSpace(gitOut(t, res.Worktree, "rev-parse", "--abbrev-ref", "HEAD")); ref != "HEAD" {
		t.Fatalf("worktree must be detached, got ref %q", ref)
	}
	// Per-attempt slot: named after the full work id plus a suffix, with a
	// marker owned by this instance.
	base := filepath.Base(res.Worktree)
	if !strings.HasPrefix(base, qaWorktreePrefix+f.workID.String()+"-") || len(base) != len(qaWorktreePrefix+f.workID.String()+"-")+8 {
		t.Fatalf("slot name %q must be qa-worktree-<work id>-<8 hex>", base)
	}
	m, err := readWorktreeMarker(res.Worktree)
	if err != nil || m.InstanceID != testPolicy.InstanceID || m.WorkID != f.workID.String() || m.Agent != agentID || m.CreatedAt.IsZero() {
		t.Fatalf("marker = %+v err=%v", m, err)
	}
	res.Cleanup()
	res.Cleanup = nil
	if _, err := os.Stat(res.Worktree); !os.IsNotExist(err) {
		t.Fatalf("cleanup must remove the worktree (stat err=%v)", err)
	}
	if worktreeRegistered(context.Background(), f.clone, res.Worktree) {
		t.Fatalf("cleanup must unregister the worktree")
	}
}

// Two attempts for the same work item (a redelivery racing a live run on
// another replica) get distinct slots and the second never disturbs the
// first.
func TestCheckoutVerifiedHead_ConcurrentAttemptsGetDistinctSlots(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)

	replicaA := reclaimPolicy{InstanceID: "replica-a", StaleAfter: 24 * time.Hour, Now: time.Now()}
	replicaB := reclaimPolicy{InstanceID: "replica-b", StaleAfter: 24 * time.Hour, Now: time.Now()}

	first := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, replicaA)
	if first.BlockReason != "" {
		t.Fatalf("first: %s", first.BlockReason)
	}
	defer first.Cleanup()
	writeFile(t, filepath.Join(first.Worktree, "in-progress.txt"), "live\n")

	second := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, replicaB)
	if second.BlockReason != "" {
		t.Fatalf("second: %s", second.BlockReason)
	}
	defer second.Cleanup()

	if first.Worktree == second.Worktree {
		t.Fatalf("attempts must not share a slot: %s", first.Worktree)
	}
	if _, err := os.Stat(filepath.Join(first.Worktree, "in-progress.txt")); err != nil {
		t.Fatalf("the live worktree of the other replica must be untouched: %v", err)
	}
	if worktreeHead(t, first.Worktree) != verified || worktreeHead(t, second.Worktree) != verified {
		t.Fatalf("both slots must be at the verified SHA")
	}
}

func TestCheckoutVerifiedHead_BranchMovedBlocks(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)
	moved := f.push(t, "c.txt")

	for _, window := range []evidence.CompatWindow{evidence.CompatWindowLegacy, evidence.CompatWindowStrict} {
		res := checkout(f, f.branch, verified, window)
		if res.Cleanup != nil {
			res.Cleanup()
		}
		want := "branch moved after verification: expected " + verified + ", found " + moved
		if res.BlockReason != want {
			t.Fatalf("window=%s BlockReason = %q, want %q", window, res.BlockReason, want)
		}
		if res.Worktree != "" || res.SHA != "" {
			t.Fatalf("a blocked checkout must not hand back a worktree: %+v", res)
		}
	}
	if slots := f.slots(t); len(slots) != 0 {
		t.Fatalf("blocked checkout must not leave a worktree: %v", slots)
	}
}

// After a merge with branch auto-delete the branch is gone but the verified
// SHA is reachable through origin/main: QA proceeds on that exact SHA.
func TestCheckoutVerifiedHead_BranchDeletedAfterMergeProceeds(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)
	f.mergeAndDeleteBranch(t)

	res := checkout(f, f.branch, verified, evidence.CompatWindowStrict)
	if res.BlockReason != "" {
		t.Fatalf("unexpected block: %s", res.BlockReason)
	}
	if res.Cleanup != nil {
		defer res.Cleanup()
	}
	if res.SHA != verified || worktreeHead(t, res.Worktree) != verified {
		t.Fatalf("QA must test the verified SHA %s, got %+v", verified, res)
	}
	if res.Source != sourceEvidenceBranchGone || res.Legacy {
		t.Fatalf("source must record the deleted branch, got %+v", res)
	}
	// Not the merge commit: the worktree is at the verified SHA itself.
	if _, err := os.Stat(filepath.Join(res.Worktree, "b.txt")); err != nil {
		t.Fatalf("worktree must contain the feature commit: %v", err)
	}
	if ref := strings.TrimSpace(gitOut(t, res.Worktree, "rev-parse", "--abbrev-ref", "HEAD")); ref != "HEAD" {
		t.Fatalf("worktree must be detached, got ref %q", ref)
	}
}

// A branch deleted without merging leaves the verified SHA unreachable from
// every origin ref, even when the clone fetched it earlier: block.
func TestCheckoutVerifiedHead_BranchDeletedUnreachableBlocks(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)

	// First run fetches the commit into the clone's object store.
	first := checkout(f, f.branch, verified, evidence.CompatWindowStrict)
	if first.BlockReason != "" {
		t.Fatalf("setup run blocked: %s", first.BlockReason)
	}
	first.Cleanup()

	git(t, f.pusher, "push", "-q", "origin", "--delete", f.branch)

	for _, window := range []evidence.CompatWindow{evidence.CompatWindowLegacy, evidence.CompatWindowStrict} {
		res := checkout(f, f.branch, verified, window)
		if res.Cleanup != nil {
			res.Cleanup()
		}
		if res.BlockReason == "" || res.Worktree != "" {
			t.Fatalf("window=%s: unreachable verified SHA must block, got %+v", window, res)
		}
		for _, want := range []string{"not reachable from any remote ref", verified, "found (branch not on remote)"} {
			if !strings.Contains(res.BlockReason, want) {
				t.Fatalf("window=%s: BlockReason %q must contain %q", window, res.BlockReason, want)
			}
		}
	}
}

// Reclaim: an abandoned slot of this instance and a stale foreign slot are
// removed; a fresh foreign slot (a sibling replica's live run) is not.
func TestReclaimAbandonedWorktrees_OwnershipRules(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)
	now := time.Now()
	mine := reclaimPolicy{InstanceID: "me", StaleAfter: 24 * time.Hour, Now: now}
	// Distinct instance ids: the setup checkouts each run the reclaim sweep
	// under their own policy, and a sweep must only ever take its own slots.
	foreignLive := reclaimPolicy{InstanceID: "other-live", StaleAfter: 24 * time.Hour, Now: now}
	foreignStale := reclaimPolicy{InstanceID: "other-stale", StaleAfter: 24 * time.Hour, Now: now.Add(-25 * time.Hour)}

	// Three registered worktrees, none cleaned up (killed runs / live run).
	own := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, mine)
	live := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, foreignLive)
	stale := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, foreignStale)
	for _, r := range []checkoutResult{own, live, stale} {
		if r.BlockReason != "" {
			t.Fatalf("setup: %s", r.BlockReason)
		}
	}
	writeFile(t, filepath.Join(live.Worktree, "in-progress.txt"), "live\n")
	writeFile(t, filepath.Join(own.Worktree, "dirty.txt"), "dirty\n")

	// A markerless leftover directory that is old, and one that is young.
	oldDir := filepath.Join(filepath.Dir(f.clone), qaWorktreePrefix+"old-unmarked")
	youngDir := filepath.Join(filepath.Dir(f.clone), qaWorktreePrefix+"young-unmarked")
	for _, d := range []string{oldDir, youngDir} {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(d, "sub", "junk.txt"), "junk\n")
	}
	oldTime := now.Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	removed := reclaimAbandonedWorktrees(context.Background(), f.clone, mine)

	removedSet := map[string]bool{}
	for _, p := range removed {
		removedSet[filepath.Base(p)] = true
	}
	for _, want := range []string{filepath.Base(own.Worktree), filepath.Base(stale.Worktree), filepath.Base(oldDir)} {
		if !removedSet[want] {
			t.Errorf("expected %s to be reclaimed, removed=%v", want, removed)
		}
	}
	for _, keep := range []string{filepath.Base(live.Worktree), filepath.Base(youngDir)} {
		if removedSet[keep] {
			t.Errorf("%s must not be reclaimed", keep)
		}
	}
	if _, err := os.Stat(filepath.Join(live.Worktree, "in-progress.txt")); err != nil {
		t.Fatalf("foreign live worktree must be intact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(youngDir, "sub", "junk.txt")); err != nil {
		t.Fatalf("young markerless directory must be intact: %v", err)
	}
	for _, gone := range []string{own.Worktree, stale.Worktree, oldDir} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s must be gone (stat err=%v)", gone, err)
		}
	}
	if worktreeRegistered(context.Background(), f.clone, own.Worktree) || worktreeRegistered(context.Background(), f.clone, stale.Worktree) {
		t.Fatalf("reclaimed worktrees must be unregistered")
	}
	if !worktreeRegistered(context.Background(), f.clone, live.Worktree) {
		t.Fatalf("live foreign worktree must stay registered")
	}
	live.Cleanup()
}

// The reclaim sweep runs as part of a checkout: a fresh foreign slot for
// the same work item survives the next attempt, a stale one does not.
func TestCheckoutVerifiedHead_ReclaimRespectsForeignLiveSlot(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)
	now := time.Now()
	// Distinct instance ids: the setup checkouts each run the reclaim sweep
	// under their own policy, and a sweep must only ever take its own slots.
	foreignLive := reclaimPolicy{InstanceID: "other-live", StaleAfter: 24 * time.Hour, Now: now}
	foreignStale := reclaimPolicy{InstanceID: "other-stale", StaleAfter: 24 * time.Hour, Now: now.Add(-25 * time.Hour)}

	live := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, foreignLive)
	stale := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, f.workID, f.branch, verified, evidence.CompatWindowStrict, foreignStale)
	if live.BlockReason != "" || stale.BlockReason != "" {
		t.Fatalf("setup: %s / %s", live.BlockReason, stale.BlockReason)
	}
	defer live.Cleanup()

	res := checkout(f, f.branch, verified, evidence.CompatWindowStrict)
	if res.BlockReason != "" {
		t.Fatalf("unexpected block: %s", res.BlockReason)
	}
	defer res.Cleanup()
	if _, err := os.Stat(live.Worktree); err != nil {
		t.Fatalf("foreign live slot must survive: %v", err)
	}
	if _, err := os.Stat(stale.Worktree); !os.IsNotExist(err) {
		t.Fatalf("stale foreign slot must be reclaimed (stat err=%v)", err)
	}
	if res.Worktree == live.Worktree || res.Worktree == stale.Worktree {
		t.Fatalf("new attempt must get its own slot")
	}
}

func TestDecideReclaim(t *testing.T) {
	now := time.Now()
	policy := reclaimPolicy{InstanceID: "me", StaleAfter: time.Hour, Now: now}
	cases := []struct {
		name   string
		marker worktreeMarker
		err    error
		mtime  time.Time
		remove bool
		why    reclaimDecision
	}{
		{"own instance, fresh", worktreeMarker{InstanceID: "me", CreatedAt: now}, nil, now, true, reclaimOwn},
		{"foreign, fresh", worktreeMarker{InstanceID: "other", CreatedAt: now.Add(-30 * time.Minute)}, nil, now, false, keepForeignLive},
		{"foreign, stale", worktreeMarker{InstanceID: "other", CreatedAt: now.Add(-2 * time.Hour)}, nil, now, true, reclaimStaleMarker},
		{"foreign, no created_at", worktreeMarker{InstanceID: "other"}, nil, now.Add(-3 * time.Hour), false, keepForeignLive},
		{"empty instance id never matches", worktreeMarker{InstanceID: "", CreatedAt: now}, nil, now, false, keepForeignLive},
		{"unmarked, young", worktreeMarker{}, os.ErrNotExist, now.Add(-10 * time.Minute), false, keepUnmarkedYoung},
		{"unmarked, old", worktreeMarker{}, os.ErrNotExist, now.Add(-2 * time.Hour), true, reclaimStaleUnmarked},
		{"unreadable marker, young", worktreeMarker{}, errors.New("parse"), now, false, keepUnreadable},
		{"unreadable marker, old", worktreeMarker{}, errors.New("parse"), now.Add(-2 * time.Hour), true, reclaimStaleUnmarked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remove, why := decideReclaim(policy, tc.marker, tc.err, tc.mtime)
			if remove != tc.remove || why != tc.why {
				t.Fatalf("got (%t, %s), want (%t, %s)", remove, why, tc.remove, tc.why)
			}
		})
	}
	// Policy from env: default and override.
	os.Unsetenv(envWorktreeStaleAfter)
	if got := worktreeStaleAfterFromEnv(); got != defaultWorktreeStaleAfter {
		t.Fatalf("default stale-after = %s", got)
	}
	t.Setenv(envWorktreeStaleAfter, "90m")
	if got := worktreeStaleAfterFromEnv(); got != 90*time.Minute {
		t.Fatalf("stale-after from env = %s", got)
	}
	t.Setenv(envWorktreeStaleAfter, "bogus")
	if got := worktreeStaleAfterFromEnv(); got != defaultWorktreeStaleAfter {
		t.Fatalf("unparseable stale-after must fall back, got %s", got)
	}
}

// The remover must never target the primary clone or anything outside the
// qa-worktree-* sibling slot.
func TestIsSafeToRemove(t *testing.T) {
	f := newRepoFixture(t)
	parent := filepath.Dir(f.clone)
	slot := filepath.Join(parent, qaWorktreePrefix+f.workID.String()+"-deadbeef")
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"own worktree slot", slot, true},
		{"primary clone", f.clone, false},
		{"clone via dot path", filepath.Join(f.clone, ".", "..", "clone"), false},
		{"sibling without prefix", filepath.Join(parent, "other"), false},
		{"prefix but nested elsewhere", filepath.Join(parent, "deeper", qaWorktreePrefix+"x"), false},
		{"pusher clone (a real primary checkout)", f.pusher, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := isSafeToRemove(tc.path, f.clone)
			if tc.ok && err != nil {
				t.Fatalf("expected safe, got %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("expected refusal for %s", tc.path)
				}
				if !errors.Is(err, errUnsafeWorktree) {
					t.Fatalf("refusal must wrap errUnsafeWorktree, got %v", err)
				}
			}
		})
	}
	// A directory in the slot whose .git is a directory is a primary
	// checkout that happens to be named like our slot: refuse.
	if err := os.MkdirAll(filepath.Join(slot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := isSafeToRemove(slot, f.clone); err == nil || !errors.Is(err, errUnsafeWorktree) {
		t.Fatalf("a .git directory in the slot must be refused, got %v", err)
	}
	if err := removeWorktree(context.Background(), f.clone, f.clone); err == nil {
		t.Fatalf("removeWorktree must refuse the primary clone")
	}
	if _, err := os.Stat(filepath.Join(f.clone, ".git")); err != nil {
		t.Fatalf("primary clone must be untouched: %v", err)
	}
}

func TestCheckoutVerifiedHead_LegacyTipWithWarning(t *testing.T) {
	f := newRepoFixture(t)
	tip := f.head(t)

	res := checkout(f, f.branch, "", evidence.CompatWindowLegacy)
	if res.BlockReason != "" {
		t.Fatalf("unexpected block: %s", res.BlockReason)
	}
	if res.Cleanup != nil {
		defer res.Cleanup()
	}
	if !res.Legacy || res.Source != sourceLegacyTip {
		t.Fatalf("legacy fallback must be flagged: %+v", res)
	}
	if res.SHA != tip || worktreeHead(t, res.Worktree) != tip {
		t.Fatalf("legacy QA must test the tip %s, got %+v", tip, res)
	}
}

func TestCheckoutVerifiedHead_LegacyFindsAgentBranch(t *testing.T) {
	f := newRepoFixture(t)
	tip := f.head(t)

	// No branch on the message: the pre-D1.5 agent/<work-id>/* search.
	res := checkout(f, "", "", evidence.CompatWindowLegacy)
	if res.BlockReason != "" {
		t.Fatalf("unexpected block: %s", res.BlockReason)
	}
	if res.Cleanup != nil {
		defer res.Cleanup()
	}
	if res.Branch != f.branch || res.SHA != tip {
		t.Fatalf("expected branch %s at %s, got %+v", f.branch, tip, res)
	}
}

func TestCheckoutVerifiedHead_StrictWithoutSHABlocks(t *testing.T) {
	f := newRepoFixture(t)

	for _, sha := range []string{"", "   \n"} {
		res := checkout(f, f.branch, sha, evidence.CompatWindowStrict)
		if res.Cleanup != nil {
			res.Cleanup()
		}
		if res.BlockReason == "" || !strings.Contains(res.BlockReason, evidence.CompatWindowEnv+"=strict") {
			t.Fatalf("strict window without head_sha (%q) must block, got %+v", sha, res)
		}
		if res.Worktree != "" {
			t.Fatalf("blocked checkout must not hand back a worktree")
		}
	}
}

// A malformed head_sha is never handed to git: strict blocks naming the
// problem, legacy treats it as absent and tests the tip.
func TestCheckoutVerifiedHead_MalformedSHA(t *testing.T) {
	f := newRepoFixture(t)
	tip := f.head(t)
	for _, bad := range []string{"--upload-pack=evil", "HEAD~1", "main", "abc"} {
		res := checkout(f, f.branch, bad, evidence.CompatWindowStrict)
		if res.Cleanup != nil {
			res.Cleanup()
		}
		if res.BlockReason == "" || !strings.Contains(res.BlockReason, "malformed head_sha") || res.Worktree != "" {
			t.Fatalf("strict: malformed %q must block, got %+v", bad, res)
		}

		res = checkout(f, f.branch, bad, evidence.CompatWindowLegacy)
		if res.BlockReason != "" {
			t.Fatalf("legacy: malformed %q must fall back to the tip, got block %s", bad, res.BlockReason)
		}
		if !res.Legacy || res.SHA != tip || res.Source != sourceLegacyTip {
			t.Fatalf("legacy: malformed %q must test the tip, got %+v", bad, res)
		}
		res.Cleanup()
	}
}

func TestCheckoutVerifiedHead_SHAWithoutBranchBlocks(t *testing.T) {
	f := newRepoFixture(t)
	verified := f.head(t)
	other := uuid.New() // no agent/<other>/* branch exists

	res := checkoutVerifiedHeadWithPolicy(context.Background(), f.clone, other, "", verified, evidence.CompatWindowLegacy, testPolicy)
	if res.Cleanup != nil {
		res.Cleanup()
	}
	if res.BlockReason == "" || !strings.Contains(res.BlockReason, verified) {
		t.Fatalf("a verified SHA with no branch to check it against must block, got %+v", res)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = gitOut(t, dir, args...)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
