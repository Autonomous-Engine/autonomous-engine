package sql_schemas

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

func TestSQLSchemas_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "contracts.sql_schemas" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "contracts" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestSQLSchemas_CaresAbout(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("should NOT care about .md")
	}
}

func TestSQLSchemas_ProducerExtract(t *testing.T) {
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

	// Expect: users (table), users.email (column), orders (table) = 3
	if len(producers) != 3 {
		for _, p := range producers {
			t.Logf("producer: type=%s name=%s file=%s line=%d", p.Type, p.Name, p.File, p.Line)
		}
		t.Fatalf("expected 3 producers, got %d", len(producers))
	}

	names := map[string]string{}
	for _, p := range producers {
		names[p.Name] = p.Type
		t.Logf("producer: type=%s name=%s file=%s line=%d", p.Type, p.Name, p.File, p.Line)
	}

	if names["users"] != "sql.table" {
		t.Errorf("users type = %q, want sql.table", names["users"])
	}
	if names["users.email"] != "sql.column" {
		t.Errorf("users.email type = %q, want sql.column", names["users.email"])
	}
	if names["orders"] != "sql.table" {
		t.Errorf("orders type = %q, want sql.table", names["orders"])
	}
}

func TestSQLSchemas_ConsumerScan(t *testing.T) {
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

	foundTables := map[string]bool{}
	for _, edge := range edges {
		if p, ok := edge["producer"].(string); ok {
			foundTables[p] = true
			t.Logf("consumer edge: %v", edge)
		}
	}

	if !foundTables["users"] {
		t.Error("expected users consumer edge")
	}
	if !foundTables["orders"] {
		t.Error("expected orders consumer edge")
	}
}

func TestSQLSchemas_ExtractFull(t *testing.T) {
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
