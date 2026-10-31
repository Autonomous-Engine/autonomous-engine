package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A legacy-unpinned item (or no database) renders empty arrays, never null:
// the frontend maps over them.
func TestLoadBaseSnapshot_NoDatabaseIsEmptyArrays(t *testing.T) {
	pins, events := loadBaseSnapshot(context.Background(), nil, uuid.New())
	b, _ := json.Marshal(map[string]any{"base_shas": pins, "base_sha_events": events})
	if string(b) != `{"base_sha_events":[],"base_shas":[]}` {
		t.Fatalf("got %s", b)
	}
}

// M2-02 detail payload against PostgreSQL (TEST_POSTGRES_DSN, migrated).
func TestIntegration_LoadBaseSnapshot(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	product, repo, item := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{product, "m202-dash-" + product.String()}},
		{`INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'dash-repo', $3, 'main')`, []any{repo, product, "o/" + repo.String()}},
		{`INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'prd_approval', 'M2-02 dash', $2)`, []any{item, product}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, item)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, product)
	}()
	const sha = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.NewStore(nil).PinTx(ctx, tx, snapshot.Pin{WorkItemID: item, RepositoryID: repo, BaseRef: "main", BaseSHA: sha}, "intake: test"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var q rowQuerier = pool
	pins, events := loadBaseSnapshot(ctx, q, item)
	if len(pins) != 1 || pins[0].BaseSHA != sha || pins[0].BaseRef != "main" || pins[0].RepositoryName != "dash-repo" || pins[0].RepositoryID != repo.String() {
		t.Fatalf("pins = %+v", pins)
	}
	if len(events) != 1 || events[0].Kind != "resolved" || events[0].NewSHA != sha || !strings.Contains(events[0].Detail, "intake") {
		t.Fatalf("events = %+v", events)
	}
}
