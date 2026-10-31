package zones

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests against a real PostgreSQL (TEST_POSTGRES_DSN; migrated,
// so they also prove migration 063 applies on top of 001–062).

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

// newRepo inserts a product with one repository on branch.
func newRepo(t *testing.T, pool *pgxpool.Pool, branch string) (productID, repoID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pid, rid := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name, description) VALUES ($1, $2, 'p2')`, pid, "p2-"+pid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r', $3, $4)`, rid, pid, "o/r-"+rid.String(), branch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, pid)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, pid)
	})
	return pid, rid
}

// zoneVersions are the codex versions recorded for product on branch whose
// sections include section:zones.
func zoneVersions(t *testing.T, pool *pgxpool.Pool, pid uuid.UUID, branch string) []int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT product_version FROM codex_versions
		 WHERE product_id = $1 AND branch = $2 AND $3 = ANY(sections_changed)
		 ORDER BY product_version`, pid, branch, codex.SectionChange("zones"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestStorePerRepositoryUniqueness(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := NewStore(pool)
	_, repoA := newRepo(t, pool, "main")
	_, repoB := newRepo(t, pool, "master")
	tpl := Template{Name: "payments", Patterns: []string{"payments/**"}, Policy: PolicyHumanReview, Source: SourceDashboard}

	if _, err := s.Create(ctx, repoA, tpl); err != nil {
		t.Fatalf("create in A: %v", err)
	}
	if _, err := s.Create(ctx, repoB, tpl); err != nil {
		t.Fatalf("same name in another repository must be allowed: %v", err)
	}
	if _, err := s.Create(ctx, repoA, tpl); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate in one repository = %v, want ErrDuplicate", err)
	}
	// The database enforces it too, not only the store.
	_, err := pool.Exec(ctx, `INSERT INTO code_zones (repository_id, name, patterns) VALUES ($1, 'payments', '{x}')`, repoA)
	if err == nil || !strings.Contains(err.Error(), "code_zones_repository_name_key") {
		t.Fatalf("raw duplicate insert = %v, want code_zones_repository_name_key violation", err)
	}
}

func TestStoreWritesRecordZonesVersion(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := NewStore(pool)
	pid, rid := newRepo(t, pool, "develop")

	z, err := s.Create(ctx, rid, Template{Name: "db", Description: "database", Patterns: []string{" internal/db/** ", "migrations/**"}, Policy: PolicyEscalate, Source: SourceDashboard})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(z.Patterns, []string{"internal/db/**", "migrations/**"}) || !reflect.DeepEqual(z.ModulePaths, []string{"internal/db", "migrations"}) {
		t.Fatalf("created = %+v", z)
	}
	if z.Status != models.ZoneStatusActive || z.Source != SourceDashboard || z.RepositoryID != rid {
		t.Fatalf("created = %+v", z)
	}
	if got := zoneVersions(t, pool, pid, "develop"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("versions after create = %v", got)
	}

	policy := PolicyHumanReview
	desc := "db layer"
	z, err = s.Update(ctx, z.ID, Update{Patterns: []string{"internal/db/**"}, Policy: &policy, Description: &desc})
	if err != nil {
		t.Fatal(err)
	}
	if z.Policy != PolicyHumanReview || z.Description != "db layer" || !reflect.DeepEqual(z.ModulePaths, []string{"internal/db"}) {
		t.Fatalf("updated = %+v", z)
	}
	if _, err := s.Update(ctx, z.ID, Update{Patterns: []string{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty patterns = %v, want ErrInvalid", err)
	}

	z, err = s.Freeze(ctx, z.ID, "release freeze")
	if err != nil {
		t.Fatal(err)
	}
	if !z.Frozen() || z.FrozenReason == nil || *z.FrozenReason != "release freeze" || z.FrozenAt == nil {
		t.Fatalf("frozen = %+v", z)
	}
	if _, err := s.Freeze(ctx, z.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("freeze without reason = %v, want ErrInvalid", err)
	}
	z, err = s.Thaw(ctx, z.ID)
	if err != nil {
		t.Fatal(err)
	}
	if z.Frozen() || z.FrozenReason != nil || z.FrozenAt != nil {
		t.Fatalf("thawed = %+v", z)
	}
	if err := s.Delete(ctx, z.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, z.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted = %v", err)
	}
	// create, update, freeze, thaw, delete: five versions, numbered for
	// this product and the repository's branch; the failed writes none.
	if got := zoneVersions(t, pool, pid, "develop"); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("versions = %v", got)
	}
	// Each version announces itself on codex.indexed through the outbox.
	var events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = 'codex.indexed'`, pid).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 5 {
		t.Fatalf("%d codex.indexed events, want 5", events)
	}
}

