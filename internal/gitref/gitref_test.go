package gitref

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
)

func TestSanitizeAndValid(t *testing.T) {
	cases := []struct {
		raw     string
		wantSHA string
		wantOK  bool
	}{
		{"", "", true},
		{"  \n", "", true},
		{shaA, shaA, true},
		{" " + strings.ToUpper(shaA) + "\n", shaA, true},
		{"abcdef1", "abcdef1", true},                               // 7 chars: shortest abbreviation
		{"abcdef", "", false},                                      // too short
		{strings.Repeat("a", 65), "", false},                       // too long
		{"--upload-pack=evil", "", false},                          // option injection
		{"main", "", false},                                        // a ref name, not a SHA
		{"HEAD~1", "", false},                                      // a revision expression
		{"1111111111111111111111111111111111111111 --", "", false}, // trailing junk
		{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", true},
	}
	for _, tc := range cases {
		got, ok := Sanitize(tc.raw)
		if got != tc.wantSHA || ok != tc.wantOK {
			t.Errorf("Sanitize(%q) = (%q, %t), want (%q, %t)", tc.raw, got, ok, tc.wantSHA, tc.wantOK)
		}
		if v := Valid(tc.raw); v != (tc.wantOK && tc.wantSHA != "") {
			t.Errorf("Valid(%q) = %t", tc.raw, v)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	tests := []struct {
		name       string
		head       string
		tip        string
		window     evidence.CompatWindow
		wantSHA    string
		blocked    bool
		legacy     bool
		branchGone bool
		reason     string // substring
	}{
		{
			name: "sha present and equals tip", head: shaA, tip: shaA, window: evidence.CompatWindowLegacy,
			wantSHA: shaA,
		},
		{
			name: "sha present and equals tip (strict)", head: shaA, tip: shaA, window: evidence.CompatWindowStrict,
			wantSHA: shaA,
		},
		{
			name: "sha present, case and whitespace differ", head: " " + strings.ToUpper(shaA) + "\n", tip: shaA, window: evidence.CompatWindowStrict,
			wantSHA: shaA,
		},
		{
			name: "sha present and differs → block", head: shaA, tip: shaB, window: evidence.CompatWindowLegacy,
			blocked: true, reason: "branch moved after verification: expected " + shaA + ", found " + shaB,
		},
		{
			name: "sha present and differs → block even in strict", head: shaA, tip: shaB, window: evidence.CompatWindowStrict,
			blocked: true, reason: "branch moved after verification: expected " + shaA + ", found " + shaB,
		},
		{
			name: "sha present, branch gone from remote → block, flagged", head: shaA, tip: "", window: evidence.CompatWindowLegacy,
			blocked: true, branchGone: true, reason: "branch moved after verification: expected " + shaA + ", found (branch not on remote)",
		},
		{
			name: "sha absent, legacy → tip with warning", head: "", tip: shaB, window: evidence.CompatWindowLegacy,
			wantSHA: shaB, legacy: true, reason: "no verified head_sha",
		},
		{
			name: "sha whitespace-only counts as absent", head: "  \n", tip: shaB, window: evidence.CompatWindowLegacy,
			wantSHA: shaB, legacy: true, reason: "no verified head_sha",
		},
		{
			name: "sha absent, legacy, tip unknown → empty target, caller uses ref", head: "", tip: "", window: evidence.CompatWindowLegacy,
			wantSHA: "", legacy: true, reason: "no verified head_sha",
		},
		{
			name: "sha absent, strict → block", head: "", tip: shaB, window: evidence.CompatWindowStrict,
			blocked: true, reason: evidence.CompatWindowEnv + "=strict",
		},
		{
			name: "sha malformed, strict → block, never compared", head: "--upload-pack=evil", tip: shaB, window: evidence.CompatWindowStrict,
			blocked: true, reason: "malformed head_sha",
		},
		{
			name: "sha malformed, legacy → treated as absent, tip with warning", head: "not-a-sha", tip: shaB, window: evidence.CompatWindowLegacy,
			wantSHA: shaB, legacy: true, reason: "malformed head_sha",
		},
		{
			name: "malformed tip is ignored, sha present → branch gone", head: shaA, tip: "junk", window: evidence.CompatWindowStrict,
			blocked: true, branchGone: true, reason: "found (branch not on remote)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveTarget("qa.ready", tc.head, tc.tip, tc.window)
			if got.Blocked != tc.blocked {
				t.Fatalf("Blocked = %t, want %t (%+v)", got.Blocked, tc.blocked, got)
			}
			if got.Legacy != tc.legacy {
				t.Errorf("Legacy = %t, want %t", got.Legacy, tc.legacy)
			}
			if got.BranchGone != tc.branchGone {
				t.Errorf("BranchGone = %t, want %t", got.BranchGone, tc.branchGone)
			}
			if got.SHA != tc.wantSHA {
				t.Errorf("SHA = %q, want %q", got.SHA, tc.wantSHA)
			}
			if got.SHA != "" && !Valid(got.SHA) {
				t.Errorf("a returned SHA must be well-formed, got %q", got.SHA)
			}
			if tc.blocked && got.SHA != "" {
				t.Errorf("a blocked resolution must not name a SHA, got %q", got.SHA)
			}
			if tc.reason == "" {
				if got.Reason != "" {
					t.Errorf("Reason = %q, want empty", got.Reason)
				}
			} else if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
			if (tc.blocked || tc.legacy) && got.Reason == "" {
				t.Errorf("blocked/legacy resolutions must carry an operator-readable reason")
			}
			if tc.legacy && !strings.Contains(got.Reason, "qa.ready") {
				t.Errorf("reason must name the topic: %q", got.Reason)
			}
		})
	}
}

// TestGitHelpers_RealRepo exercises RemoteTip, CommitPresent and
// ReachableFromRemote against a bare origin and a clone of it.
func TestGitHelpers_RealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	pusher := filepath.Join(root, "pusher")

	git(t, root, "init", "--bare", "-b", "main", origin)
	git(t, root, "clone", "-q", origin, pusher)
	writeFile(t, filepath.Join(pusher, "a.txt"), "one\n")
	git(t, pusher, "add", ".")
	git(t, pusher, "commit", "-q", "-m", "one")
	git(t, pusher, "push", "-q", "origin", "HEAD:main")

	// The service's clone: taken before the feature branch exists, so it
	// has neither the remote-tracking ref nor the commit until it fetches.
	git(t, root, "clone", "-q", origin, clone)

	git(t, pusher, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(pusher, "b.txt"), "two\n")
	git(t, pusher, "add", ".")
	git(t, pusher, "commit", "-q", "-m", "two")
	git(t, pusher, "push", "-q", "origin", "feature")
	verified := strings.TrimSpace(gitOut(t, pusher, "rev-parse", "HEAD"))

	tip, err := RemoteTip(ctx, clone, "feature")
	if err != nil {
		t.Fatalf("RemoteTip: %v", err)
	}
	if tip != verified {
		t.Fatalf("remote tip = %q, want %q", tip, verified)
	}
	if CommitPresent(ctx, clone, verified) {
		t.Fatalf("verified commit must not be present before fetch")
	}
	if ok, _ := ReachableFromRemote(ctx, clone, verified); ok {
		t.Fatalf("unknown commit must not be reachable")
	}
	git(t, clone, "fetch", "-q", "--prune", "origin")
	if !CommitPresent(ctx, clone, verified) {
		t.Fatalf("verified commit must be present after fetch")
	}
	if CommitPresent(ctx, clone, "HEAD") || CommitPresent(ctx, clone, "origin/main") {
		t.Fatalf("CommitPresent must refuse non-SHA revisions")
	}
	if ok, refs := ReachableFromRemote(ctx, clone, verified); !ok || len(refs) != 1 || refs[0] != "origin/feature" {
		t.Fatalf("verified commit must be reachable from origin/feature, got ok=%t refs=%v", ok, refs)
	}
	if got := ResolveTarget("pr.ready", verified, tip, evidence.CompatWindowStrict); got.Blocked || got.SHA != verified {
		t.Fatalf("unexpected resolution %+v", got)
	}

	// Someone pushes to the branch after verification: the remote tip
	// moves and the resolution must block naming both SHAs.
	writeFile(t, filepath.Join(pusher, "c.txt"), "three\n")
	git(t, pusher, "add", ".")
	git(t, pusher, "commit", "-q", "-m", "three")
	git(t, pusher, "push", "-q", "origin", "feature")
	moved := strings.TrimSpace(gitOut(t, pusher, "rev-parse", "HEAD"))

	tip, err = RemoteTip(ctx, clone, "feature")
	if err != nil {
		t.Fatalf("RemoteTip after move: %v", err)
	}
	if tip != moved {
		t.Fatalf("remote tip after move = %q, want %q", tip, moved)
	}
	got := ResolveTarget("pr.ready", verified, tip, evidence.CompatWindowLegacy)
	if !got.Blocked || got.BranchGone {
		t.Fatalf("moved branch must block (not branch-gone), got %+v", got)
	}
	if want := BranchMovedReason(verified, moved); got.Reason != want {
		t.Fatalf("Reason = %q, want %q", got.Reason, want)
	}

	// A second remote that still carries the branch must not count:
	// reachability is about origin only.
	git(t, clone, "remote", "add", "mirror", origin)
	git(t, clone, "fetch", "-q", "mirror")
	if ok, refs := ReachableFromRemote(ctx, clone, verified); !ok || strings.Join(refs, ",") != "origin/feature" {
		t.Fatalf("only origin refs may be reported, got ok=%t refs=%v", ok, refs)
	}

	// Delete the branch on origin without merging: the commit is still in
	// the clone's object store from the earlier fetch, and mirror/feature
	// still contains it, but after a prune no origin/* ref does.
	git(t, pusher, "push", "-q", "origin", "--delete", "feature")
	tip, err = RemoteTip(ctx, clone, "feature")
	if err != nil || tip != "" {
		t.Fatalf("deleted branch: tip=%q err=%v", tip, err)
	}
	got = ResolveTarget("pr.ready", verified, tip, evidence.CompatWindowLegacy)
	if !got.Blocked || !got.BranchGone {
		t.Fatalf("deleted branch must block with BranchGone, got %+v", got)
	}
	git(t, clone, "fetch", "-q", "--prune", "origin")
	if !CommitPresent(ctx, clone, verified) {
		t.Fatalf("commit should still be in the local object store")
	}
	if out := gitOut(t, clone, "branch", "-r", "--contains", verified); !strings.Contains(out, "mirror/feature") {
		t.Fatalf("precondition: mirror/feature should still contain the commit, got %q", out)
	}
	if ok, refs := ReachableFromRemote(ctx, clone, verified); ok {
		t.Fatalf("commit reachable only via a non-origin remote must not count, got %v", refs)
	}

	// Merge the verified commit into main and push: it is now reachable
	// through origin/main even though its branch is gone.
	git(t, pusher, "checkout", "-q", "main")
	git(t, pusher, "merge", "-q", "--no-ff", "-m", "merge feature", verified)
	git(t, pusher, "push", "-q", "origin", "main")
	git(t, clone, "fetch", "-q", "--prune", "origin")
	if ok, refs := ReachableFromRemote(ctx, clone, verified); !ok || len(refs) != 1 || refs[0] != "origin/main" {
		t.Fatalf("merged commit must be reachable from origin/main only, got ok=%t refs=%v", ok, refs)
	}
	if CommitPresent(ctx, clone, shaA) {
		t.Fatalf("CommitPresent must be false for an unknown SHA")
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
