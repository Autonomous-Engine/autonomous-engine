package verifier

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ScratchPrefix names the verifier's scratch directories (under the OS temp
// dir, never inside the clone).
const ScratchPrefix = "ae-verifier-"

// Scratch is a throwaway repository at the head commit the verifier works
// in: `git clone --shared --no-checkout <clone>` into a fresh temp
// directory, origin removed, head checked out detached. It has its own
// refs, config, index and object store (the clone's objects are reachable
// read-only through objects/info/alternates), so git commands run in it —
// branch, update-ref, config, commit, gc — cannot change the clone.
// Whatever the verifier writes is discarded with it.
type Scratch struct {
	Clone string
	Path  string
	Head  string
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveCommit resolves ref (a SHA or e.g. origin/<branch>) to a commit id
// in clone.
func ResolveCommit(ctx context.Context, clone, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("unusable ref %q", ref)
	}
	return git(ctx, clone, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

// NewScratch materialises a Scratch of clone at head (resolved in the
// clone, so origin/<branch> refs work). The caller must Close it.
func NewScratch(ctx context.Context, clone, head string) (*Scratch, error) {
	sha, err := ResolveCommit(ctx, clone, head)
	if err != nil {
		return nil, err
	}
	parent, err := os.MkdirTemp("", ScratchPrefix+shortSHA(sha)+"-")
	if err != nil {
		return nil, fmt.Errorf("create scratch dir: %w", err)
	}
	s := &Scratch{Clone: clone, Path: filepath.Join(parent, "tree"), Head: sha}
	fail := func(err error) (*Scratch, error) {
		_ = os.RemoveAll(parent)
		return nil, err
	}
	src, err := filepath.Abs(clone)
	if err != nil {
		return fail(err)
	}
	if _, err := git(ctx, parent, "clone", "--quiet", "--shared", "--no-checkout", "--", src, s.Path); err != nil {
		return fail(err)
	}
	// No path back to the clone: no remote to push to or fetch from.
	if _, err := git(ctx, s.Path, "remote", "remove", "origin"); err != nil {
		return fail(err)
	}
	if _, err := git(ctx, s.Path, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", sha); err != nil {
		return fail(err)
	}
	got, err := git(ctx, s.Path, "rev-parse", "HEAD")
	if err != nil || got != sha {
		if err == nil {
			err = fmt.Errorf("scratch HEAD is %s, want %s", got, sha)
		}
		return fail(err)
	}
	return s, nil
}

// Close deletes the scratch repository. It never touches the clone.
func (s *Scratch) Close(_ context.Context) error {
	if s == nil || s.Path == "" {
		return nil
	}
	parent := filepath.Dir(s.Path)
	if !strings.HasPrefix(filepath.Base(parent), ScratchPrefix) {
		return fmt.Errorf("refusing to remove %s: not a verifier scratch directory", parent)
	}
	// Files a command left read-only (Go's module cache, a chmod) would
	// make RemoveAll fail: make everything writable first.
	_ = filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(parent)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// DiffOf returns the git-derived changed files and the patch of
// base...head in dir, the patch truncated to MaxPatchBytes (lockfiles
// excluded, as git_diff does).
func DiffOf(ctx context.Context, dir, base, head string) (files []string, patch string, truncated bool, err error) {
	if strings.HasPrefix(base, "-") || strings.HasPrefix(head, "-") {
		return nil, "", false, fmt.Errorf("unusable diff range %q...%q", base, head)
	}
	rng := base + "..." + head
	names, err := git(ctx, dir, "diff", "--name-only", "--end-of-options", rng)
	if err != nil {
		return nil, "", false, err
	}
	for _, l := range strings.Split(names, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	patch, err = git(ctx, dir, "diff", "--end-of-options", rng, "--", ".",
		":(exclude)package-lock.json", ":(exclude)yarn.lock", ":(exclude)pnpm-lock.yaml")
	if err != nil {
		return files, "", false, err
	}
	if len(patch) > MaxPatchBytes {
		patch, truncated = patch[:MaxPatchBytes], true
	}
	return files, patch, truncated, nil
}
