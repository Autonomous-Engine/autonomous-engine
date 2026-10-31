package design_tokens

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/google/uuid"
)

func TestCSSVars_ExtractsFull(t *testing.T) {
	e := NewCSSVars()
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

	var vars []CSSVar
	if err := json.Unmarshal(blobs[0].Payload, &vars); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if len(vars) != 4 {
		t.Fatalf("expected 4 vars, got %d: %+v", len(vars), vars)
	}

	expected := map[string]string{
		"--color-primary":   "#3B82F6",
		"--color-secondary": "#10B981",
		"--spacing-sm":      "0.5rem",
		"--font-body":       "'Inter', sans-serif",
	}

	for _, v := range vars {
		want, ok := expected[v.Name]
		if !ok {
			t.Errorf("unexpected var %q", v.Name)
			continue
		}
		if v.Value != want {
			t.Errorf("var %q = %q, want %q", v.Name, v.Value, want)
		}
		if v.File != "globals.css" {
			t.Errorf("var %q file = %q, want globals.css", v.Name, v.File)
		}
		if v.Line == 0 {
			t.Errorf("var %q should have a line number", v.Name)
		}
	}

	if blobs[0].Section != "design_tokens_css_vars" {
		t.Errorf("section = %q, want design_tokens_css_vars", blobs[0].Section)
	}
	if blobs[0].Confidence != 0.95 {
		t.Errorf("confidence = %v, want 0.95", blobs[0].Confidence)
	}
}

func TestCSSVars_NameAndKind(t *testing.T) {
	e := NewCSSVars()
	if e.Name() != "design_tokens.css_vars" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "design_tokens" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestCSSVars_CaresAbout(t *testing.T) {
	e := NewCSSVars()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"styles/globals.css"}}) {
		t.Error("should care about CSS files")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"main.go"}}) {
		t.Error("should NOT care about Go files")
	}
}

func TestCSSVars_NoCSSReturnsNil(t *testing.T) {
	e := NewCSSVars()
	blobs, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath:  t.TempDir(),
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blobs != nil {
		t.Error("expected nil blobs when no CSS files exist")
	}
}
