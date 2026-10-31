package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// symlinkOrSkip creates a symlink or skips the test where the platform
// refuses (Windows without developer mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// depsWorktree builds a clone whose .gitignore has "node_modules/" (which
// matches directories only, not a symlink), an installed node_modules in
// the clone, and a linked worktree of it.
func depsWorktree(t *testing.T) (clone, wt string) {
	t.Helper()
	clone = initTestRepository(t)
	writeFile(t, clone, ".gitignore", "node_modules/\n")
	gitRun(t, clone, "add", ".gitignore")
	gitRun(t, clone, "commit", "-m", "ignore node_modules")
	if err := os.MkdirAll(filepath.Join(clone, "node_modules", "left-pad"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(clone, "node_modules", "left-pad"), "index.js", "module.exports = 1;\n")
	wt = filepath.Join(t.TempDir(), "worktree-task")
	gitRun(t, clone, "worktree", "add", "-b", "agent/w/t", wt, "main")
	return clone, wt
}

// TestSymlinkDepsLinkIsNeverCommitted is a live run (attempt 6): symlinkDeps
// linked node_modules into the worktree, "node_modules/" did not ignore
// the link, and `git add -A` committed a mode-120000 node_modules pointing
// at the clone's absolute path. The link is now excluded via the
// repository's info/exclude, so the agent's stage-and-commit leaves it out.
func TestSymlinkDepsLinkIsNeverCommitted(t *testing.T) {
	clone, wt := depsWorktree(t)
	probe := filepath.Join(t.TempDir(), "probe")
	symlinkOrSkip(t, clone, probe)

	linked := symlinkDeps(clone, wt)
	if len(linked) != 1 || linked[0] != "node_modules" {
		t.Fatalf("symlinkDeps linked %v, want [node_modules]", linked)
	}
	if fi, err := os.Lstat(filepath.Join(wt, "node_modules")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("node_modules in the worktree is not a symlink: %v %v", fi, err)
	}
	writeFile(t, wt, "app.js", "require('left-pad');\n")

	if err := stageAll(context.Background(), wt); err != nil {
		t.Fatalf("stageAll: %v", err)
	}
	gitRun(t, wt, "commit", "-m", "feat: app")
	tree := gitOutput(t, wt, "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tree, "node_modules") {
		t.Fatalf("commit contains the node_modules symlink:\n%s", tree)
	}
	if !strings.Contains(tree, "app.js") {
		t.Fatalf("commit lost the real change:\n%s", tree)
	}
	// The clone's own status is unaffected by the exclude entry.
	if st := gitOutput(t, clone, "status", "--porcelain"); strings.Contains(st, "node_modules") {
		t.Fatalf("clone status shows node_modules: %q", st)
	}
}

// TestStageAllRefusesSymlinkOutsideWorktree is the commit guard: whatever
// the ignore rules say, a staged symlink whose target leaves the worktree
// (absolute, or relative through ..) is refused with an error naming it,
// and so is a dependency-directory name staged as a symlink. A symlink
// inside the worktree is ordinary content and is allowed.
func TestStageAllRefusesSymlinkOutsideWorktree(t *testing.T) {
	cases := []struct {
		name   string
		link   string
		target func(wt, outside string) string
		refuse bool
	}{
		{"absolute target outside", "cache", func(_, outside string) string { return outside }, true},
		{"relative target escaping", "up", func(_, _ string) string { return "../elsewhere" }, true},
		{"dependency name inside", "vendor", func(_, _ string) string { return "lib" }, true},
		{"relative target inside", "readme-link", func(_, _ string) string { return "README.md" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wt := initTestRepository(t)
			outside := t.TempDir()
			if err := os.MkdirAll(filepath.Join(wt, "lib"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(wt, "lib"), "a.txt", "a\n")
			symlinkOrSkip(t, tc.target(wt, outside), filepath.Join(wt, tc.link))

			if tc.link == "vendor" {
				// R6 M6: a linked dependency name is left out by pathspec
				// (not staged, so nothing to refuse); the guard still
				// refuses one staged by other means.
				if err := stageAll(context.Background(), wt); err != nil {
					t.Fatalf("stageAll: %v", err)
				}
				if staged := gitOutput(t, wt, "diff", "--cached", "--name-only"); strings.Contains(staged, "vendor") {
					t.Fatalf("the vendor link was staged: %s", staged)
				}
				gitRun(t, wt, "add", "-A")
			}
			var err error
			if tc.link == "vendor" {
				err = guardStagedSymlinks(context.Background(), wt)
			} else {
				err = stageAll(context.Background(), wt)
			}
			if !tc.refuse {
				if err != nil {
					t.Fatalf("stageAll refused an in-worktree symlink: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("stageAll accepted symlink %s", tc.link)
			}
			if !strings.Contains(err.Error(), tc.link) {
				t.Fatalf("error %q does not name %s", err, tc.link)
			}
		})
	}
}
