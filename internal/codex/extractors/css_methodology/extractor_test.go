package css_methodology

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

func TestCSSMethodology_DetectsTailwindAndModules(t *testing.T) {
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

	var out MethodologyOutput
	if err := json.Unmarshal(blobs[0].Payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.Primary != "tailwind" {
		t.Errorf("primary = %q, want tailwind", out.Primary)
	}
	if out.Secondary != "css_modules" {
		t.Errorf("secondary = %q, want css_modules", out.Secondary)
	}

	// Check indicators contain expected values
	hasIndicator := func(name string) bool {
		for _, ind := range out.Indicators {
			if ind == name {
				return true
			}
		}
		return false
	}
	if !hasIndicator("tailwind") {
		t.Error("indicators should include tailwind")
	}
	if !hasIndicator("css_modules") {
		t.Error("indicators should include css_modules")
	}

	if blobs[0].Section != "css_methodology" {
		t.Errorf("section = %q", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.90 {
		t.Errorf("confidence = %v, want 0.90", blobs[0].Confidence)
	}
}

func TestCSSMethodology_NameAndKind(t *testing.T) {
	e := New()
	if e.Name() != "css_methodology" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "design_system" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestCSSMethodology_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"styles/app.css"}}) {
		t.Error("should care about CSS files")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"package.json"}}) {
		t.Error("should care about package.json")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"main.go"}}) {
		t.Error("should NOT care about Go files")
	}
}

func TestCSSMethodology_EmptyDirReturnsNil(t *testing.T) {
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
		t.Error("expected nil blobs when empty dir")
	}
}
