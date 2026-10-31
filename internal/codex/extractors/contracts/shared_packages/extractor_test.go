package shared_packages

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

func TestSharedPackages_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "contracts.shared_packages" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "contracts" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestSharedPackages_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("should NOT care about .md")
	}
}

func TestSharedPackages_ProducerExtract(t *testing.T) {
	e := New()
	dir := testdataDir(t)

	producers, err := e.ProducerExtract(context.Background(), &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("ProducerExtract: %v", err)
	}

	// Expect: @acme/ui = 1
	if len(producers) != 1 {
		for _, p := range producers {
			t.Logf("producer: type=%s name=%s file=%s line=%d", p.Type, p.Name, p.File, p.Line)
		}
		t.Fatalf("expected 1 producer, got %d", len(producers))
	}

	p := producers[0]
	t.Logf("producer: type=%s name=%s file=%s line=%d", p.Type, p.Name, p.File, p.Line)

	if p.Name != "@acme/ui" {
		t.Errorf("name = %q, want @acme/ui", p.Name)
	}
	if p.Type != "npm.workspace-package" {
		t.Errorf("type = %q, want npm.workspace-package", p.Type)
	}
}

func TestSharedPackages_ConsumerScan(t *testing.T) {
	e := New()
	dir := testdataDir(t)

	ec := &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	}

	producers, err := e.ProducerExtract(context.Background(), ec)
	if err != nil {
		t.Fatalf("ProducerExtract: %v", err)
	}

	blobs, err := e.ConsumerScan(context.Background(), ec, producers)
	if err != nil {
		t.Fatalf("ConsumerScan: %v", err)
	}
	if len(blobs) == 0 {
		t.Fatal("expected consumer blobs")
	}

	var edges []map[string]any
	if err := json.Unmarshal(blobs[0].Payload, &edges); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	found := false
	for _, edge := range edges {
		t.Logf("consumer edge: %v", edge)
		if p, ok := edge["producer"].(string); ok && p == "@acme/ui" {
			found = true
			if pattern, ok := edge["pattern"].(string); ok && pattern != "workspace-dependency" {
				t.Errorf("pattern = %q, want workspace-dependency", pattern)
			}
		}
	}

	if !found {
		t.Error("expected @acme/ui consumer edge")
	}
}

func TestSharedPackages_ExtractFull(t *testing.T) {
	e := New()
	dir := testdataDir(t)

	blobs, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("ExtractFull: %v", err)
	}
	if len(blobs) == 0 {
		t.Fatal("expected blobs from ExtractFull")
	}
	if blobs[0].Section != "contract_edges" {
		t.Errorf("section = %q", blobs[0].Section)
	}
}
