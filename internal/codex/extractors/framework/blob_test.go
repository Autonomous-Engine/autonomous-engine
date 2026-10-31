// internal/codex/extractors/framework/blob_test.go
package framework_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func fwPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return p
}

func TestBlobStore_InsertAndGet(t *testing.T) {
	p := fwPool(t)
	defer p.Close()
	bs := framework.NewBlobStore(p)
	ctx := context.Background()

	b := &framework.Blob{
		Section:    "module",
		Payload:    []byte(`{"name":"hello"}`),
		Confidence: 0.95,
	}
	if err := bs.Insert(ctx, b, "test_extractor"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if b.Hash == "" {
		t.Fatal("Insert should set Blob.Hash")
	}
	got, err := bs.Get(ctx, b.Hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal(got.Payload, &payload)
	if payload["name"] != "hello" {
		t.Errorf("payload round-trip: %+v", payload)
	}
}

func TestBlobStore_IdempotentInsert(t *testing.T) {
	p := fwPool(t)
	defer p.Close()
	bs := framework.NewBlobStore(p)
	ctx := context.Background()

	b1 := &framework.Blob{Section: "module", Payload: []byte(`{"n":1}`), Confidence: 0.9}
	b2 := &framework.Blob{Section: "module", Payload: []byte(`{"n":1}`), Confidence: 0.9}
	if err := bs.Insert(ctx, b1, "tx"); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	if err := bs.Insert(ctx, b2, "tx"); err != nil {
		t.Fatalf("Insert 2 (same payload): %v", err)
	}
	if b1.Hash != b2.Hash {
		t.Errorf("same payload → different hash: %s vs %s", b1.Hash, b2.Hash)
	}
}

func TestSnapshotStore_PutAndGet(t *testing.T) {
	p := fwPool(t)
	defer p.Close()
	ctx := context.Background()
	ss := framework.NewSnapshotStore(p)
	bs := framework.NewBlobStore(p)

	productID := uuid.New()
	_, _ = p.Exec(ctx, `INSERT INTO products (id, name, created_at, updated_at) VALUES ($1, $2, NOW(), NOW())`, productID, "fx-"+productID.String()[:8])
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM codex_snapshots WHERE repo_id IN (SELECT id FROM repositories WHERE product_id = $1)`, productID)
		_, _ = p.Exec(context.Background(), `DELETE FROM repositories WHERE product_id = $1`, productID)
		_, _ = p.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, productID)
	})
	repoID := uuid.New()
	_, _ = p.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch, merge_strategy, clone_path, created_at) VALUES ($1, $2, 'fx-repo', 'https://x.y/z', 'main', 'squash', '/tmp/r', NOW())`, repoID, productID)

	b1 := &framework.Blob{Section: "module", Payload: []byte(`{"a":1}`), Confidence: 0.9}
	b2 := &framework.Blob{Section: "pattern", Payload: []byte(`{"b":2}`), Confidence: 0.9}
	_ = bs.Insert(ctx, b1, "x")
	_ = bs.Insert(ctx, b2, "x")

	snap := &framework.Snapshot{
		RepoID:     repoID,
		CommitSHA:  "deadbeef",
		Branch:     "main",
		ParentSHA:  "",
		BlobHashes: []string{b1.Hash, b2.Hash},
	}
	if err := ss.Put(ctx, snap); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ss.Get(ctx, repoID, "deadbeef")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.BlobHashes) != 2 {
		t.Errorf("BlobHashes length = %d, want 2", len(got.BlobHashes))
	}
}
