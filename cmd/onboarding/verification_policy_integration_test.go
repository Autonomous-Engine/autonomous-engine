package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/autonomousengine/autonomous-engine/internal/verifypolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func onboardingIntegrationPool(t *testing.T) *pgxpool.Pool {
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

// TestIntegrationOnboardingPolicyOwnership exercises the real
// database-backed onboarding route: it creates a policy for a repository
// without one, updates its own onboarding row, and gets 409 — with the row
// unchanged — for an admin-owned policy. add-repo's same-transaction write
// goes through the same ownership-restricted upsert.
func TestIntegrationOnboardingPolicyOwnership(t *testing.T) {
	pool := onboardingIntegrationPool(t)
	ctx := context.Background()
	productID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m303-onboarding-it-"+productID.String()[:8]); err != nil {
		t.Fatalf("insert product: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, productID) })
	ps := products.NewStore(pool)
	s := &server{pool: pool, productStore: ps}
	store := verifypolicy.NewStore(pool)
	target := "/api/onboarding/verification-policy"

	// Admin-owned row: onboarding gets 409 and the row is unchanged.
	adminRepo, err := ps.AddRepository(ctx, productID, "it-admin", "https://github.com/example/admin", "main", "squash", "ae/")
	if err != nil {
		t.Fatal(err)
	}
	cmd := "go test ./..."
	if _, err := verifypolicy.UpsertTx(ctx, pool, verifypolicy.Policy{
		RepositoryID: adminRepo.ID, TestCommand: &cmd, RequiredChecks: []evidence.CheckKind{"test"},
		NoChecksPolicy: verifypolicy.NoChecksBlock, SecurityReviewPaths: []string{"auth*"},
		Source: verifypolicy.SourceAdmin, SetBy: "user:admin",
	}); err != nil {
		t.Fatalf("seed admin policy: %v", err)
	}
	before, _, err := store.LoadVerificationPolicy(ctx, adminRepo.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := policyRequest(t, s, http.MethodPost, target, map[string]any{
		"repository_id": adminRepo.ID.String(), "test_command": "true",
		"no_checks_policy": "partial_with_human_review", "security_review_paths": []string{},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("onboarding POST over admin row = %d %s, want 409", rec.Code, rec.Body.String())
	}
	after, found, err := store.LoadVerificationPolicy(ctx, adminRepo.ID)
	if err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatalf("admin row changed: before %+v after %+v (found=%v err=%v)", before, after, found, err)
	}
	// The same rule holds inside a transaction (add-repo path helper).
	if _, err := verifypolicy.UpsertOnboardingTx(ctx, pool, verifypolicy.Policy{RepositoryID: adminRepo.ID, NoChecksPolicy: verifypolicy.NoChecksBlock, Source: verifypolicy.SourceOnboarding}); err == nil {
		t.Fatal("UpsertOnboardingTx overwrote an admin row")
	}

	// No row: onboarding creates it, then updates its own row.
	ownRepo, err := ps.AddRepository(ctx, productID, "it-own", "https://github.com/example/own", "main", "squash", "ae/")
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"block", "partial_with_human_review"} {
		rec, view := policyRequest(t, s, http.MethodPost, target, map[string]any{"repository_id": ownRepo.ID.String(), "no_checks_policy": policy})
		if rec.Code != http.StatusOK || !view.Found {
			t.Fatalf("onboarding POST %s = %d %s", policy, rec.Code, rec.Body.String())
		}
		p, found, err := store.LoadVerificationPolicy(ctx, ownRepo.ID)
		if err != nil || !found || string(p.NoChecksPolicy) != policy || p.Source != verifypolicy.SourceOnboarding {
			t.Fatalf("stored after POST %s: %+v found=%v err=%v", policy, p, found, err)
		}
	}

	// add-repo writes the repository and its onboarding policy together.
	repo, err := s.addRepositoryWithPolicy(ctx, &verifypolicy.Input{NoChecksPolicy: "block"}, productID, "it-added", "https://github.com/example/added", "main", "squash", "ae/")
	if err != nil {
		t.Fatalf("addRepositoryWithPolicy: %v", err)
	}
	if p, found, err := store.LoadVerificationPolicy(ctx, repo.ID); err != nil || !found || p.Source != verifypolicy.SourceOnboarding {
		t.Fatalf("added repo policy: %+v found=%v err=%v", p, found, err)
	}
}
