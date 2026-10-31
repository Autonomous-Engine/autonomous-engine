package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests against a real PostgreSQL (TEST_POSTGRES_DSN; migrated,
// so they also prove migration 050 applies).

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
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

type fixture struct {
	product, repo, repo2, parent, child uuid.UUID
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{product: uuid.New(), repo: uuid.New(), repo2: uuid.New(), parent: uuid.New(), child: uuid.New()}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO products (id, name, description) VALUES ($1, $2, 'm201')`, f.product, "m201-"+f.product.String())
	exec(`INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r1', $3, 'main')`, f.repo, f.product, "o/r1-"+f.repo.String())
	exec(`INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r2', $3, 'main')`, f.repo2, f.product, "o/r2-"+f.repo2.String())
	exec(`INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'intake', 'm201 parent', 'd', $2)`, f.parent, f.product)
	exec(`INSERT INTO work_items (id, type, state, title, description, product_id, parent_id, repository_id) VALUES ($1, 'subtask', 'implementation', 'm201 child', 'd', $2, $3, $4)`, f.child, f.product, f.parent, f.repo)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = ANY($1)`, []uuid.UUID{f.child, f.parent})
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, f.product)
	})
	return f
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func TestIntegration_PinTxCreatesKeepsAndDetectsDrift(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	s := NewStore(pool)
	ctx := context.Background()
	pin := Pin{WorkItemID: f.parent, RepositoryID: f.repo, BaseRef: "main", BaseSHA: shaA}

	var res PinResult
	if err := inTx(t, pool, func(tx pgx.Tx) (err error) { res, err = s.PinTx(ctx, tx, pin, "test fetch"); return }); err != nil {
		t.Fatal(err)
	}
	if res != PinCreated {
		t.Fatalf("first pin: %s, want created", res)
	}
	if err := inTx(t, pool, func(tx pgx.Tx) (err error) { res, err = s.PinTx(ctx, tx, pin, "again"); return }); err != nil {
		t.Fatal(err)
	}
	if res != PinUnchanged {
		t.Fatalf("same sha: %s, want unchanged", res)
	}
	moved := pin
	moved.BaseSHA = shaB
	if err := inTx(t, pool, func(tx pgx.Tx) (err error) { res, err = s.PinTx(ctx, tx, moved, "re-intake"); return }); err != nil {
		t.Fatal(err)
	}
	if res != PinDrifted {
		t.Fatalf("moved sha: %s, want drifted", res)
	}

	got, found, err := s.Get(ctx, f.parent, f.repo)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.BaseSHA != shaA || got.BaseRef != "main" {
		t.Fatalf("pin after drift = %+v, want the original %s kept", got, shaA)
	}
	evs, err := s.Events(ctx, f.parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Kind != EventResolved || evs[0].NewSHA != shaA || evs[0].Detail != "test fetch" ||
		evs[1].Kind != EventDriftDetected || evs[1].OldSHA != shaA || evs[1].NewSHA != shaB {
		t.Fatalf("events = %+v", evs)
	}
	if _, found, _ := s.Get(ctx, f.parent, f.repo2); found {
		t.Fatal("unpinned repository reported as pinned")
	}
}

func TestIntegration_CopyToChildAndGetAll(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	s := NewStore(pool)
	ctx := context.Background()
	if err := inTx(t, pool, func(tx pgx.Tx) error {
		return s.UpsertTx(ctx, tx,
			Pin{WorkItemID: f.parent, RepositoryID: f.repo, BaseRef: "main", BaseSHA: shaA},
			Pin{WorkItemID: f.parent, RepositoryID: f.repo2, BaseRef: "develop", BaseSHA: shaB})
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.GetAll(ctx, f.parent)
	if err != nil || len(all) != 2 {
		t.Fatalf("GetAll = %+v, %v", all, err)
	}

	var pin Pin
	var copied bool
	if err := inTx(t, pool, func(tx pgx.Tx) (err error) {
		pin, copied, err = NewStore(nil).CopyToChildTx(ctx, tx, f.parent, f.child, f.repo)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if !copied || pin.BaseSHA != shaA || pin.BaseRef != "main" || pin.WorkItemID != f.child {
		t.Fatalf("copy = %+v copied=%v", pin, copied)
	}
	childPins, _ := s.GetAll(ctx, f.child)
	if len(childPins) != 1 || childPins[0].RepositoryID != f.repo {
		t.Fatalf("child pins = %+v, want only repo %s", childPins, f.repo)
	}
	evs, _ := s.Events(ctx, f.child)
	if len(evs) != 1 || evs[0].Kind != EventResolved || !strings.Contains(evs[0].Detail, f.parent.String()) {
		t.Fatalf("child events = %+v", evs)
	}

	// A parent with no pin for the repository: nothing copied, nothing written.
	other := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', 'intake', 'm201 bare', 'd')`, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, other) })
	if err := inTx(t, pool, func(tx pgx.Tx) (err error) {
		_, copied, err = s.CopyToChildTx(ctx, tx, other, f.child, f.repo2)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if copied {
		t.Fatal("copied a pin from a parent that has none")
	}

	// Upsert replaces (the M2-03 repin primitive) and records nothing itself.
	if err := inTx(t, pool, func(tx pgx.Tx) error {
		return s.UpsertTx(ctx, tx, Pin{WorkItemID: f.parent, RepositoryID: f.repo, BaseRef: "main", BaseSHA: shaB})
	}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.Get(ctx, f.parent, f.repo); p.BaseSHA != shaB {
		t.Fatalf("upsert did not replace: %+v", p)
	}
}

func TestIntegration_SchemaRejectsAbbreviatedSHAAndCascades(t *testing.T) {
	pool := integrationPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO work_item_base_shas (work_item_id, repository_id, base_ref, base_sha) VALUES ($1, $2, 'main', 'abc123')`, f.parent, f.repo)
	if !IsCheckViolation(err) {
		t.Fatalf("abbreviated sha: err = %v, want a CHECK violation", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO base_sha_events (work_item_id, repository_id, kind) VALUES ($1, $2, 'moved')`, f.parent, f.repo)
	if !IsCheckViolation(err) {
		t.Fatalf("unknown kind: err = %v, want a CHECK violation", err)
	}

	s := NewStore(pool)
	if err := inTx(t, pool, func(tx pgx.Tx) error {
		_, err := s.PinTx(ctx, tx, Pin{WorkItemID: f.parent, RepositoryID: f.repo, BaseRef: "main", BaseSHA: shaA}, "x")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Deleting the repository removes its pins and events (ON DELETE CASCADE),
	// and so does deleting the work item.
	if _, err := pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, f.child); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, f.repo); err != nil {
		t.Fatalf("delete pinned repository: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM work_item_base_shas WHERE work_item_id = $1) + (SELECT count(*) FROM base_sha_events WHERE work_item_id = $1)`, f.parent).Scan(&n)
	if n != 0 {
		t.Fatalf("%d pin/event rows survived their repository", n)
	}
}

// TestMigration050_FollowsCLAUDEmdRules checks the migration text without a
// database: guarded DDL, ON DELETE on every foreign key, named constraints
// dropped before they are added, and a down migration.
func TestMigration050_FollowsCLAUDEmdRules(t *testing.T) {
	read := func(suffix string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", "050_work_item_base_shas."+suffix))
		if err != nil {
			t.Fatalf("read migration 050 %s: %v", suffix, err)
		}
		return string(b)
	}
	up, down := read("up.sql"), read("down.sql")
	for _, frag := range []string{
		"CREATE TABLE IF NOT EXISTS work_item_base_shas",
		"CREATE TABLE IF NOT EXISTS base_sha_events",
		"PRIMARY KEY (work_item_id, repository_id)",
		"CHECK (base_sha ~ '^[0-9a-f]{40}$')",
		"CHECK (kind IN ('resolved', 'drift_detected', 'repinned'))",
		"CREATE INDEX IF NOT EXISTS idx_base_sha_events_work_item_created",
		"ON base_sha_events (work_item_id, created_at)",
	} {
		if !strings.Contains(up, frag) {
			t.Errorf("up migration missing %q", frag)
		}
	}
	refs := regexp.MustCompile(`REFERENCES\s+\w+\(\w+\)([^;]*);`).FindAllStringSubmatch(up, -1)
	if len(refs) != 4 {
		t.Fatalf("found %d foreign keys, want 4", len(refs))
	}
	for _, m := range refs {
		if !strings.Contains(m[1], "ON DELETE CASCADE") {
			t.Errorf("foreign key without ON DELETE CASCADE: %s", m[0])
		}
	}
	for _, m := range regexp.MustCompile(`ADD CONSTRAINT (\w+)`).FindAllStringSubmatch(up, -1) {
		if !strings.Contains(up, "DROP CONSTRAINT IF EXISTS "+m[1]+";") {
			t.Errorf("constraint %s added without a preceding DROP CONSTRAINT IF EXISTS", m[1])
		}
	}
	for _, frag := range []string{"DROP TABLE IF EXISTS base_sha_events", "DROP TABLE IF EXISTS work_item_base_shas", "DROP INDEX IF EXISTS idx_base_sha_events_work_item_created"} {
		if !strings.Contains(down, frag) {
			t.Errorf("down migration missing %q", frag)
		}
	}
}
