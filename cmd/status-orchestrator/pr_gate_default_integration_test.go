package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Migration 029 promised "products.default_pr_gate_mode: default gate mode
// for new work items in this product" and the dashboard sets it, but no
// creation path read it: every new item was 'auto', and a product whose
// owner had chosen the manual gate still had its PRs opened on GitHub (or,
// with no token, skipped with a log line). A new work item that does not
// name its gate takes its product's default; one that names it keeps it.
func TestIntegration_NewWorkItemTakesTheProductsDefaultPRGate(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	productID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name, default_pr_gate_mode) VALUES ($1, $2, 'manual')`, productID, "gate-test-"+productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE product_id = $1`, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
	gateOf := func(id uuid.UUID) string {
		var g string
		if err := pool.QueryRow(ctx, `SELECT pr_gate_mode FROM work_items WHERE id = $1`, id).Scan(&g); err != nil {
			t.Fatalf("read gate: %v", err)
		}
		return g
	}

	inherited := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id)
		VALUES ($1, 'feature', 'intake', 'inherits', 'd', $2)`, inherited, productID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if g := gateOf(inherited); g != "manual" {
		t.Fatalf("a new item of a manual-gate product has pr_gate_mode %q, want manual", g)
	}

	explicit := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, product_id, pr_gate_mode)
		VALUES ($1, 'feature', 'intake', 'explicit', 'd', $2, 'auto')`, explicit, productID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if g := gateOf(explicit); g != "auto" {
		t.Fatalf("an item created with an explicit gate has pr_gate_mode %q, want auto", g)
	}

	orphan := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description)
		VALUES ($1, 'feature', 'intake', 'no product', 'd')`, orphan); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, orphan) })
	if g := gateOf(orphan); g != "auto" {
		t.Fatalf("a product-less item has pr_gate_mode %q, want auto", g)
	}
}
