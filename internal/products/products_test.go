package products_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationPool follows the internal/evidence convention: it connects to
// TEST_POSTGRES_DSN (or TEST_DATABASE_URL), skips cleanly when neither is
// set, and applies the repository migrations so 045 is present.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
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

// TestStoreIntegration_PartialVerificationOptIn covers the D2 opt-in column
// through the store: it defaults to false on create, is loaded by GetProduct
// and ListProducts, and round-trips through SetPartialVerificationOptIn.
func TestStoreIntegration_PartialVerificationOptIn(t *testing.T) {
	pool := integrationPool(t)
	defer pool.Close()
	ctx := context.Background()
	store := products.NewStore(pool)

	name := "opt-in-fixture-" + uuid.New().String()[:8]
	created, err := store.CreateProduct(ctx, name, "D2 opt-in test")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = store.DeleteProduct(ctx, created.ID)
	})
	if created.PartialVerificationOptIn {
		t.Fatal("CreateProduct: PartialVerificationOptIn must default to false")
	}

	got, err := store.GetProduct(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}
	if got.PartialVerificationOptIn {
		t.Fatal("GetProduct: PartialVerificationOptIn must default to false")
	}

	if err := store.SetPartialVerificationOptIn(ctx, created.ID, true); err != nil {
		t.Fatalf("SetPartialVerificationOptIn(true): %v", err)
	}
	got, err = store.GetProduct(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetProduct after opt-in: %v", err)
	}
	if !got.PartialVerificationOptIn {
		t.Fatal("GetProduct: PartialVerificationOptIn should be true after opt-in")
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("opt-in should bump updated_at: created %v, now %v", created.UpdatedAt, got.UpdatedAt)
	}

	list, err := store.ListProducts(ctx)
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	found := false
	for _, p := range list {
		if p.ID == created.ID {
			found = true
			if !p.PartialVerificationOptIn {
				t.Error("ListProducts: PartialVerificationOptIn should be true after opt-in")
			}
		}
	}
	if !found {
		t.Fatal("ListProducts did not return the fixture product")
	}

	if err := store.SetPartialVerificationOptIn(ctx, created.ID, false); err != nil {
		t.Fatalf("SetPartialVerificationOptIn(false): %v", err)
	}
	got, err = store.GetProduct(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetProduct after opt-out: %v", err)
	}
	if got.PartialVerificationOptIn {
		t.Fatal("GetProduct: PartialVerificationOptIn should be false after opt-out")
	}

	err = store.SetPartialVerificationOptIn(ctx, uuid.New(), true)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("SetPartialVerificationOptIn on unknown id: err = %v, want not found", err)
	}
}
