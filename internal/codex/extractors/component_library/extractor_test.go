package component_library

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

func TestComponentLibrary_DetectsRadixWithCVA(t *testing.T) {
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

	var info LibraryInfo
	if err := json.Unmarshal(blobs[0].Payload, &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Should detect radix-ui
	foundRadix := false
	for _, lib := range info.Libraries {
		if lib.Name == "radix-ui" {
			foundRadix = true
		}
	}
	if !foundRadix {
		t.Error("should detect radix-ui")
	}

	// Should detect CVA
	if !info.HasCVA {
		t.Error("should detect class-variance-authority (CVA)")
	}

	// Should identify as shadcn stack (radix + cva + tailwind-merge)
	if !info.HasShadcn {
		t.Error("should identify as shadcn stack")
	}

	if info.StackLabel != "shadcn/ui (radix + cva)" {
		t.Errorf("stack_label = %q, want %q", info.StackLabel, "shadcn/ui (radix + cva)")
	}

	if blobs[0].Section != "component_library" {
		t.Errorf("section = %q", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.95 {
		t.Errorf("confidence = %v", blobs[0].Confidence)
	}
}

func TestComponentLibrary_NameAndKind(t *testing.T) {
	e := New()
	if e.Name() != "component_library" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "design_system" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestComponentLibrary_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"package.json"}}) {
		t.Error("should care about package.json")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesAdded: []string{"components.json"}}) {
		t.Error("should care about components.json")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"app/page.tsx"}}) {
		t.Error("should NOT care about source files")
	}
}

func TestComponentLibrary_NoPackageJSONReturnsNil(t *testing.T) {
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
		t.Error("expected nil blobs when no package.json exists")
	}
}
