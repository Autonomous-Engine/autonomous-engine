package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RepinTx moves a pin to the codex version a plan was built on, keeping its
// product, branch and modules, and the work item's displayed codex_version
// follows. A pin on another branch is left as it is; an item without a pin
// stays unpinned.
func TestRepinTx(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	pid := newProduct(t, pool, "main")
	item := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id) VALUES ($1, 'feature', 'planning', 'p', 'd', $2)`, item, pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, item) })
	store := NewPinStore(pool)
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
	inTx(func(tx pgx.Tx) error {
		return store.PinTx(ctx, tx, Pin{WorkItemID: item, ProductID: pid, Branch: "main", ProductVersion: 3, Modules: []string{"api", "billing"}})
	})

	var got Pin
	var found bool
	inTx(func(tx pgx.Tx) (err error) {
		got, found, err = store.RepinTx(ctx, tx, item, "main", 4)
		return err
	})
	if !found || got.ProductVersion != 4 || got.Branch != "main" || got.ProductID != pid || strings.Join(got.Modules, ",") != "api,billing" {
		t.Fatalf("RepinTx = %+v, %v; want v4 on main with api,billing", got, found)
	}
	var display int
	_ = pool.QueryRow(ctx, `SELECT codex_version FROM work_items WHERE id = $1`, item).Scan(&display)
	if display != 4 {
		t.Fatalf("work_items.codex_version = %d, want 4", display)
	}

	// Another branch: the row's pin is returned unchanged.
	inTx(func(tx pgx.Tx) (err error) {
		got, found, err = store.RepinTx(ctx, tx, item, "develop", 9)
		return err
	})
	if !found || got.ProductVersion != 4 || got.Branch != "main" {
		t.Fatalf("RepinTx on another branch = %+v, %v; want the unchanged v4 main pin", got, found)
	}

	inTx(func(tx pgx.Tx) error {
		_, found, err := store.RepinTx(ctx, tx, uuid.New(), "main", 4)
		if found {
			t.Error("an item without a pin was pinned")
		}
		return err
	})
}
