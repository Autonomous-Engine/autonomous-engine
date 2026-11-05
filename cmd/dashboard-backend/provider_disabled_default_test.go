package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/google/uuid"
)

// A disabled default is not a default: a new provider created while the
// only default row is disabled takes the default over, and the old row
// loses the flag in the same transaction, so a second default is never
// waiting to come back when the old row is re-enabled.
func TestIntegration_ANewProviderReplacesADisabledDefault(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	var hasDefault bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llm_providers WHERE is_default)`).Scan(&hasDefault)
	if hasDefault {
		t.Skip("a default LLM provider already exists in this database")
	}
	old := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO llm_providers (id, name, provider_type, endpoint, settings, is_default, enabled)
		VALUES ($1, 'old-default', 'ollama', 'http://127.0.0.1:1', '{}', true, false)`, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM llm_providers WHERE id = $1`, old) })
	store := configstore.New(pool, testMasterKey)
	ph := NewProviderHandlers(pool, store)
	rec := httptest.NewRecorder()
	ph.CreateProvider(rec, httptest.NewRequest(http.MethodPost, "/api/admin/providers/create",
		strings.NewReader(`{"name":"replacement","provider_type":"ollama","endpoint":"http://127.0.0.1:1","settings":{"model":"m"}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var row providerRow
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM llm_providers WHERE id = $1`, row.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM credential_registry WHERE scope='global' AND scope_target='default' AND category='llm' AND created_at > NOW() - interval '1 minute'`)
	})
	if !row.IsDefault {
		t.Fatal("the replacement must become the default")
	}
	var defaults int
	var oldDefault bool
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM llm_providers WHERE is_default`).Scan(&defaults)
	_ = pool.QueryRow(ctx, `SELECT is_default FROM llm_providers WHERE id = $1`, old).Scan(&oldDefault)
	if defaults != 1 || oldDefault {
		t.Fatalf("defaults = %d, old row default = %v; want the replacement alone", defaults, oldDefault)
	}
}

// Same for embedding providers, where a partial unique index allows one
// is_default row whether enabled or not: without clearing the disabled
// default first the insert collided with it (HTTP 500).
func TestIntegration_ANewEmbeddingProviderReplacesADisabledDefault(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	var hasDefault bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM embedding_providers WHERE is_default)`).Scan(&hasDefault)
	if hasDefault {
		t.Skip("a default embedding provider already exists in this database")
	}
	old := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO embedding_providers (id, name, provider_type, endpoint, model, dimension, settings, is_default, enabled)
		VALUES ($1, 'old-emb', 'ollama', 'http://127.0.0.1:1', 'nomic', 0, '{}', true, false)`, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM embedding_providers WHERE id = $1`, old) })
	eh := NewEmbeddingProviderHandlers(pool, configstore.New(pool, testMasterKey))
	rec := httptest.NewRecorder()
	eh.CreateEmbeddingProvider(rec, httptest.NewRequest(http.MethodPost, "/api/admin/embedding-providers/create",
		strings.NewReader(`{"name":"replacement-emb","provider_type":"ollama","endpoint":"http://127.0.0.1:1","model":"nomic-embed-text"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s (a disabled default must be replaced, not collide with the unique index)", rec.Code, rec.Body.String())
	}
	var row struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"is_default"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM embedding_providers WHERE id = $1`, row.ID) })
	var defaults int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM embedding_providers WHERE is_default`).Scan(&defaults)
	if !row.IsDefault || defaults != 1 {
		t.Fatalf("replacement default = %v, defaults = %d; want the replacement alone", row.IsDefault, defaults)
	}
}
