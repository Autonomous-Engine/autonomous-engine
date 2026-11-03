package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Validation said "ready" for a product whose onboarding answers were
// never approved, whose repos had no verification policy or zones, and on
// a system with no model or embedding provider (F30).
func TestIntegration_ValidateProductChecksWhatOnboardingNeeds(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	productID, repoID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "validate-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repositories (id, product_id, name, github_url, base_branch, clone_path, enabled)
		VALUES ($1, $2, 'r', '/repo-cache/mirrors/r.git', 'main', '/repo-cache/x/r/', true)`, repoID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, repoID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
	})

	h := &AdminHandlers{db: pool}
	rec := httptest.NewRecorder()
	h.ValidateProduct(rec, httptest.NewRequest(http.MethodPost, "/api/admin/products/validate?product_id="+productID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var results []struct {
		Check   string `json:"check"`
		Passed  bool   `json:"passed"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	byCheck := map[string]bool{}
	for _, r := range results {
		byCheck[r.Check] = r.Passed
	}
	for _, name := range []string{"Default LLM provider has a model", "Default embedding provider"} {
		if _, ok := byCheck[name]; !ok {
			t.Errorf("check %q missing from %v", name, byCheck)
		}
	}
	for _, name := range []string{"Verification policy saved for every repo", "Zones confirmed for every repo", "Codex approved"} {
		passed, ok := byCheck[name]
		if !ok {
			t.Errorf("check %q missing from %v", name, byCheck)
		} else if passed {
			t.Errorf("check %q passed for a fresh product", name)
		}
	}

	// A zones-only onboarding version (what confirming zones records) is not
	// an approval; the audit's state passed the first version of this check
	// (review I3). A version that wrote an answer section is.
	validate := func() map[string]bool {
		rec := httptest.NewRecorder()
		h.ValidateProduct(rec, httptest.NewRequest(http.MethodPost, "/api/admin/products/validate?product_id="+productID.String(), nil))
		var rs []struct {
			Check  string `json:"check"`
			Passed bool   `json:"passed"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &rs)
		out := map[string]bool{}
		for _, r := range rs {
			out[r.Check] = r.Passed
		}
		return out
	}
	if _, err := pool.Exec(ctx, `INSERT INTO codex_versions (product_id, branch, product_version, sections_changed, change_summary, change_source)
		VALUES ($1, 'main', 1, ARRAY['section:zones'], 'zones', 'onboarding')`, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM codex_versions WHERE product_id = $1`, productID) })
	if validate()["Codex approved"] {
		t.Fatal("a zones-only onboarding version must not count as Approve Codex")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO codex_versions (product_id, branch, product_version, sections_changed, change_summary, change_source)
		VALUES ($1, 'main', 2, ARRAY['section:architecture_overview','patterns'], 'approve', 'onboarding')`, productID); err != nil {
		t.Fatal(err)
	}
	if !validate()["Codex approved"] {
		t.Fatal("an onboarding version with answer sections is an approval")
	}
}
