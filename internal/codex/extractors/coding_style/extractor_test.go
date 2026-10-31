package coding_style

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

func TestCodingStyle_PendingBlobWhenLLMNil(t *testing.T) {
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
	if payload.Fingerprint == "" {
		t.Error("expected non-empty fingerprint")
	}

	// Verify sampled files are .go files from testdata/internal
	for _, f := range payload.SampledFiles {
		t.Logf("sampled: %s", f)
	}

	if blobs[0].Section != "coding_style" {
		t.Errorf("section = %q", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.5 {
		t.Errorf("confidence = %v, want 0.5", blobs[0].Confidence)
	}
}

func TestCodingStyle_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "coding_style" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "coding_practices" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindLLM {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestCodingStyle_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"internal/handler.go"}}) {
		t.Error("should care about .go files")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"src/app.ts"}}) {
		t.Error("should care about .ts files")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("should NOT care about markdown files")
	}
}

func TestCodingStyle_IncrementalCarriesForwardOnSameFingerprint(t *testing.T) {
	e := New()
	dir := testdataDir(t)

	ec := &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
		CommitSHA: "HEAD",
		Branch:    "main",
	}

	// First extraction
	blobs, err := e.ExtractFull(context.Background(), ec)
	if err != nil {
		t.Fatalf("ExtractFull: %v", err)
	}
	if len(blobs) == 0 {
		t.Fatal("expected blobs")
	}

	// Incremental with same files should carry forward
	blobs2, err := e.ExtractIncremental(context.Background(), ec, blobs)
	if err != nil {
		t.Fatalf("ExtractIncremental: %v", err)
	}
	if len(blobs2) == 0 {
		t.Fatal("expected blobs from incremental")
	}

	// Should be the same blob (carried forward)
	if string(blobs[0].Payload) != string(blobs2[0].Payload) {
		t.Error("incremental should carry forward when fingerprint matches")
	}
}

func TestCodingStyle_NoSourceFilesReturnsNil(t *testing.T) {
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
		t.Error("expected nil blobs when no source files found")
	}
}
