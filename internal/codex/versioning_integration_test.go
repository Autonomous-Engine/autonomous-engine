package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests against a real PostgreSQL (TEST_POSTGRES_DSN; migrated,
// so they also prove migration 060 applies on top of 001–059).

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

// newProduct inserts a product with one repository on defaultBranch.
func newProduct(t *testing.T, pool *pgxpool.Pool, defaultBranch string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pid, rid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name, description) VALUES ($1, $2, 'p0')`, pid, "p0-"+pid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r', $3, $4)`, rid, pid, "o/r-"+rid.String(), defaultBranch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, pid)
	})
	return pid
}

func record(t *testing.T, cx *Codex, pid uuid.UUID, branch string, sections ...string) RecordedVersion {
	t.Helper()
	v, err := cx.RecordVersion(context.Background(), NewVersion{
		ProductID: pid, Branch: branch, SectionsChanged: sections,
		Summary: "test", Source: models.CodexChangeSourceAutomatedReanalysis, Trigger: TriggerFullIndex,
	})
	if err != nil {
		t.Fatalf("RecordVersion(%s): %v", branch, err)
	}
	return v
}

func TestRecordVersionNumbersPerProductAndBranch(t *testing.T) {
	pool := integrationPool(t)
	cx := New(pool)
	a, b := newProduct(t, pool, "main"), newProduct(t, pool, "master")

	got := []int{
		record(t, cx, a, "main", ModuleChange("x")).ProductVersion,
		record(t, cx, b, "master", ModuleChange("x")).ProductVersion,
		record(t, cx, a, "main", ModuleChange("y")).ProductVersion,
		record(t, cx, a, "develop", ModuleChange("y")).ProductVersion,
		record(t, cx, a, "main").ProductVersion,
	}
	if want := []int{1, 1, 2, 1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("product versions = %v, want %v", got, want)
	}

	// An empty branch resolves to the product's default branch.
	if v := record(t, cx, b, "", ModuleChange("z")); v.Branch != "master" || v.ProductVersion != 2 {
		t.Fatalf("default-branch write = %+v, want master v2", v)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		pid    uuid.UUID
		branch string
		want   int
	}{{a, "main", 3}, {a, "develop", 1}, {a, "nope", 0}, {b, "master", 2}} {
		cur, err := cx.GetCurrentVersion(ctx, tc.pid, tc.branch)
		if err != nil || cur != tc.want {
			t.Errorf("GetCurrentVersion(%s) = %d, %v; want %d", tc.branch, cur, err, tc.want)
		}
	}
	latest, err := cx.GetLatestVersion(ctx, a, "main")
	if err != nil || latest == nil || latest.ProductVersion != 3 || latest.Branch != "main" || latest.ProductID == nil || *latest.ProductID != a {
		t.Fatalf("GetLatestVersion = %+v, %v", latest, err)
	}
	if none, err := cx.GetLatestVersion(ctx, a, "nope"); err != nil || none != nil {
		t.Fatalf("GetLatestVersion(no versions) = %+v, %v; want nil, nil", none, err)
	}

	// No product: refused, by writers and readers alike.
	if _, err := cx.RecordVersion(ctx, NewVersion{Branch: "main", Source: models.CodexChangeSourceHumanUpdate}); !errors.Is(err, ErrNoProduct) {
		t.Errorf("RecordVersion without a product: %v, want ErrNoProduct", err)
	}
	if _, err := cx.GetCurrentVersion(ctx, uuid.Nil, "main"); !errors.Is(err, ErrNoProduct) {
		t.Errorf("GetCurrentVersion without a product: %v, want ErrNoProduct", err)
	}
	if _, err := cx.GetCurrentVersion(ctx, a, ""); err == nil {
		t.Error("GetCurrentVersion without a branch: no error")
	}
}

