package verifypolicy_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationPool connects to TEST_POSTGRES_DSN (skipping when unset) and
// applies the repository's migrations so 051 is present.
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

func insertProduct(t *testing.T, p *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := p.Exec(context.Background(), `INSERT INTO products (id, name) VALUES ($1, $2)`, id, "verifypolicy-it-"+id.String()[:8]); err != nil {
		t.Fatalf("insert product: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, id)
	})
	return id
}

// TestIntegrationVerificationPolicyStore covers the store against real
// PostgreSQL: missing row -> fail-closed default; repository and policy
// written in one transaction (onboarding); a failed policy write rolls the
// repository back; upsert updates in place; CHECK constraints reject bad
// values written around the store; the row cascades with its repository.
func TestIntegrationVerificationPolicyStore(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	productID := insertProduct(t, pool)
	ps := products.NewStore(pool)
	store := verifypolicy.NewStore(pool)

	// Repository + policy in one transaction.
	var repo *products.Repository
	cmd := "go test ./..."
	err := workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		r, err := ps.AddRepositoryTx(ctx, tx, productID, "it-repo", "https://github.com/example/it", "main", "squash", "ae/")
		if err != nil {
			return err
		}
		_, err = verifypolicy.UpsertTx(ctx, tx, verifypolicy.Policy{
			RepositoryID:        r.ID,
			TestCommand:         &cmd,
			RequiredChecks:      []evidence.CheckKind{evidence.CheckKindTest, evidence.CheckKindLint},
			NoChecksPolicy:      verifypolicy.NoChecksPartialWithHumanReview,
			SecurityReviewPaths: []string{"auth*", "migrations/**"},
			Source:              verifypolicy.SourceOnboarding,
			SetBy:               "onboarding wizard",
		})
		repo = r
		return err
	})
	if err != nil {
		t.Fatalf("add repository with policy: %v", err)
	}
	p, found, err := store.LoadVerificationPolicy(ctx, repo.ID)
	if err != nil || !found {
		t.Fatalf("load stored policy: found=%v err=%v", found, err)
	}
	if p.TestCommand == nil || *p.TestCommand != cmd || p.NoChecksPolicy != verifypolicy.NoChecksPartialWithHumanReview ||
		!reflect.DeepEqual(p.RequiredChecks, []evidence.CheckKind{"test", "lint"}) ||
		!reflect.DeepEqual(p.SecurityReviewPaths, []string{"auth*", "migrations/**"}) ||
		p.Source != verifypolicy.SourceOnboarding || p.SetBy != "onboarding wizard" || p.SetAt == nil {
		t.Fatalf("loaded = %+v", p)
	}

	// Upsert replaces in place; an empty override is stored as NULL.
	if _, err := verifypolicy.UpsertTx(ctx, pool, verifypolicy.Policy{
		RepositoryID: repo.ID, RequiredChecks: []evidence.CheckKind{}, NoChecksPolicy: verifypolicy.NoChecksBlock,
		SecurityReviewPaths: []string{}, Source: verifypolicy.SourceAdmin, SetBy: "user:x",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	p, found, err = store.LoadVerificationPolicy(ctx, repo.ID)
	if err != nil || !found || p.TestCommand != nil || p.NoChecksPolicy != verifypolicy.NoChecksBlock ||
		len(p.RequiredChecks) != 0 || len(p.SecurityReviewPaths) != 0 || p.Source != verifypolicy.SourceAdmin {
		t.Fatalf("after upsert: %+v found=%v err=%v", p, found, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM repository_verification_policies WHERE repository_id = $1`, repo.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows for repository = %d (%v), want 1", rows, err)
	}

	// A repository without a row gets the fail-closed default.
	bare, err := ps.AddRepository(ctx, productID, "it-bare", "https://github.com/example/bare", "main", "squash", "ae/")
	if err != nil {
		t.Fatalf("add bare repository: %v", err)
	}
	p, found, err = store.LoadVerificationPolicy(ctx, bare.ID)
	if err != nil || found || !reflect.DeepEqual(p, verifypolicy.Default(bare.ID)) {
		t.Fatalf("bare repository: %+v found=%v err=%v, want the default", p, found, err)
	}

	// The policy write failing rolls the repository insert back.
	var ghost uuid.UUID
	err = workgraph.WithTx(ctx, pool, func(tx pgx.Tx) error {
		r, err := ps.AddRepositoryTx(ctx, tx, productID, "it-ghost", "https://github.com/example/ghost", "main", "squash", "ae/")
		if err != nil {
			return err
		}
		ghost = r.ID
		_, err = tx.Exec(ctx, `INSERT INTO repository_verification_policies (repository_id, no_checks_policy, source) VALUES ($1, 'allow', 'onboarding')`, r.ID)
		return err
	})
	if err == nil {
		t.Fatal("CHECK constraint accepted no_checks_policy='allow'")
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM repositories WHERE id = $1`, ghost).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("repository survived its policy's rollback: rows=%d err=%v", rows, err)
	}

	// The other CHECK constraints hold for writes around the store.
	for name, sql := range map[string]string{
		"source":          `INSERT INTO repository_verification_policies (repository_id, source) VALUES ($1, 'api')`,
		"required_checks": `INSERT INTO repository_verification_policies (repository_id, required_checks, source) VALUES ($1, '{diagnostic}', 'admin')`,
	} {
		if _, err := pool.Exec(ctx, sql, bare.ID); err == nil {
			t.Errorf("CHECK %s accepted an invalid value", name)
		}
	}
	// Column defaults are the fail-closed ones.
	if _, err := pool.Exec(ctx, `INSERT INTO repository_verification_policies (repository_id) VALUES ($1)`, bare.ID); err != nil {
		t.Fatalf("insert with defaults: %v", err)
	}
	p, found, err = store.LoadVerificationPolicy(ctx, bare.ID)
	if err != nil || !found || p.NoChecksPolicy != verifypolicy.NoChecksBlock || !reflect.DeepEqual(p.RequiredChecks, []evidence.CheckKind{"test"}) || p.Source != verifypolicy.SourceDefault {
		t.Fatalf("column defaults: %+v found=%v err=%v", p, found, err)
	}

	// Deleting the repository cascades to its policy.
	if err := ps.DeleteRepository(ctx, repo.ID); err != nil {
		t.Fatalf("delete repository: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM repository_verification_policies WHERE repository_id = $1`, repo.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("policy rows after repository delete = %d (%v), want 0", rows, err)
	}
	_, _, err = store.LoadVerificationPolicy(ctx, repo.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("load after delete: %v", err)
	}
}
