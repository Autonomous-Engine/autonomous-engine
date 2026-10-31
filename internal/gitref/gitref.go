// Package gitref resolves which commit a downstream consumer (review-agent,
// qa-agent) examines for a work item, so that every stage examines the same verified head: the
// verified head SHA from the implementation evidence, never the branch tip
// by name. It is shared so the two services cannot drift in how they read
// the head_sha field, compare it with the remote, or apply the
// EVIDENCE_COMPAT_WINDOW rules for messages without one.
package gitref

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// Resolution is the outcome of ResolveTarget: which commit to examine, or
// why the consumer must not proceed.
type Resolution struct {
	// SHA is the commit to examine. Empty when Blocked, and empty for a
	// legacy message whose remote tip could not be resolved (the caller
	// then falls back to the origin/<branch> ref as it did before D1.5).
	SHA string
	// Blocked is true when the work item must be blocked with Reason.
	Blocked bool
	// Reason is the block reason when Blocked, the warning when Legacy,
	// and empty otherwise. Always operator-readable.
	Reason string
	// Legacy is true when the message carried no (usable) head_sha and the
	// tip was accepted under the legacy window.
	Legacy bool
	// BranchGone is true when a head_sha was given but the branch no
	// longer exists on the remote. The resolution is Blocked (fail closed);
	// a caller that can establish the verified SHA is still reachable from
	// the remote by another ref (a merge commit on the base branch, D1.5
	// "remotely resolvable") may proceed on that SHA instead.
	BranchGone bool
}

// shaPattern is what a normalised head_sha must look like before it is
// compared with anything or handed to git: 7–64 lower-case hex characters
// (abbreviated through SHA-256 object names). Anything else is treated as
// absent so a malformed field can never reach a git command line.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Normalise trims and lower-cases a SHA so whitespace-only values read as
// absent and hex case never causes a false mismatch.
func Normalise(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Valid reports whether s (after Normalise) is a well-formed object name.
func Valid(s string) bool {
	return shaPattern.MatchString(Normalise(s))
}

// Sanitize normalises raw and returns it when well-formed. When raw is
// non-empty but malformed it returns "" with ok=false so the caller can log
// that a head_sha was ignored; an empty raw is "" with ok=true (absent, not
// malformed). The returned value is safe to pass to git.
func Sanitize(raw string) (sha string, ok bool) {
	n := Normalise(raw)
	if n == "" {
		return "", true
	}
	if !shaPattern.MatchString(n) {
		return "", false
	}
	return n, true
}

// ResolveTarget is the pure D1.5 rule. headSHA is the verified head SHA
// from the message (empty for a legacy producer); tipSHA is the current tip
// of the branch on the remote (empty when the branch is not on the remote
// or the tip could not be read). topic names the message in reasons.
//
//	head present, tip == head → examine head.
//	head present, tip != head → block: branch moved after verification.
//	head present, no tip      → block, BranchGone (see Resolution).
//	head absent, legacy       → examine tip with a warning.
//	head absent, strict       → block.
//
// A malformed head (see Valid) is treated as absent — block in strict, tip
// with a warning in legacy — and the reason says so; it is never compared
// or returned.
func ResolveTarget(topic, headSHA, tipSHA string, window evidence.CompatWindow) Resolution {
	head, headOK := Sanitize(headSHA)
	tip, tipOK := Sanitize(tipSHA)
	if !tipOK {
		tip = ""
	}

	if head == "" {
		detail := "carries no verified head_sha"
		if !headOK {
			detail = "carries a malformed head_sha (ignored)"
		}
		if window == evidence.CompatWindowStrict {
			return Resolution{
				Blocked: true,
				Reason:  topic + " " + detail + " (" + evidence.CompatWindowEnv + "=strict); the implementation evidence must name the SHA to examine",
			}
		}
		return Resolution{
			SHA:    tip,
			Legacy: true,
			Reason: topic + " " + detail + " — using the branch tip under the legacy window (" + evidence.CompatWindowEnv + "=legacy)",
		}
	}

	if tip == "" {
		return Resolution{
			Blocked:    true,
			BranchGone: true,
			Reason:     BranchMovedReason(head, ""),
		}
	}
	if tip != head {
		return Resolution{
			Blocked: true,
			Reason:  BranchMovedReason(head, tip),
		}
	}
	return Resolution{SHA: head}
}

// BranchMovedReason is the operator-facing block reason for a branch whose
// remote tip is no longer the verified head. An empty found means the
// branch is not on the remote at all.
func BranchMovedReason(expected, found string) string {
	if found == "" {
		found = "(branch not on remote)"
	}
	return fmt.Sprintf("branch moved after verification: expected %s, found %s", expected, found)
}

// RemoteTip returns the SHA that refs/heads/<branch> points at on origin,
// asked of the remote directly (git ls-remote) so a stale local
// remote-tracking ref cannot mask a moved branch. It returns "" with a nil
// error when the branch does not exist on the remote or branch is empty.
func RemoteTip(ctx context.Context, dir, branch string) (string, error) {
	if branch == "" {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git ls-remote origin %s: %v: %s", branch, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "refs/heads/"+branch {
			return Normalise(fields[0]), nil
		}
	}
	return "", nil
}

// CommitPresent reports whether sha names a commit object in the local
// repository at dir (i.e. a preceding fetch brought it in). A malformed sha
// is never passed to git and reads as absent.
func CommitPresent(ctx context.Context, dir, sha string) bool {
	sha, ok := Sanitize(sha)
	if !ok || sha == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	cmd.Dir = dir
	return cmd.Run() == nil
}

// ReachableFromRemote reports whether sha is an ancestor of (or equal to)
// at least one origin/* remote-tracking ref in dir, i.e. still remotely
// resolvable on origin after a fetch --prune. Refs of any other remote are
// ignored. A commit that only survives in the local object store because
// it was fetched before its branch was deleted is not reachable. It returns
// the containing refs for logging.
func ReachableFromRemote(ctx context.Context, dir, sha string) (bool, []string) {
	sha, ok := Sanitize(sha)
	if !ok || sha == "" || !CommitPresent(ctx, dir, sha) {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "branch", "-r", "--contains", sha, "--list", "origin/*")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, nil
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.Contains(ref, " -> ") { // skip origin/HEAD -> origin/main
			continue
		}
		if !strings.HasPrefix(ref, "origin/") {
			continue // belt and braces: --list already restricts
		}
		refs = append(refs, ref)
	}
	return len(refs) > 0, refs
}
