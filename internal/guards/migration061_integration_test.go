package guards

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration 061 up → down → up on a database of its own: up creates the
// final Part 3 schema (refusals with block_transition_id and
// index_posted_at and both partial indexes, gap runs, gap events with
// event_at), down removes all of it and is re-runnable, and up applies again
// and is re-runnable too.
func TestIntegration_Migration061UpDownUp(t *testing.T) {
	base := integrationPool(t)
	ctx := context.Background()
	name := "ae_m061_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := base.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = base.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	dir := filepath.Join("..", "..", "migrations")
	if err := db.Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sqlOf := func(file string) string {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	exists := func(q string, args ...any) bool {
		var ok bool
		if err := pool.QueryRow(ctx, q, args...).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	column := `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`
	relation := `SELECT to_regclass($1) IS NOT NULL`
	columns := [][2]string{
		{"codex_grounding_refusals", "block_transition_id"},
		{"codex_grounding_refusals", "index_posted_at"},
		{"codex_grounding_refusals", "reindex_requested_at"},
		{"codex_gap_runs", "ungrounded_at"},
		{"codex_gap_events", "event_at"},
	}
	relations := []string{"codex_grounding_refusals", "codex_gap_runs", "codex_gap_events",
		"codex_grounding_refusals_reindex_idx", "codex_grounding_refusals_unposted_idx", "codex_gap_events_work_item_idx"}
	checkUp := func(when string) {
		t.Helper()
		for _, c := range columns {
			if !exists(column, c[0], c[1]) {
				t.Errorf("%s: %s.%s missing", when, c[0], c[1])
			}
		}
		for _, r := range relations {
			if !exists(relation, r) {
				t.Errorf("%s: %s missing", when, r)
			}
		}
	}
	checkUp("after migrate")

	for i := 0; i < 2; i++ { // down twice: IF EXISTS guards
		if _, err := pool.Exec(ctx, sqlOf("061_codex_grounding.down.sql")); err != nil {
			t.Fatalf("down %d: %v", i+1, err)
		}
	}
	for _, r := range relations {
		if exists(relation, r) {
			t.Errorf("down left %s behind", r)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '061_codex_grounding'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("re-apply 061: %v", err)
	}
	checkUp("after re-apply")
	if _, err := pool.Exec(ctx, sqlOf("061_codex_grounding.up.sql")); err != nil {
		t.Fatalf("re-run up: %v", err)
	}
	checkUp("after re-run")
}
