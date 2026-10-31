package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M1: when the commit guard refused a symlink during a continuation, the
// work was left staged and uncommitted while runGeneration returned ok, so
// finalize pushed HEAD without it. commitContinuation unstages only the
// refused paths and commits the rest.
func TestContinuationCommitsAroundARefusedSymlink(t *testing.T) {
	wt := initTestRepository(t)
	outside := t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(wt, "cache"))
	writeFile(t, wt, "landing.html", "<p>TILER_API_KEY required</p>\n")

	refused, err := commitContinuation(context.Background(), wt, "feat: x (cont 1)")
	if err != nil {
		t.Fatalf("commitContinuation: %v", err)
	}
	if len(refused) != 1 || refused[0] != "cache" {
		t.Fatalf("refused = %v, want [cache]", refused)
	}
	tree := gitOutput(t, wt, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(tree, "landing.html") || strings.Contains(tree, "cache") {
		t.Fatalf("commit tree:\n%s", tree)
	}
	if staged := gitOutput(t, wt, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("still staged: %q", staged)
	}
	if _, err := os.Lstat(filepath.Join(wt, "cache")); err != nil {
		t.Fatalf("the refused link was removed from disk: %v", err)
	}
}

// R6 review: refused paths are unstaged literally — a link named "[c]at"
// must not unstage the file "cat" through glob matching.
func TestContinuationUnstagesRefusedPathsLiterally(t *testing.T) {
	wt := initTestRepository(t)
	symlinkOrSkip(t, t.TempDir(), filepath.Join(wt, "[c]at"))
	writeFile(t, wt, "cat", "meow\n")
	refused, err := commitContinuation(context.Background(), wt, "feat: x (cont 1)")
	if err != nil {
		t.Fatalf("commitContinuation: %v", err)
	}
	if len(refused) != 1 || refused[0] != "[c]at" {
		t.Fatalf("refused = %v", refused)
	}
	tree := gitOutput(t, wt, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(tree, "cat\n") && !strings.HasSuffix(tree, "cat") || strings.Contains(tree, "[c]at") {
		t.Fatalf("commit tree:\n%s", tree)
	}
	// The exclude entry written for the link does not hide "cat" either.
	writeFile(t, wt, "cat", "purr\n")
	if st := gitOutput(t, wt, "status", "--porcelain"); !strings.Contains(st, "cat") || strings.Contains(st, "[c]at") {
		t.Fatalf("status after the exclude entry: %q", st)
	}
}

// When nothing but refused paths remains, it fails: nothing is silently
// dropped and nothing is left staged.
func TestContinuationWithOnlyRefusedPathsFails(t *testing.T) {
	wt := initTestRepository(t)
	head := gitOutput(t, wt, "rev-parse", "HEAD")
	symlinkOrSkip(t, t.TempDir(), filepath.Join(wt, "cache"))
	refused, err := commitContinuation(context.Background(), wt, "feat: x (cont 1)")
	if err == nil {
		t.Fatal("commitContinuation succeeded with nothing to commit")
	}
	if len(refused) != 1 || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("refused = %v, err = %v", refused, err)
	}
	if got := gitOutput(t, wt, "rev-parse", "HEAD"); got != head {
		t.Fatal("HEAD moved")
	}
	if staged := gitOutput(t, wt, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("still staged: %q", staged)
	}
}

// M2: a worktree made before R5 already has the node_modules link but no
// exclude entry; symlinkDeps now writes the entry for a link it finds
// pointing at the clone's directory.
func TestSymlinkDepsExcludesAnExistingLink(t *testing.T) {
	clone, wt := depsWorktree(t)
	symlinkOrSkip(t, filepath.Join(clone, "node_modules"), filepath.Join(wt, "node_modules"))
	if st := gitOutput(t, wt, "status", "--porcelain"); !strings.Contains(st, "node_modules") {
		t.Fatalf("precondition: the pre-R5 link is not visible to git: %q", st)
	}
	if linked := symlinkDeps(clone, wt); len(linked) != 0 {
		t.Fatalf("symlinkDeps re-linked %v", linked)
	}
	if st := gitOutput(t, wt, "status", "--porcelain"); strings.Contains(st, "node_modules") {
		t.Fatalf("the existing link is still visible to git: %q", st)
	}
}

// M5: generation stages through the guard before the tests run, so a
// refused link fails before a test run is paid for.
func TestGenerationStagesThroughTheGuardBeforeTests(t *testing.T) {
	src, err := os.ReadFile("generation.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if strings.Contains(s, `"git", "add", "-A"`) {
		t.Error(`generation.go still runs "git add -A" outside the guard`)
	}
	pre := s[strings.Index(s, "if hasChanges {"):strings.Index(s, "rr := runRepairLoop(ctx, rin)")]
	if !strings.Contains(pre, "stageAll(ctx, worktreePath)") || !strings.Contains(pre, "return generationResult{}, false") {
		t.Errorf("the pre-test staging does not go through stageAll and fail on a refusal:\n%s", pre)
	}
}

// M6: stageAll leaves linked dependency directories out by pathspec, not
// only through the shared info/exclude: a link with no exclude entry is
// not staged (and so not refused), while a real directory of the same kind
// of name (a committed vendor/) is staged as usual.
func TestStageAllExcludesLinkedDependencyDirsByPathspec(t *testing.T) {
	clone, wt := depsWorktree(t)
	symlinkOrSkip(t, filepath.Join(clone, "node_modules"), filepath.Join(wt, "node_modules")) // no exclude entry
	if err := os.MkdirAll(filepath.Join(wt, "vendor", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "vendor", "lib"), "lib.php", "<?php\n")
	writeFile(t, wt, "app.js", "require('left-pad');\n")

	if err := stageAll(context.Background(), wt); err != nil {
		t.Fatalf("stageAll: %v", err)
	}
	staged := gitOutput(t, wt, "diff", "--cached", "--name-only")
	if strings.Contains(staged, "node_modules") {
		t.Fatalf("the linked node_modules was staged:\n%s", staged)
	}
	for _, want := range []string{"app.js", "vendor/lib/lib.php"} {
		if !strings.Contains(staged, want) {
			t.Fatalf("%s not staged:\n%s", want, staged)
		}
	}
}

// The continuation path uses commitContinuation and reports a failure
// instead of breaking out with the work staged.
func TestContinuationNeverDropsWorkSilently(t *testing.T) {
	src, err := os.ReadFile("generation.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	i := strings.Index(s, "// ── Partial-completion continuation")
	if i < 0 {
		t.Fatal("continuation section not found")
	}
	cont := s[i:]
	if !strings.Contains(cont, "commitContinuation(ctx, worktreePath,") {
		t.Error("the continuation does not commit with commitContinuation")
	}
	if strings.Contains(cont, "continuation not committed: staging refused") {
		t.Error("the continuation still breaks out with its work staged")
	}
	if !strings.Contains(cont, "return generationResult{}, false") {
		t.Error("a continuation that cannot commit does not fail")
	}
}
