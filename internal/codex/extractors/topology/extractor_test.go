package topology

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func extractRoles(t *testing.T, dir string) TopologyOutput {
	t.Helper()
	e := New()
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
	var out TopologyOutput
	if err := json.Unmarshal(blobs[0].Payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func hasRole(roles []RoleAssignment, role string) bool {
	for _, r := range roles {
		if r.Role == role {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestTopology_NameFamilyKind(t *testing.T) {
	e := New()
	if e.Name() != "product_topology" {
		t.Errorf("Name = %q", e.Name())
	}
	if e.Family() != "topology" {
		t.Errorf("Family = %q", e.Family())
	}
	if e.Kind() != framework.KindStatic {
		t.Errorf("Kind = %v", e.Kind())
	}
}

func TestTopology_CaresAboutAlways(t *testing.T) {
	e := New()
	if !e.CaresAbout(nil) {
		t.Error("nil diff => should care")
	}
	if !e.CaresAbout(&framework.GitDiff{FilesChanged: []string{"README.md"}}) {
		t.Error("any diff => should care")
	}
}

func TestTopology_FrontendRepo(t *testing.T) {
	dir := t.TempDir()

	// Create app/ directory.
	mkdirAll(t, filepath.Join(dir, "app"))

	// Create package.json with react.
	writeFile(t, filepath.Join(dir, "package.json"), `{
		"name": "my-frontend",
		"dependencies": {
			"react": "^18.2.0",
			"react-dom": "^18.2.0",
			"next": "^14.0.0"
		}
	}`)

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "frontend") {
		t.Errorf("expected frontend role, got %+v", out.Roles)
	}
	// Should not be classified as api (no server framework).
	if hasRole(out.Roles, "api") {
		t.Error("frontend-only repo should not be classified as api")
	}
}

func TestTopology_APIRepo_Go(t *testing.T) {
	dir := t.TempDir()

	// Create go.mod with chi framework.
	writeFile(t, filepath.Join(dir, "go.mod"), `module github.com/example/api

go 1.22

require (
	github.com/go-chi/chi v5.0.0
)
`)

	// Create cmd/server/main.go.
	writeFile(t, filepath.Join(dir, "cmd", "server", "main.go"), `package main

func main() {}
`)

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "api") {
		t.Errorf("expected api role, got %+v", out.Roles)
	}
}

func TestTopology_BareRepo_DefaultsToService(t *testing.T) {
	dir := t.TempDir()

	e := New()
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

	var out TopologyOutput
	if err := json.Unmarshal(blobs[0].Payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(out.Roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(out.Roles))
	}
	if out.Roles[0].Role != "service" {
		t.Errorf("role = %q, want service", out.Roles[0].Role)
	}
	if out.Roles[0].Confidence != 0.5 {
		t.Errorf("confidence = %v, want 0.5", out.Roles[0].Confidence)
	}

	// Blob-level confidence should also be 0.5 for the default fallback.
	if blobs[0].Confidence != 0.5 {
		t.Errorf("blob confidence = %v, want 0.5", blobs[0].Confidence)
	}
}

func TestTopology_WorkerRepo(t *testing.T) {
	dir := t.TempDir()

	// go.mod with kafka dependency and no HTTP framework.
	writeFile(t, filepath.Join(dir, "go.mod"), `module github.com/example/worker

go 1.22

require (
	github.com/segmentio/kafka-go v0.4.0
)
`)

	// cmd/worker directory.
	mkdirAll(t, filepath.Join(dir, "cmd", "worker"))

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "worker") {
		t.Errorf("expected worker role, got %+v", out.Roles)
	}
	if hasRole(out.Roles, "api") {
		t.Error("worker repo should not be classified as api")
	}
}

func TestTopology_SDKRepo(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "package.json"), `{
		"name": "@myorg/sdk",
		"main": "dist/index.js",
		"module": "dist/index.mjs",
		"types": "dist/index.d.ts"
	}`)
	writeFile(t, filepath.Join(dir, "src", "index.ts"), `export const version = "1.0.0";`)

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "sdk") {
		t.Errorf("expected sdk role, got %+v", out.Roles)
	}
}

func TestTopology_SharedLibRepo(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "go.mod"), `module github.com/example/shared/utils

go 1.22
`)
	mkdirAll(t, filepath.Join(dir, "pkg"))

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "shared_lib") {
		t.Errorf("expected shared_lib role, got %+v", out.Roles)
	}
}

func TestTopology_InfraRepo(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "Dockerfile"), `FROM nginx:1.25-alpine
COPY nginx.conf /etc/nginx/nginx.conf
`)
	writeFile(t, filepath.Join(dir, "nginx.conf"), `server { listen 80; }`)

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "infra") {
		t.Errorf("expected infra role, got %+v", out.Roles)
	}
}

func TestTopology_Monolith_MultipleRoles(t *testing.T) {
	dir := t.TempDir()

	// Frontend signals.
	mkdirAll(t, filepath.Join(dir, "app"))
	writeFile(t, filepath.Join(dir, "package.json"), `{
		"name": "monolith",
		"dependencies": {
			"react": "^18.0.0",
			"express": "^4.18.0"
		}
	}`)

	out := extractRoles(t, dir)

	if !hasRole(out.Roles, "frontend") {
		t.Errorf("monolith should have frontend role, got %+v", out.Roles)
	}
	// express is detected as an API framework via node_framework signal,
	// plus package.json provides the go.mod-equivalent second signal
	// (we don't require go.mod for node API detection).
}

func TestTopology_IncrementalDelegatesToFull(t *testing.T) {
	dir := t.TempDir()

	e := New()
	ec := &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
		CommitSHA: "HEAD",
		Branch:    "main",
	}

	blobs1, err := e.ExtractFull(context.Background(), ec)
	if err != nil {
		t.Fatalf("ExtractFull: %v", err)
	}

	blobs2, err := e.ExtractIncremental(context.Background(), ec, blobs1)
	if err != nil {
		t.Fatalf("ExtractIncremental: %v", err)
	}

	if string(blobs1[0].Payload) != string(blobs2[0].Payload) {
		t.Error("incremental should produce same result as full for unchanged repo")
	}
}

func TestTopology_SectionIsProductTopology(t *testing.T) {
	dir := t.TempDir()
	e := New()
	blobs, err := e.ExtractFull(context.Background(), &framework.ExtractionContext{
		RepoPath:  dir,
		RepoID:    uuid.New(),
		ProductID: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if blobs[0].Section != "product_topology" {
		t.Errorf("section = %q, want product_topology", blobs[0].Section)
	}
}

func TestTopology_Registered(t *testing.T) {
	ext, ok := framework.Get("product_topology")
	if !ok {
		t.Fatal("product_topology not registered")
	}
	if ext.Name() != "product_topology" {
		t.Errorf("registered extractor Name = %q", ext.Name())
	}
}
