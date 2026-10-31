package graphql

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

func TestGraphQL_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "contracts.graphql" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "contracts" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestGraphQL_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"src/schema.graphql"}}) {
		t.Error("should care about .graphql")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("should NOT care about .md")
	}
}

func TestGraphQL_ProducerExtract(t *testing.T) {
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

	if len(producers) != 4 {
		t.Fatalf("expected 4 producers, got %d", len(producers))
	}

	names := map[string]bool{}
	for _, p := range producers {
		names[p.Name] = true
		t.Logf("producer: type=%s name=%s file=%s line=%d", p.Type, p.Name, p.File, p.Line)
	}

	for _, want := range []string{"users", "user", "createUser", "deleteUser"} {
		if !names[want] {
			t.Errorf("missing producer %q", want)
		}
	}
}

func TestGraphQL_ConsumerScan(t *testing.T) {
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
		if edge["producer"] == "createUser" {
			found = true
			t.Logf("consumer edge: %v", edge)
		}
	}
	if !found {
		t.Error("expected createUser consumer edge")
	}
}

func TestGraphQL_ExtractFull(t *testing.T) {
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
	if blobs[0].Confidence != 0.90 {
		t.Errorf("confidence = %v, want 0.90", blobs[0].Confidence)
	}
}