func TestStoreApplyDefaultsOnlyOnce(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := NewStore(pool)
	pid, rid := newRepo(t, pool, "main")
	defaults, err := DefaultTemplates()
	if err != nil {
		t.Fatal(err)
	}
	risk := RiskZoneTemplates("high-risk: internal/billing", nil)
	n, err := s.ApplyIfEmpty(ctx, rid, append(defaults, risk...), "onboarding approval")
	if err != nil || n != 3 {
		t.Fatalf("apply = %d, %v; want 3 (guarded, restricted, one risk zone)", n, err)
	}
	n, err = s.ApplyIfEmpty(ctx, rid, defaults, "again")
	if err != nil || n != 0 {
		t.Fatalf("second apply = %d, %v; want 0 (repository already has zones)", n, err)
	}
	zs, err := s.ListForRepository(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Policy{}
	for _, z := range zs {
		got[z.Name] = z.Policy
	}
	want := map[string]Policy{"guarded": PolicyHumanReview, "internal/billing": PolicyHumanReview, "restricted": PolicyEscalate}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zones = %v", got)
	}
	if got := zoneVersions(t, pool, pid, "main"); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("versions = %v (one per apply that wrote)", got)
	}
	byProduct, err := s.ListForProduct(ctx, pid)
	if err != nil || len(byProduct) != 3 {
		t.Fatalf("by product = %d, %v", len(byProduct), err)
	}

	// The repository now has zones: an onboarding confirmation may only add
	// new ones. It cannot drop guarded, weaken restricted, or touch the
	// dashboard zone; the new "ledger" zone is added.
	if _, err := s.Create(ctx, rid, Template{Name: "mine", Patterns: []string{"x/**"}, Policy: PolicyAutonomous, Source: SourceDashboard}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListForRepository(ctx, rid)
	res, err := s.ConfirmOnboarding(ctx, rid, []Template{
		{Name: "restricted", Patterns: []string{"nothing/**"}, Policy: PolicyAutonomous, Source: SourceConfig},
		{Name: "mine", Patterns: []string{"y/**"}, Policy: PolicyEscalate, Source: SourceOnboarding},
		{Name: "ledger", Patterns: []string{"ledger/**"}, Policy: PolicyEscalate, Source: SourceOnboarding},
	}, "wizard")
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstTime || !reflect.DeepEqual(res.Added, []string{"ledger"}) || !reflect.DeepEqual(res.Kept, []string{"mine", "restricted"}) {
		t.Fatalf("result = %+v", res)
	}
	after, _ := s.ListForRepository(ctx, rid)
	if len(after) != len(before)+1 {
		t.Fatalf("zones %d → %d, want one added and none removed", len(before), len(after))
	}
	for _, b := range before {
		for _, a := range after {
			if a.ID == b.ID && (a.Policy != b.Policy || !reflect.DeepEqual(a.Patterns, b.Patterns) || a.Source != b.Source || a.Status != b.Status) {
				t.Errorf("zone %s changed: %+v → %+v", b.Name, b, a)
			}
		}
	}
}

