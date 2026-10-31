package git_strategy

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/google/uuid"
)

func newFixtureRepo(t *testing.T, pattern string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "x@y.z"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if pattern == "gitflow" {
		cmd := exec.Command("git", "-C", dir, "branch", "develop")
		_ = cmd.Run()
	}
	return filepath.ToSlash(dir)
}

func TestGitStrategy_DetectsTrunkBasedFromSingleMain(t *testing.T) {
	dir := newFixtureRepo(t, "")
	e := New()
	out, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath: dir, RepoID: uuid.New(), CommitSHA: "HEAD", Branch: "main",
	})
	if err != nil {
		t.Fatalf("ExtractFull: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected at least one blob")
	}
	var payload map[string]any
	_ = json.Unmarshal(out[0].Payload, &payload)
	if payload["main_branch"] != "main" {
		t.Errorf("main_branch = %v", payload["main_branch"])
	}
	if payload["pattern"] != "trunk_based" {
		t.Errorf("pattern = %v, want trunk_based", payload["pattern"])
	}
}

func TestGitStrategy_DetectsGitflowFromDevelopBranch(t *testing.T) {
	dir := newFixtureRepo(t, "gitflow")
	e := New()
	out, _ := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath: dir, RepoID: uuid.New(), CommitSHA: "HEAD", Branch: "main",
	})
	var payload map[string]any
	_ = json.Unmarshal(out[0].Payload, &payload)
	if payload["pattern"] != "gitflow" {
		t.Errorf("pattern = %v, want gitflow", payload["pattern"])
	}
	lls, _ := payload["long_lived_branches"].([]any)
	found := false
	for _, b := range lls {
		if b == "develop" {
			found = true
		}
	}
	if !found {
		t.Errorf("long_lived_branches should contain develop: %v", lls)
	}
}

func TestGitStrategy_CaresAboutGitRefsOnly(t *testing.T) {
	e := New()
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{".github/PULL_REQUEST_TEMPLATE.md"}}) {
		t.Error("should care about PR template changes")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"app/page.tsx"}}) {
		t.Error("should NOT care about source file changes")
	}
}

func TestGitStrategy_KindIsStatic(t *testing.T) {
	if New().Kind() != framework.KindStatic {
		t.Error("git_strategy is pure git-log analysis; Kind should be Static")
	}
}
