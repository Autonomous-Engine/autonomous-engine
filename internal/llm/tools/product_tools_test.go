package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// Live, the chat agent passed the product by its name (as list_products
// shows it), both product tools answered "invalid product_id", and its
// third try created the work item with no product at all — which the
// product agent then blocked, and the block raised no prompt because a
// prompt needs a product. A work item without a product is refused at
// creation, with the way out in the message.
func TestCreateWorkItemWithoutAProductIsRefused(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	res := ex.Execute(context.Background(), adapter.ToolCall{ID: "c1", Name: "create_work_item", Arguments: `{"title":"t","description":"d","type":"feature","priority":"medium"}`})
	if !res.IsError || !strings.Contains(res.Content, "product is required") || !strings.Contains(res.Content, "set_product_context") {
		t.Fatalf("result = %+v, want a refusal that names set_product_context", res)
	}
}

// The product tools take the product's id or its name.
func TestIntegration_ProductToolsAcceptTheProductName(t *testing.T) {
	pool := toolsIntegrationPool(t)
	ctx := context.Background()
	productID := uuid.New()
	name := "Enmime Test " + productID.String()[:8]
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, name); err != nil {
		t.Fatal(err)
	}
	var created []uuid.UUID
	t.Cleanup(func() {
		for _, id := range created {
			_, _ = pool.Exec(ctx, `DELETE FROM outbox_events WHERE aggregate_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, id)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})
	ex := NewExecutor(t.TempDir())
	ex.db = pool
	// By name, in another case.
	res := ex.Execute(ctx, adapter.ToolCall{ID: "c1", Name: "set_product_context", Arguments: `{"product_id":"` + strings.ToLower(name) + `"}`})
	if res.IsError || ex.productID == nil || *ex.productID != productID {
		t.Fatalf("set_product_context by name: %+v (product %v)", res, ex.productID)
	}
	// No product_id: the session's product context is the product.
	res = ex.Execute(ctx, adapter.ToolCall{ID: "c2", Name: "create_work_item", Arguments: `{"title":"by context","description":"d","type":"feature","priority":"medium"}`})
	if res.IsError {
		t.Fatalf("create_work_item from context: %+v", res)
	}
	id := createdWorkItemID(t, res.Content)
	created = append(created, id)
	var got *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT product_id FROM work_items WHERE id = $1`, id).Scan(&got); err != nil || got == nil || *got != productID {
		t.Fatalf("work item product = %v (%v), want %s", got, err, productID)
	}
	// By name on create_work_item itself.
	ex2 := NewExecutor(t.TempDir())
	ex2.db = pool
	res = ex2.Execute(ctx, adapter.ToolCall{ID: "c3", Name: "create_work_item", Arguments: `{"title":"by name","description":"d","type":"feature","priority":"medium","product_id":"` + name + `"}`})
	if res.IsError {
		t.Fatalf("create_work_item by name: %+v", res)
	}
	id = createdWorkItemID(t, res.Content)
	created = append(created, id)
	if err := pool.QueryRow(ctx, `SELECT product_id FROM work_items WHERE id = $1`, id).Scan(&got); err != nil || got == nil || *got != productID {
		t.Fatalf("work item product = %v (%v), want %s", got, err, productID)
	}
	// An unknown name is refused, naming list_products.
	res = ex2.Execute(ctx, adapter.ToolCall{ID: "c4", Name: "set_product_context", Arguments: `{"product_id":"no such product"}`})
	if !res.IsError || !strings.Contains(res.Content, "list_products") {
		t.Fatalf("unknown product: %+v", res)
	}
}

// createdWorkItemID extracts the new work item's id from create_work_item's message.
func createdWorkItemID(t *testing.T, content string) uuid.UUID {
	t.Helper()
	for _, f := range strings.FieldsFunc(content, func(r rune) bool { return r == ' ' || r == '\n' || r == '/' || r == ')' || r == '(' || r == '`' || r == ']' || r == '[' }) {
		if id, err := uuid.Parse(strings.Trim(f, ".,:")); err == nil {
			return id
		}
	}
	t.Fatalf("no work item id in %q", content)
	return uuid.Nil
}
