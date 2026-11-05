package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
)

// Create Product answered {id, status} only, so the wizard's product list
// and header showed the new product with an empty name until a reload
// (rerun NEW-3). The response is the product row the caller just created.
func TestIntegration_CreateProductReturnsTheProduct(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	h := &AdminHandlers{db: pool, store: configstore.New(pool, testMasterKey)}
	rec := httptest.NewRecorder()
	h.CreateProduct(rec, httptest.NewRequest(http.MethodPost, "/api/admin/products/create", strings.NewReader(`{"name":"enmime","description":"MIME parser"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, got.ID) })
	if got.ID == "" || got.Name != "enmime" || got.Description != "MIME parser" || got.CreatedAt == "" {
		t.Fatalf("response = %s, want the created product with id, name, description and created_at", rec.Body.String())
	}
}
