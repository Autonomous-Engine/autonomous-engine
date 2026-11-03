package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedProductionWorkItem makes the database look like a system with
// production behaviour to protect: the default-setting guard exempts a
// bootstrapping install (no work item has ever existed, F20), so tests that
// expect a refusal need one work item on record.
func seedProductionWorkItem(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "guard-prod-"+productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'intake', 'production marker', $2)`, workID, productID); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
}
