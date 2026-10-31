package agentregistry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/autonomousengine/autonomous-engine/internal/db"
)

// Against a real PostgreSQL with all migrations applied (migration 012's
// TEXT[] NOT NULL routing columns). Skips without TEST_POSTGRES_DSN /
// TEST_DATABASE_URL. Point it at a fresh, disposable database.

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN / TEST_DATABASE_URL not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// The seed on an empty table inserts both definitions; read back, the
// unconstrained one has empty (not NULL) routing arrays; a second run is a
// no-op; Create/Update with nil slices succeed. Runs in a rolled-back
// transaction so it leaves the database as it found it.
func TestIntegration_SeedDefaultsTx_EmptyTable(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `DELETE FROM agent_definitions`); err != nil {
		t.Fatalf("empty table: %v", err)
	}
	if err := SeedDefaultsTx(ctx, tx); err != nil {
		t.Fatalf("SeedDefaultsTx: %v", err)
	}
	if err := SeedDefaultsTx(ctx, tx); err != nil {
		t.Fatalf("SeedDefaultsTx (second run): %v", err)
	}

	rows, err := tx.Query(ctx, `SELECT `+defColumns+` FROM agent_definitions ORDER BY name`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defs, err := collectDefs(rows)
	rows.Close()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("got %d definitions, want 2", len(defs))
	}
	full, quick := defs[0], defs[1]
	if full.Name != "full-stack-engineer" || !full.IsDefault || len(full.RoutingTags) != 0 || len(full.RoutingComplexity) != 0 {
		t.Fatalf("full-stack-engineer read back wrong: %+v", full)
	}
	if quick.Name != "quick-text-editor" || len(quick.RoutingTags) != 3 || len(quick.RoutingComplexity) != 2 {
		t.Fatalf("quick-text-editor read back wrong: %+v", quick)
	}
	var nulls int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_definitions WHERE routing_tags IS NULL OR routing_complexity IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("null check: %v", err)
	}
	if nulls != 0 {
		t.Fatalf("%d rows have NULL routing arrays", nulls)
	}

	def := &AgentDefinition{Name: "f1-nil-slices-" + uuid.NewString()[:8]}
	if err := CreateTx(ctx, tx, def); err != nil {
		t.Fatalf("CreateTx with nil slices: %v", err)
	}
	if err := UpdateTx(ctx, tx, def.ID, def); err != nil {
		t.Fatalf("UpdateTx with nil slices: %v", err)
	}
}

// The public SeedDefaults (its own committed transaction), as
// status-orchestrator calls it on startup. Skipped unless the table is
// empty (a fresh database).
func TestIntegration_SeedDefaults_FreshDatabase(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_definitions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n > 0 {
		t.Skipf("agent_definitions already has %d rows — needs a fresh database", n)
	}
	reg := New(pool)
	if err := reg.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	if err := reg.SeedDefaults(ctx); err != nil {
		t.Fatalf("SeedDefaults (second run): %v", err)
	}
	all, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d definitions, want 2", len(all))
	}
	def, err := reg.GetDefault(ctx)
	if err != nil || def == nil || def.Name != "full-stack-engineer" {
		t.Fatalf("GetDefault = %+v, %v", def, err)
	}
}
