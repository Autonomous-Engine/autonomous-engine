package design_tokens

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/google/uuid"
)

func testdataDir(t *testing.T) string {
	t.Helper()
	_, f, _, _ := runtime.Caller(0)
	return filepath.ToSlash(filepath.Join(filepath.Dir(f), "testdata"))
}

// tailwindFixtureDir copies the testdata fixture into a temp dir
// with the canonical filename tailwind.config.ts.
func tailwindFixtureDir(t *testing.T) string {
	t.Helper()
	src := filepath.Join(testdataDir(t), "simple-tailwind.config.ts")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "tailwind.config.ts")
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return filepath.ToSlash(dir)
}

func TestTailwind_ExtractsFull(t *testing.T) {
	e := NewTailwind()
	dir := tailwindFixtureDir(t)

	blobs, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
		CommitSHA: "HEAD",
		Branch:    "main",
	})
	if err != nil {
		t.Fatalf("ExtractFull: %v", err)
	}
	if len(blobs) == 0 {
		t.Fatal("expected at least one blob")
	}

	var tokens TailwindTokens
	if err := json.Unmarshal(blobs[0].Payload, &tokens); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	// Assert colors
	for _, key := range []string{"brand", "accent", "muted"} {
		if _, ok := tokens.Colors[key]; !ok {
			t.Errorf("missing color key %q", key)
		}
	}

	// Assert spacing
	for _, key := range []string{"18", "22"} {
		if _, ok := tokens.Spacing[key]; !ok {
			t.Errorf("missing spacing key %q", key)
		}
	}

	// Assert metadata
	if blobs[0].Section != "design_tokens" {
		t.Errorf("section = %q, want design_tokens", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95", blobs[0].Confidence)
	}
}

func TestTailwind_NameAndKind(t *testing.T) {
	e := NewTailwind()
	if e.Name() != "design_tokens.tailwind" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "design_tokens" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestTailwind_CaresAbout(t *testing.T) {
	e := NewTailwind()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"tailwind.config.ts"}}) {
		t.Error("should care about tailwind.config.ts")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"app/page.tsx"}}) {
		t.Error("should NOT care about app/page.tsx")
	}
}

func TestTailwind_NoConfigReturnsNil(t *testing.T) {
	e := NewTailwind()
	blobs, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath:  t.TempDir(),
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blobs != nil {
		t.Error("expected nil blobs when no config exists")
	}
}
