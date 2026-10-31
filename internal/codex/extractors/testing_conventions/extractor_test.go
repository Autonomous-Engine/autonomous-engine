package testing_conventions

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

func TestTestingConventions_DetectsVitest(t *testing.T) {
	e := New()
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

	var payload StaticOutput
	if err := json.Unmarshal(blobs[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if payload.TestFramework != "vitest" {
		t.Errorf("test_framework = %q, want vitest", payload.TestFramework)
	}

	foundVitest := false
	for _, f := range payload.TestFrameworks {
		if f == "vitest" {
			foundVitest = true
		}
	}
	if !foundVitest {
		t.Error("expected vitest in test_frameworks list")
	}

	// Should also detect testing-library.
	foundTL := false
	for _, f := range payload.TestFrameworks {
		if f == "testing-library" {
			foundTL = true
		}
	}
	if !foundTL {
		t.Error("expected testing-library in test_frameworks list")
	}

	if payload.Language != "javascript" {
		t.Errorf("language = %q, want javascript", payload.Language)
	}

	if payload.LLMStatus != "pending" {
		t.Errorf("llm_status = %q, want pending (no LLM adapter set)", payload.LLMStatus)
	}

	if payload.Fingerprint == "" {
		t.Error("expected non-empty fingerprint")
	}

	if blobs[0].Section != "testing_conventions" {
		t.Errorf("section = %q", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.85 {
		t.Errorf("confidence = %v, want 0.85", blobs[0].Confidence)
	}
}

func TestTestingConventions_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "testing_conventions" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "coding_practices" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindHybrid {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestTestingConventions_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"package.json"}}) {
		t.Error("should care about package.json")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"go.mod"}}) {
		t.Error("should care about go.mod")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("should NOT care about markdown files")
	}
}

func TestTestingConventions_NoFrameworksReturnsNil(t *testing.T) {
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
		t.Error("expected nil blobs when no test frameworks detected")
	}
}

func TestTestingConventions_IncrementalCarriesForward(t *testing.T) {
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