func TestRecordVersionConcurrentWritersGetDistinctNumbers(t *testing.T) {
	pool := integrationPool(t)
	cx := New(pool)
	pid := newProduct(t, pool, "main")

	const writers = 24
	var wg sync.WaitGroup
	got := make([]int, writers)
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, err := cx.RecordVersion(context.Background(), NewVersion{
				ProductID: pid, Branch: "main", SectionsChanged: []string{ModuleChange(fmt.Sprint(i))},
				Source: models.CodexChangeSourceAutomatedReanalysis, Trigger: TriggerIncrementalIndex,
			})
			got[i], errs[i] = v.ProductVersion, err
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	sort.Ints(got)
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("product versions = %v, want 1..%d with no gap or duplicate", got, writers)
		}
	}
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE topic = $1 AND aggregate_id = $2`, bus.TopicCodexIndexed, pid).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != writers {
		t.Fatalf("%d codex.indexed events for %d versions", events, writers)
	}
}

func TestRecordVersionTxEnqueuesCodexIndexedAtomically(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	nv := NewVersion{
		ProductID: pid, Branch: "main", SectionsChanged: []string{"b", ModuleChange("a"), "patterns"},
		Summary: "s", Source: models.CodexChangeSourceHumanUpdate, Trigger: TriggerInteractionAnswer,
	}

	// Rolled back: neither the version nor its event exists.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordVersionTx(ctx, tx, nv); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM codex_versions WHERE product_id = $1`, pid).Scan(&n)
	var e int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, pid).Scan(&e)
	if n != 0 || e != 0 {
		t.Fatalf("after rollback: %d versions, %d events", n, e)
	}

	// Committed: both, and the event describes the row.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := RecordVersionTx(ctx, tx, nv)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"module:a", "module:b", "patterns"}; rec.ProductVersion != 1 || !reflect.DeepEqual(rec.SectionsChanged, want) {
		t.Fatalf("recorded %+v, want v1 with %v", rec, want)
	}
	var topic, key, aggType string
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT topic, key, aggregate_type, payload FROM outbox_events WHERE aggregate_id = $1`, pid).Scan(&topic, &key, &aggType, &payload); err != nil {
		t.Fatal(err)
	}
	ev, err := DecodeIndexedEvent(payload)
	if err != nil {
		t.Fatal(err)
	}
	if topic != bus.TopicCodexIndexed || key != pid.String() || aggType != "product" ||
		ev.ProductVersion != 1 || ev.VersionNumber != rec.VersionNumber || ev.Branch != "main" ||
		ev.Trigger != TriggerInteractionAnswer || !reflect.DeepEqual(ev.SectionsChanged, rec.SectionsChanged) {
		t.Fatalf("event %s/%s/%s = %+v (recorded %+v)", topic, key, aggType, ev, rec)
	}
	var stored []string
	if err := pool.QueryRow(ctx, `SELECT sections_changed FROM codex_versions WHERE version_number = $1`, rec.VersionNumber).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, rec.SectionsChanged) {
		t.Fatalf("stored sections %v", stored)
	}
}

func TestChangedSectionsAndStalenessAreScopedToProductAndBranch(t *testing.T) {
	pool := integrationPool(t)
	cx := New(pool)
	ctx := context.Background()
	a, b := newProduct(t, pool, "main"), newProduct(t, pool, "main")

	record(t, cx, a, "main", ModuleChange("pinned"))           // v1: the pin
	record(t, cx, a, "main", ModuleChange("other"))            // v2
	record(t, cx, a, "develop", ModuleChange("pinned"))        // other branch
	record(t, cx, b, "main", ModuleChange("pinned"))           // other product
	record(t, cx, a, "main", "legacy", SectionChange("zones")) // v3

	got, err := cx.GetChangedSectionsBetween(ctx, a, "main", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"module:legacy", "module:other", "section:zones"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changed v2..v3 = %v, want %v", got, want)
	}

	for _, tc := range []struct {
		name   string
		pinned int
		deps   []string
		want   bool
	}{
		{"pinned module unchanged on this product+branch since v1", 1, []string{"pinned"}, false},
		{"intersecting module", 1, []string{"other"}, true},
		{"legacy un-prefixed value counts as a module", 1, []string{ModuleChange("legacy")}, true},
		{"section dependency", 1, []string{SectionChange("zones")}, true},
		{"up to date", 3, []string{"other"}, false},
	} {
		stale, err := cx.IsWorkItemStale(ctx, a, "main", tc.pinned, tc.deps)
		if err != nil || stale != tc.want {
			t.Errorf("%s: stale = %v, %v; want %v", tc.name, stale, err, tc.want)
		}
	}
}

func TestCodexPins(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	parent, child, other := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'research', 'p', 'd', $2)`, parent, pid)
	exec(`INSERT INTO work_items (id, type, state, title, description, product_id, parent_id) VALUES ($1, 'subtask', 'implementation', 'c', 'd', $2, $3)`, child, pid, parent)
	exec(`INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'review', 'o', 'd', $2)`, other, pid)

	store := NewPinStore(pool)
	if _, found, err := store.Get(ctx, parent); err != nil || found {
		t.Fatalf("Get before pinning = %v, %v", found, err)
	}
	inTx := func(fn func(tx pgx.Tx) error) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pin := Pin{WorkItemID: parent, ProductID: pid, Branch: "main", ProductVersion: 4, Modules: []string{"b", "a", "b"}}
	inTx(func(tx pgx.Tx) error { return store.PinTx(ctx, tx, pin) })
	// A second brief (research re-run) replaces the pin.
	pin.ProductVersion = 5
	inTx(func(tx pgx.Tx) error { return store.PinTx(ctx, tx, pin) })
	inTx(func(tx pgx.Tx) error {
		return store.PinTx(ctx, tx, Pin{WorkItemID: other, ProductID: pid, Branch: "main", ProductVersion: 5})
	})

	got, found, err := store.Get(ctx, parent)
	if err != nil || !found || got.ProductVersion != 5 || got.Branch != "main" || got.ProductID != pid || !reflect.DeepEqual(got.Modules, []string{"a", "b"}) {
		t.Fatalf("Get = %+v, %v, %v", got, found, err)
	}
	var display int
	_ = pool.QueryRow(ctx, `SELECT codex_version FROM work_items WHERE id = $1`, parent).Scan(&display)
	if display != 5 {
		t.Fatalf("work_items.codex_version = %d, want the pinned product_version 5", display)
	}

	var copied Pin
	var ok bool
	inTx(func(tx pgx.Tx) error {
		var err error
		copied, ok, err = store.CopyToChildTx(ctx, tx, parent, child)
		return err
	})
	if !ok || copied.WorkItemID != child || copied.ProductVersion != 5 || !reflect.DeepEqual(copied.Modules, []string{"a", "b"}) {
		t.Fatalf("CopyToChildTx = %+v, %v", copied, ok)
	}
	_ = pool.QueryRow(ctx, `SELECT codex_version FROM work_items WHERE id = $1`, child).Scan(&display)
	if display != 5 {
		t.Fatalf("subtask codex_version = %d, want 5", display)
	}
	inTx(func(tx pgx.Tx) error {
		_, ok, err := store.CopyToChildTx(ctx, tx, uuid.New(), uuid.New())
		if ok {
			return errors.New("copied from an unpinned parent")
		}
		return err
	})

	items, err := store.ListPinned(ctx, pid, "main", []models.WorkItemState{models.WorkItemStateResearch, models.WorkItemStateImplementation})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.WorkItemID.String()+"/"+string(it.State))
		if it.WorkItemID == child && (it.ParentID == nil || *it.ParentID != parent) {
			t.Errorf("child listed without its parent: %+v", it)
		}
	}
	sort.Strings(ids)
	want := []string{child.String() + "/implementation", parent.String() + "/research"}
	sort.Strings(want)
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ListPinned = %v, want %v (the review item excluded)", ids, want)
	}
	if none, err := store.ListPinned(ctx, pid, "develop", nil); err != nil || len(none) != 0 {
		t.Fatalf("ListPinned(other branch) = %v, %v", none, err)
	}

	// Deleting the work item deletes its pin.
	exec(`DELETE FROM work_items WHERE id = ANY($1)`, []uuid.UUID{child, parent, other})
	if _, found, _ := store.Get(ctx, parent); found {
		t.Fatal("pin survived its work item")
	}

	// Validation.
	for name, bad := range map[string]Pin{
		"no item":    {ProductID: pid, Branch: "main"},
		"no product": {WorkItemID: parent, Branch: "main"},
		"no branch":  {WorkItemID: parent, ProductID: pid},
		"negative":   {WorkItemID: parent, ProductID: pid, Branch: "main", ProductVersion: -1},
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PinTx(ctx, tx, bad); err == nil {
			t.Errorf("%s: pinned", name)
		}
		_ = tx.Rollback(ctx)
	}
}