// The review's reproduction: a dashboard zone (escalate, frozen) and a
// default restricted zone; an anonymous onboarding POST asking for
// payments autonomous (and nothing else) changes neither row.
func TestConfirmOnboardingNeverWeakens(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := NewStore(pool)
	_, rid := newRepo(t, pool, "main")
	if _, err := s.ApplyIfEmpty(ctx, rid, []Template{{Name: "restricted", Patterns: []string{"**/secrets/**"}, Policy: PolicyEscalate, Source: SourceConfig}}, "seed"); err != nil {
		t.Fatal(err)
	}
	pay, err := s.Create(ctx, rid, Template{Name: "payments", Patterns: []string{"payments/**"}, Policy: PolicyEscalate, Source: SourceDashboard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Freeze(ctx, pay.ID, "audit"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListForRepository(ctx, rid)
	res, err := s.ConfirmOnboarding(ctx, rid, []Template{{Name: "payments", Patterns: []string{"payments/**"}, Policy: PolicyAutonomous, Source: SourceOnboarding}}, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 {
		t.Fatalf("added %v", res.Added)
	}
	after, _ := s.ListForRepository(ctx, rid)
	if !reflect.DeepEqual(zoneState(before), zoneState(after)) {
		t.Fatalf("zones changed:\n before %v\n after  %v", zoneState(before), zoneState(after))
	}
}

// First-time confirmation writes exactly the chosen zones; an explicit
// empty choice is recorded, so neither approval seeding nor a later
// confirmation re-applies the defaults behind the operator's back.
func TestConfirmOnboardingFirstTimeAndNoZonesChoice(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := NewStore(pool)
	_, rid := newRepo(t, pool, "main")
	res, err := s.ConfirmOnboarding(ctx, rid, []Template{
		{Name: "restricted", Patterns: []string{"**/secrets/**"}, Policy: PolicyEscalate, Source: SourceConfig},
		{Name: "billing", Patterns: []string{"billing/**"}, Policy: PolicyHumanReview, Source: SourceOnboarding},
	}, "wizard")
	if err != nil || !res.FirstTime || len(res.Zones) != 2 {
		t.Fatalf("first confirm = %+v, %v", res, err)
	}

	_, empty := newRepo(t, pool, "main")
	res, err = s.ConfirmOnboarding(ctx, empty, nil, "wizard: no zones")
	if err != nil || !res.FirstTime || len(res.Zones) != 0 {
		t.Fatalf("empty confirm = %+v, %v", res, err)
	}
	if ok, err := s.Confirmed(ctx, empty); err != nil || !ok {
		t.Fatalf("confirmed = %v, %v", ok, err)
	}
	defaults, _ := DefaultTemplates()
	if n, err := s.ApplyIfEmpty(ctx, empty, defaults, "approval"); err != nil || n != 0 {
		t.Fatalf("approval re-applied %d default zone(s) after an explicit no-zones choice (%v)", n, err)
	}
	// Later confirmations on a confirmed repository only add.
	res, err = s.ConfirmOnboarding(ctx, empty, []Template{{Name: "new", Patterns: []string{"n/**"}, Policy: PolicyEscalate, Source: SourceOnboarding}}, "again")
	if err != nil || res.FirstTime || !reflect.DeepEqual(res.Added, []string{"new"}) {
		t.Fatalf("add after empty = %+v, %v", res, err)
	}
}

// Concurrent partial edits of one zone (patterns by one caller, policy by
// another) both survive: Update reads the row FOR UPDATE in its
// transaction instead of before it.
func TestUpdateConcurrentPartialEditsDoNotLoseWrites(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := NewStore(pool)
	_, rid := newRepo(t, pool, "main")
	for i := 0; i < 15; i++ {
		z, err := s.Create(ctx, rid, Template{Name: "z" + strconv.Itoa(i), Patterns: []string{"a/**"}, Policy: PolicyAutonomous, Source: SourceDashboard})
		if err != nil {
			t.Fatal(err)
		}
		pol := PolicyEscalate
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.Update(ctx, z.ID, Update{Patterns: []string{"b/**"}}) }()
		go func() { defer wg.Done(); _, _ = s.Update(ctx, z.ID, Update{Policy: &pol}) }()
		wg.Wait()
		got, err := s.Get(ctx, z.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Policy != PolicyEscalate || !reflect.DeepEqual(got.Patterns, []string{"b/**"}) {
			t.Fatalf("iteration %d: lost an edit: policy %s patterns %v", i, got.Policy, got.Patterns)
		}
	}
}

func zoneState(zs []Zone) []string {
	var out []string
	for _, z := range zs {
		reason := ""
		if z.FrozenReason != nil {
			reason = *z.FrozenReason
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%v|%s", z.Name, z.Policy, z.Status, z.Source, z.Patterns, reason))
	}
	sort.Strings(out)
	return out
}

func TestMigration063UpDownUp(t *testing.T) {
	base := integrationPool(t)
	ctx := context.Background()
	name := "ae_m063_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
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
	hasColumn := func() bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'code_zones' AND column_name = 'policy')`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// Rows written under 063: the same name in two repositories.
	pid := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, 'm063')`, pid); err != nil {
		t.Fatal(err)
	}
	r1, r2 := uuid.New(), uuid.New()
	for i, r := range []uuid.UUID{r1, r2} {
		if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, 'r', $3, 'main')`, r, pid, "o/r"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO code_zones (repository_id, name, patterns, policy) VALUES ($1, 'core', '{core/**}', 'escalate')`, r); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pool.Exec(ctx, sqlOf("063_code_zones_per_repository.down.sql")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if hasColumn() {
		t.Fatal("down left code_zones.policy behind")
	}
	if _, err := pool.Exec(ctx, sqlOf("063_code_zones_per_repository.down.sql")); err != nil {
		t.Fatalf("second down: %v", err)
	}
	var zoneCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM code_zones`).Scan(&zoneCount); err != nil || zoneCount != 2 {
		t.Fatalf("down lost zones: %d, %v", zoneCount, err)
	}
	// Pre-063: the global UNIQUE(name) is back.
	if _, err := pool.Exec(ctx, `INSERT INTO code_zones (repository_id, name) VALUES ($1, 'core')`, r1); err == nil {
		t.Fatal("down did not restore UNIQUE(name)")
	}

	// A pre-063 row written the old way: module paths only.
	legacy := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO code_zones (id, repository_id, name, module_paths, status, frozen_reason) VALUES ($1, $2, 'legacy', '{internal/db,/pkg/api/}', 'frozen', 'old freeze')`, legacy, r1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '063_code_zones_per_repository'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("re-apply 063: %v", err)
	}
	if !hasColumn() {
		t.Fatal("up did not restore code_zones.policy")
	}
	z, err := NewStore(pool).Get(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(z.Patterns, []string{"internal/db", "internal/db/**", "pkg/api", "pkg/api/**"}) || z.Policy != PolicyHumanReview || !z.Frozen() || z.Source != SourceConfig {
		t.Fatalf("legacy row after up = %+v", z)
	}
	// Per-repository uniqueness again: the renamed duplicate can take its
	// name back in its own repository.
	if _, err := pool.Exec(ctx, `UPDATE code_zones SET name = 'core' WHERE repository_id = $1 AND name LIKE 'core (%'`, r2); err != nil {
		t.Fatalf("rename back: %v", err)
	}
	if _, err := pool.Exec(ctx, sqlOf("063_code_zones_per_repository.up.sql")); err != nil {
		t.Fatalf("re-run up: %v", err)
	}
}
