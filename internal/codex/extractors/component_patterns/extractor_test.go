package component_patterns

import (
	"context"
	"encoding/json"
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

func TestComponentPatterns_PendingBlobWhenLLMNil(t *testing.T) {
	e := New() // no LLM set
	dir := testdataDir(t)

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

	var payload PendingOutput
	if err := json.Unmarshal(blobs[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if payload.Status != "pending_llm_analysis" {
		t.Errorf("status = %q, want pending_llm_analysis", payload.Status)
	}
	if len(payload.SampledFiles) == 0 {
		t.Error("expected sampled_files to contain entries")
	}

	// Should contain Button.tsx
	found := false
	for _, f := range payload.SampledFiles {
		t.Logf("sampled: %s", f)
		if filepath.Base(f) == "Button.tsx" {
			found = true
		}
	}
	if !found {
		t.Error("expected Button.tsx in sampled files")
	}

	if payload.Fingerprint == "" {
		t.Error("expected non-empty fingerprint")
	}

	if blobs[0].Section != "component_patterns" {
		t.Errorf("section = %q", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.5 {
		t.Errorf("confidence = %v, want 0.5", blobs[0].Confidence)
	}
}

func TestComponentPatterns_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "component_patterns" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "coding_practices" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindLLM {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestComponentPatterns_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("should NOT care about markdown files")
	}
}

func TestComponentPatterns_NoComponentFilesReturnsNil(t *testing.T) {
	e := New()
	blobs, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath:  t.TempDir(),
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blobs != nil {
		t.Error("expected nil blobs when no component files found")
	}
}

func TestComponentPatterns_IncrementalCarriesForward(t *testing.T) {
	e := New()
	dir := testdataDir(t)

	ec := &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
		CommitSHA: "HEAD",
		Branch:    "main",
	}

	blobs, err := e.ExtractFull(context.Background(), ec)
	if err != nil {
		t.Fatalf("ExtractFull: %v", err)
	}
	if len(blobs) == 0 {
		t.Fatal("expected blobs")
	}

	blobs2, err := e.ExtractIncremental(context.Background(), ec, blobs)
	if err != nil {
		t.Fatalf("ExtractIncremental: %v", err)
	}
	if len(blobs2) == 0 {
		t.Fatal("expected blobs from incremental")
	}

	if string(blobs[0].Payload) != string(blobs2[0].Payload) {
		t.Error("incremental should carry forward when fingerprint matches")
	}
}
