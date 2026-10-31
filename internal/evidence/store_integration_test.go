package evidence_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationDSN returns the test database DSN: TEST_POSTGRES_DSN (the repo
// convention, see internal/interactions) first, then TEST_DATABASE_URL.
func integrationDSN() string {
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("TEST_DATABASE_URL")
}

// Integration coverage for the store against a real PostgreSQL. Gated on
// TEST_POSTGRES_DSN / TEST_DATABASE_URL; skips cleanly when both are unset.
// Runs the repository's migrations first so 044 is guaranteed to be present.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := integrationDSN()
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN / TEST_DATABASE_URL not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	return p
}

type fixture struct {
	productID    uuid.UUID
	repositoryID uuid.UUID
	workItemID   uuid.UUID
}

// insertFixture creates a product, repository, and work item. Cleanup deletes
// the work item explicitly (work_items.product_id is ON DELETE SET NULL, so
// the product delete alone would orphan it) and then the product, which
// cascades to the repository. implementation_verifications rows cascade from
// the work item delete.
func insertFixture(t *testing.T, p *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{productID: uuid.New(), repositoryID: uuid.New(), workItemID: uuid.New()}
	if _, err := p.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`,
		f.productID, "evidence-fixture-"+f.productID.String()[:8]); err != nil {
		t.Fatalf("fixture product: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch) VALUES ($1, $2, $3, $4, 'main')`,
		f.repositoryID, f.productID, "fixture-repo", "https://github.com/example/fixture"); err != nil {
		t.Fatalf("fixture repository: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO work_items (id, product_id, repository_id, title, state, type, priority)
		VALUES ($1, $2, $3, 'evidence fixture', 'intake', 'feature', 'medium')`,
		f.workItemID, f.productID, f.repositoryID); err != nil {
		t.Fatalf("fixture work_item: %v", err)
	}
	dsn := integrationDSN()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return
		}
		defer pool.Close()
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, f.workItemID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, f.productID)
	})
	return f
}

func newEvidence(f fixture, headSHA string, at time.Time) *evidence.ImplementationEvidence {
	repo := f.repositoryID.String()
	exit := 0
	return &evidence.ImplementationEvidence{
		SchemaVersion: evidence.SchemaVersion,
		WorkID:        f.workItemID.String(),
		TaskID:        uuid.New().String(),
		RepositoryID:  &repo,
		BaseSHA:       "0de2b26812bfc966ac61835e1b6aae2f3282cfcd",
		HeadSHA:       headSHA,
		Branch:        "agent/work/fixture",
		ChangedFiles:  []string{"internal/foo/foo.go"},
		Checks: []evidence.CheckResult{
			{Kind: evidence.CheckKindTest, Command: "go test ./...", ExitCode: &exit, Status: evidence.CheckPassed, OutputRef: "artifact://out/1"},
		},
		Outcome:      evidence.OutcomeVerified,
		StartedAt:    at,
		FinishedAt:   at.Add(time.Minute),
		ArtifactRefs: []string{},
	}
}

func TestStoreIntegration_InsertAndGet(t *testing.T) {
	p := integrationPool(t)
	defer p.Close()
	store := evidence.NewStore(p)
	ctx := context.Background()
	f := insertFixture(t, p)

	first := newEvidence(f, "aaaa000000000000000000000000000000000001", time.Now().UTC().Add(-time.Hour))
	second := newEvidence(f, "bbbb000000000000000000000000000000000002", time.Now().UTC())

	firstID, err := store.Insert(ctx, first)
	if err != nil {
		t.Fatalf("Insert first: %v", err)
	}
	secondID, err := store.Insert(ctx, second)
	if err != nil {
		t.Fatalf("Insert second: %v", err)
	}

	got, err := store.GetByWorkItem(ctx, f.workItemID)
	if err != nil {
		t.Fatalf("GetByWorkItem: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetByWorkItem returned %d rows, want 2", len(got))
	}
	if got[0].ID != secondID || got[1].ID != firstID {
		t.Errorf("rows not newest-first: %v, %v (want %v, %v)", got[0].ID, got[1].ID, secondID, firstID)
	}
	if got[0].Evidence.HeadSHA != second.HeadSHA || got[0].Evidence.Outcome != evidence.OutcomeVerified {
		t.Errorf("evidence not round-tripped: %+v", got[0].Evidence)
	}
	if len(got[0].Evidence.Checks) != 1 || got[0].Evidence.Checks[0].Status != evidence.CheckPassed {
		t.Errorf("checks not round-tripped: %+v", got[0].Evidence.Checks)
	}

	latest, err := store.GetLatestByHeadSHA(ctx, first.HeadSHA)
	if err != nil {
		t.Fatalf("GetLatestByHeadSHA: %v", err)
	}
	if latest.ID != firstID || latest.Evidence.TaskID != first.TaskID {
		t.Errorf("GetLatestByHeadSHA = %+v, want row %v", latest, firstID)
	}

	if _, err := store.GetLatestByHeadSHA(ctx, "0000000000000000000000000000000000000000"); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("unknown sha error = %v, want ErrNotFound", err)
	}

	empty, err := store.GetByWorkItem(ctx, uuid.New())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("GetByWorkItem(unknown) = %v, %v; want empty slice", empty, err)
	}
}

func TestStoreIntegration_InsertRejectsInvalidEvidence(t *testing.T) {
	p := integrationPool(t)
	defer p.Close()
	store := evidence.NewStore(p)
	ctx := context.Background()
	f := insertFixture(t, p)

	bad := newEvidence(f, "cccc000000000000000000000000000000000003", time.Now())
	bad.Outcome = evidence.OutcomeUnverified // inconsistent with the passed check
	if _, err := store.Insert(ctx, bad); err == nil {
		t.Fatal("Insert accepted inconsistent evidence")
	}
	if _, err := store.GetLatestByHeadSHA(ctx, bad.HeadSHA); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("invalid evidence reached the table: %v", err)
	}
}

func TestStoreIntegration_ForeignKeyBehaviour(t *testing.T) {
	p := integrationPool(t)
	defer p.Close()
	store := evidence.NewStore(p)
	ctx := context.Background()
	f := insertFixture(t, p)

	ev := newEvidence(f, "dddd000000000000000000000000000000000004", time.Now())
	id, err := store.Insert(ctx, ev)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// repository delete -> repository_id column SET NULL, row survives.
	if _, err := p.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, f.repositoryID); err != nil {
		t.Fatalf("delete repository: %v", err)
	}
	var repoCol *uuid.UUID
	if err := p.QueryRow(ctx, `SELECT repository_id FROM implementation_verifications WHERE id = $1`, id).Scan(&repoCol); err != nil {
		t.Fatalf("row missing after repository delete: %v", err)
	}
	if repoCol != nil {
		t.Errorf("repository_id = %v after repository delete, want NULL", *repoCol)
	}

	// work item delete -> row cascades away.
	if _, err := p.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, f.workItemID); err != nil {
		t.Fatalf("delete work item: %v", err)
	}
	if _, err := store.GetLatestByHeadSHA(ctx, ev.HeadSHA); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("row survived work item delete: %v", err)
	}
}

// TestStoreIntegration_Migration045ProductOptIn checks that migration 045
// applied cleanly alongside 044: a fresh product carries the D2 opt-in flag
// with its false default, the flag round-trips, and the value read from the
// row drives ReleaseDecision exactly as the contract states.
func TestStoreIntegration_Migration045ProductOptIn(t *testing.T) {
	p := integrationPool(t)
	defer p.Close()
	ctx := context.Background()
	f := insertFixture(t, p)

	var version string
	if err := p.QueryRow(ctx, `SELECT version FROM schema_migrations WHERE version = $1`,
		"045_products_partial_verification_opt_in").Scan(&version); err != nil {
		t.Fatalf("migration 045 not recorded in schema_migrations: %v", err)
	}

	readOptIn := func() bool {
		t.Helper()
		var optIn bool
		if err := p.QueryRow(ctx, `SELECT partial_verification_opt_in FROM products WHERE id = $1`, f.productID).Scan(&optIn); err != nil {
			t.Fatalf("read partial_verification_opt_in: %v", err)
		}
		return optIn
	}
	if readOptIn() {
		t.Fatal("partial_verification_opt_in must default to false")
	}

	partial := newEvidence(f, "2de333cabcdef0123456789abcdef0123456789a", time.Now().UTC())
	partial.Checks = append(partial.Checks, evidence.CheckResult{
		Kind: evidence.CheckKindLint, Command: "golangci-lint run",
		Status: evidence.CheckSkipped, SkipReason: "linter not installed",
	})
	partial.Outcome = evidence.OutcomePartiallyVerified

	if d := evidence.ReleaseDecision(partial, evidence.ReleasePolicy{PartialVerificationOptIn: readOptIn()}); d.Action != evidence.ActionBlock {
		t.Fatalf("default policy: decision = %+v, want block", d)
	}

	if _, err := p.Exec(ctx, `UPDATE products SET partial_verification_opt_in = true WHERE id = $1`, f.productID); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	if !readOptIn() {
		t.Fatal("partial_verification_opt_in did not persist true")
	}
	if d := evidence.ReleaseDecision(partial, evidence.ReleasePolicy{PartialVerificationOptIn: readOptIn()}); d.Action != evidence.ActionHumanReview {
		t.Fatalf("opted-in policy: decision = %+v, want human_review", d)
	}
	// Opting in never lifts a partially_verified run to release, and a
	// verified run releases regardless of the flag.
	if d := evidence.ReleaseDecision(newEvidence(f, "3de333cabcdef0123456789abcdef0123456789a", time.Now().UTC()),
		evidence.ReleasePolicy{PartialVerificationOptIn: readOptIn()}); d.Action != evidence.ActionRelease {
		t.Fatalf("verified with opt-in: decision = %+v, want release", d)
	}
}
