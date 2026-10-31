package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func zoneRequest(t *testing.T, handler http.HandlerFunc, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	req = req.WithContext(context.WithValue(req.Context(), contextKeyUserID, uuid.NewString()))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// Admin → Products → Repository → Zones (codex zones, migration 063): list,
// create, edit patterns and policy, freeze and thaw with a reason, delete.
// Every change writes a codex version with section:zones for the
// repository's product on its branch (TEST_POSTGRES_DSN, migrated).
func TestIntegration_ZonesAPIWritesZonesVersions(t *testing.T) {
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
	// Closed after the row cleanup below (cleanups run last-in first-out),
	// so no codex.indexed row is left for another package's dispatcher.
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	pid, rid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, pid, "p2-dash-"+pid.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM outbox_events WHERE aggregate_id = $1`, pid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, pid)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'api', $3, 'release')`, rid, pid, "o/"+rid.String()); err != nil {
		t.Fatal(err)
	}
	versions := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_versions WHERE product_id = $1 AND branch = 'release' AND $2 = ANY(sections_changed)`, pid, zones.ZonesSection).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	h := &AdminHandlers{db: pool}
	repoZones := "/api/admin/repositories/zones?id=" + rid.String()

	rec := zoneRequest(t, h.RepositoryZones, http.MethodPost, repoZones, map[string]any{
		"name": "payments", "description": "money", "patterns": []string{"payments/**"}, "policy": "human_review",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var z zones.Zone
	if err := json.Unmarshal(rec.Body.Bytes(), &z); err != nil {
		t.Fatal(err)
	}
	if z.Source != zones.SourceDashboard || z.Policy != zones.PolicyHumanReview || versions() != 1 {
		t.Fatalf("created %+v, versions %d", z, versions())
	}
	// Duplicate name in the same repository: 409, no version.
	if rec := zoneRequest(t, h.RepositoryZones, http.MethodPost, repoZones, map[string]any{"name": "payments", "patterns": []string{"x/**"}, "policy": "autonomous"}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d %s", rec.Code, rec.Body.String())
	}
	// Invalid policy: 400.
	if rec := zoneRequest(t, h.RepositoryZones, http.MethodPost, repoZones, map[string]any{"name": "x", "patterns": []string{"x/**"}, "policy": "maybe"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad policy = %d", rec.Code)
	}

	zoneURL := "/api/admin/zones?id=" + z.ID.String()
	rec = zoneRequest(t, h.Zone, http.MethodPut, zoneURL, map[string]any{"patterns": []string{"payments/**", "billing/**"}, "policy": "escalate"})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &z)
	if z.Policy != zones.PolicyEscalate || len(z.Patterns) != 2 || versions() != 2 {
		t.Fatalf("edited %+v, versions %d", z, versions())
	}

	if rec := zoneRequest(t, h.FreezeZone, http.MethodPost, "/api/admin/zones/freeze?id="+z.ID.String(), map[string]any{"reason": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("freeze without reason = %d", rec.Code)
	}
	rec = zoneRequest(t, h.FreezeZone, http.MethodPost, "/api/admin/zones/freeze?id="+z.ID.String(), map[string]any{"reason": "audit"})
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze = %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &z)
	if !z.Frozen() || versions() != 3 {
		t.Fatalf("frozen %+v, versions %d", z, versions())
	}
	rec = zoneRequest(t, h.ThawZone, http.MethodPost, "/api/admin/zones/thaw?id="+z.ID.String(), nil)
	if rec.Code != http.StatusOK || versions() != 4 {
		t.Fatalf("thaw = %d %s, versions %d", rec.Code, rec.Body.String(), versions())
	}

	rec = zoneRequest(t, h.RepositoryZones, http.MethodGet, repoZones, nil)
	var list []zones.Zone
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 1 || list[0].Frozen() {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	if rec := zoneRequest(t, h.Zone, http.MethodDelete, zoneURL, nil); rec.Code != http.StatusNoContent || versions() != 5 {
		t.Fatalf("delete = %d, versions %d", rec.Code, versions())
	}
	if rec := zoneRequest(t, h.Zone, http.MethodPut, zoneURL, map[string]any{"policy": "autonomous"}); rec.Code != http.StatusNotFound {
		t.Fatalf("edit deleted = %d", rec.Code)
	}
	if rec := zoneRequest(t, h.RepositoryZones, http.MethodGet, "/api/admin/repositories/zones?id=nope", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", rec.Code)
	}
}