// Migration 060 down and up again on a database of its own (other packages'
// tests share TEST_POSTGRES_DSN's database and must not see the table go):
// the down removes everything 060 added, and the up numbers existing rows by
// ROW_NUMBER over (product_id, branch) in version_number order.
func TestMigration060UpDownUp(t *testing.T) {
	base := integrationPool(t)
	ctx := context.Background()
	name := "ae_m060_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	exists := func(q string) bool {
		var ok bool
		if err := pool.QueryRow(ctx, q).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	hasColumn := `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'codex_versions' AND column_name = 'product_version')`
	hasTable := `SELECT to_regclass('work_item_codex_pins') IS NOT NULL`

	if _, err := pool.Exec(ctx, sqlOf("060_codex_coherence.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if exists(hasColumn) || exists(hasTable) {
		t.Fatal("down left product_version or work_item_codex_pins behind")
	}
	// Down twice is harmless (IF EXISTS guards).
	if _, err := pool.Exec(ctx, sqlOf("060_codex_coherence.down.sql")); err != nil {
		t.Fatalf("second down: %v", err)
	}

	// Pre-060 rows: two products, two branches, interleaved, one legacy row
	// without a product.
	a, b := uuid.New(), uuid.New()
	for _, p := range []uuid.UUID{a, b} {
		if _, err := pool.Exec(ctx, `INSERT INTO products (id, name, description) VALUES ($1, $2, 'm060')`, p, "m060-"+p.String()); err != nil {
			t.Fatal(err)
		}
	}
	type row struct {
		pid    *uuid.UUID
		branch string
	}
	rows := []row{{&a, "main"}, {&b, "main"}, {&a, "dev"}, {nil, "main"}, {&a, "main"}, {&b, "main"}, {&a, "main"}}
	var ids []uuid.UUID
	for _, r := range rows {
		id := uuid.New()
		ids = append(ids, id)
		if _, err := pool.Exec(ctx, `INSERT INTO codex_versions (id, product_id, branch, change_source) VALUES ($1, $2, $3, 'onboarding')`, id, r.pid, r.branch); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '060_codex_coherence'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("re-apply 060: %v", err)
	}
	if !exists(hasColumn) || !exists(hasTable) {
		t.Fatal("up did not restore product_version and work_item_codex_pins")
	}
	want := []any{1, 1, 1, nil, 2, 2, 3}
	for i, id := range ids {
		var pv *int
		if err := pool.QueryRow(ctx, `SELECT product_version FROM codex_versions WHERE id = $1`, id).Scan(&pv); err != nil {
			t.Fatal(err)
		}
		var got any
		if pv != nil {
			got = *pv
		}
		if got != want[i] {
			t.Errorf("row %d (%v/%s): product_version = %v, want %v", i, rows[i].pid, rows[i].branch, got, want[i])
		}
	}
	// The writer continues the backfilled sequence.
	v, err := New(pool).RecordVersion(ctx, NewVersion{ProductID: a, Branch: "main", Source: models.CodexChangeSourceOnboarding, Trigger: TriggerOnboardingApproval})
	if err != nil || v.ProductVersion != 4 {
		t.Fatalf("next version after backfill = %+v, %v; want 4", v, err)
	}
	// The CHECK rejects a product row without a number.
	_, err = pool.Exec(ctx, `INSERT INTO codex_versions (id, product_id, branch, change_source) VALUES ($1, $2, 'main', 'onboarding')`, uuid.New(), a)
	if err == nil {
		t.Fatal("a product row without product_version was accepted")
	}
	// The up is re-runnable (guards) on an already-migrated schema.
	if _, err := pool.Exec(ctx, sqlOf("060_codex_coherence.up.sql")); err != nil {
		t.Fatalf("re-run up: %v", err)
	}
}
