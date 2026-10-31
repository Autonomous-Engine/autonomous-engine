package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GET /api/codex/version needs a product: there is no global codex version.
func TestGetCodexVersion_RequiresProduct(t *testing.T) {
	h := &Handlers{}
	for _, q := range []string{"", "?product_id=nope"} {
		rec := httptest.NewRecorder()
		h.GetCodexVersion(rec, httptest.NewRequest(http.MethodGet, "/api/codex/version"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", q, rec.Code)
		}
	}
}

// The version is the product and branch's product_version; another
// product's writes do not move it (TEST_POSTGRES_DSN, migrated).
func TestIntegration_GetCodexVersionIsPerProductAndBranch(t *testing.T) {
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
	a, b := uuid.New(), uuid.New()
	for _, p := range []uuid.UUID{a, b} {
		if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, p, "p0-dash-"+p.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r', $3, 'trunk')`, uuid.New(), p, "o/"+p.String()); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE aggregate_id = ANY($1)`, []uuid.UUID{a, b})
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = ANY($1)`, []uuid.UUID{a, b})
	}()
	cx := codex.New(pool)
	write := func(p uuid.UUID, branch, summary string) {
		if _, err := cx.RecordVersion(ctx, codex.NewVersion{ProductID: p, Branch: branch, Summary: summary, Source: models.CodexChangeSourceHumanUpdate}); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "trunk", "a1")
	write(b, "trunk", "b1")
	write(b, "trunk", "b2")
	write(a, "trunk", "a2")
	write(a, "feature", "af1")

	h := &Handlers{codex: cx}
	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		h.GetCodexVersion(rec, httptest.NewRequest(http.MethodGet, "/api/codex/version?"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", q, rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// No branch: the product's default branch.
	got := get("product_id=" + a.String())
	if got["version"] != float64(2) || got["version_number"] != float64(2) || got["product_version"] != float64(2) || got["branch"] != "trunk" || got["change_summary"] != "a2" {
		t.Errorf("default branch: %v", got)
	}
	if got := get("product_id=" + a.String() + "&branch=feature"); got["version"] != float64(1) || got["change_summary"] != "af1" {
		t.Errorf("feature branch: %v", got)
	}
	if got := get("product_id=" + a.String() + "&branch=none"); got["version"] != float64(0) {
		t.Errorf("branch without versions: %v", got)
	}

	// A product with no enabled repository has no default branch, and so no
	// codex version yet: 200 with version 0, not an error that fails the
	// whole Codex view.
	bare := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, bare, "p0-dash-bare-"+bare.String()); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, bare) }()
	if got := get("product_id=" + bare.String()); got["version"] != float64(0) || got["branch"] != "" {
		t.Errorf("product without a repository: %v", got)
	}
}
