package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepoForTest(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "one"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// A model-supplied ref like "--output=<path>" would make git diff write a
// file (e.g. a hook in another clone of the shared repo-cache). It must be
// rejected and nothing written.
func TestGitDiff_RejectsOptionLikeRef(t *testing.T) {
	repo := gitRepoForTest(t)
	target := filepath.Join(t.TempDir(), "post-checkout")
	e := NewExecutor(repo)
	for _, ref := range []string{"--output=" + target, " -O" + target, "-p"} {
		res := e.gitDiff(context.Background(), "c1", map[string]interface{}{"ref": ref})
		if !res.IsError || !strings.Contains(res.Content, "looks like a git option") {
			t.Fatalf("ref %q: result = %+v, want a rejection", ref, res)
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("git diff wrote %s: %v", target, err)
	}
	// A real ref still works.
	if res := e.gitDiff(context.Background(), "c2", map[string]interface{}{"ref": "HEAD"}); res.IsError {
		t.Fatalf("HEAD: %+v", res)
	}
}

func TestGitDiff_DiffBaseStillWorks(t *testing.T) {
	repo := gitRepoForTest(t)
	e := NewExecutor(repo)
	e.SetDiffBase("HEAD")
	if res := e.gitDiff(context.Background(), "c", map[string]interface{}{}); res.IsError || strings.Contains(res.Content, "fatal") {
		t.Fatalf("diff base: %+v", res)
	}
}

func TestGitLog_RejectsOptionLikePath(t *testing.T) {
	repo := gitRepoForTest(t)
	target := filepath.Join(t.TempDir(), "out")
	e := NewExecutor(repo)
	res := e.gitLog(context.Background(), "c", map[string]interface{}{"path": "--output=" + target})
	if !res.IsError || !strings.Contains(res.Content, "looks like a git option") {
		t.Fatalf("result = %+v, want a rejection", res)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("git log wrote %s", target)
	}
	if res := e.gitLog(context.Background(), "c", map[string]interface{}{}); res.IsError || !strings.Contains(res.Content, "one") {
		t.Fatalf("plain path: %+v", res)
	}
}
