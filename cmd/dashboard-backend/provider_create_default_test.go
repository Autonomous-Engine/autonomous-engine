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

// The first provider created on an install becomes the default, so the
// credential sync agents read from runs at once; before, the row sat with
// is_default=false and no model ever reached the agents (F16).
func TestIntegration_FirstProviderBecomesDefault(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	var hasDefault bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llm_providers WHERE is_default AND enabled)`).Scan(&hasDefault)
	if hasDefault {
		t.Skip("a default LLM provider already exists in this database")
	}
	store := configstore.New(pool, testMasterKey)
	ph := NewProviderHandlers(pool, store)
	// The default-provider sync writes global llm credentials; remove the
	// ones this test creates so later tests see the registry as before.
	var before []uuid.UUID
	rows, err := pool.Query(ctx, `SELECT id FROM credential_registry WHERE scope='global' AND scope_target='default' AND category='llm'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		before = append(before, id)
	}
	rows.Close()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM credential_registry WHERE scope='global' AND scope_target='default' AND category='llm' AND NOT (id = ANY($1))`, before)
	})

	create := func(name string) providerRow {
		rec := httptest.NewRecorder()
		body := `{"name":"` + name + `","provider_type":"ollama","endpoint":"http://127.0.0.1:1","settings":{"model":"qwen3.6:35b"}}`
		ph.CreateProvider(rec, httptest.NewRequest(http.MethodPost, "/api/admin/providers/create", strings.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
		}
		var row providerRow
		if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM llm_providers WHERE id = $1`, row.ID) })
		return row
	}

	first := create("first-provider")
	if !first.IsDefault {
		t.Fatal("the first provider must become the default")
	}
	if model, _ := store.ResolveCredential(ctx, "llm", "model_name", "", ""); model != "qwen3.6:35b" {
		t.Fatalf("llm/model_name = %q after the first provider, want qwen3.6:35b (sync must run)", model)
	}
	second := create("second-provider")
	if second.IsDefault {
		t.Fatal("a later provider must not steal the default")
	}
}

func TestIntegration_FirstEmbeddingProviderBecomesDefault(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	var hasDefault bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM embedding_providers WHERE is_default AND enabled)`).Scan(&hasDefault)
	if hasDefault {
		t.Skip("a default embedding provider already exists in this database")
	}
	eh := NewEmbeddingProviderHandlers(pool, configstore.New(pool, testMasterKey))
	rec := httptest.NewRecorder()
	body := `{"name":"first-emb","provider_type":"ollama","endpoint":"http://127.0.0.1:1","model":"nomic-embed-text"}`
	eh.CreateEmbeddingProvider(rec, httptest.NewRequest(http.MethodPost, "/api/admin/embedding-providers/create", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var row struct {
		ID        string `json:"id"`
		IsDefault bool   `json:"is_default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM embedding_providers WHERE id = $1`, row.ID) })
	if !row.IsDefault {
		t.Fatal("the first embedding provider must become the default")
	}
}
