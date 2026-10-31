package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/google/uuid"
)

func f64(v float64) *float64 { return &v }

// Every M4-05 route is admin-only: snapshot documents expose prompt
// templates, definition prompts and provider settings.
func TestConfigVersionRoutes_AreAdminOnly(t *testing.T) {
	fakeAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), contextKeyRole, r.Header.Get("X-Test-Role"))
			next(w, r.WithContext(ctx))
		}
	}
	mux := http.NewServeMux()
	registerConfigVersionRoutes(mux, fakeAuth, &ConfigVersionHandlers{snaps: configsnap.NewStore(nil)})

	for _, path := range []string{
		"/api/config-snapshots?id=x",
		"/api/config-snapshots/diff?a=x",
		"/api/admin/config-snapshots",
		"/api/admin/model-prices",
	} {
		for _, role := range []string{"", "user", "viewer"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Test-Role", role)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s as %q = %d, want 403", path, role, rec.Code)
			}
		}
	}
	// An admin reaches the handler (an invalid id is a 404, not a 403).
	for _, path := range []string{"/api/config-snapshots?id=x", "/api/config-snapshots/diff?a=x"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-Role", "admin")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s as admin = %d, want 404", path, rec.Code)
		}
	}
}

// by_provider / by_model merge pre-M4-05 rows ("prov/model" in both
// columns, or "" model) with bare M4-05 rows.
func TestIntegration_MetricsGroupByNormalisedProviderAndModel(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO inference_logs (model_provider, model_id, intent_type, input_tokens, output_tokens) VALUES
		('m405mix/claude-x', 'm405mix/claude-x', 'old-both', 10, 1),
		('m405mix/claude-x', '', 'old-tool-loop', 20, 2),
		('m405mix', 'claude-x', 'new', 30, 3),
		('m405mix', 'claude-y', 'new', 40, 4)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM inference_logs WHERE model_provider LIKE 'm405mix%'`) })

	collect := func(sql string) map[string][3]int {
		rows, err := pool.Query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][3]int{}
		for rows.Next() {
			var name string
			var calls, in, outTok int
			if err := rows.Scan(&name, &calls, &in, &outTok); err != nil {
				t.Fatal(err)
			}
			out[name] = [3]int{calls, in, outTok}
		}
		return out
	}
	prov := collect(metricsByProviderSQL)
	if got := prov["m405mix"]; got != [3]int{4, 100, 10} {
		t.Errorf("by_provider m405mix = %v, want one group of 4 calls", got)
	}
	if _, split := prov["m405mix/claude-x"]; split {
		t.Error("legacy provider/model rows still form their own provider group")
	}
	models := collect(metricsByModelSQL)
	if got := models["claude-x"]; got[0] != 3 || got[1] < 60 {
		t.Errorf("by_model claude-x = %v, want the 3 old+new rows merged", got)
	}
	if _, split := models["m405mix/claude-x"]; split {
		t.Error("legacy model ids still form their own group")
	}
}

// The rollup's "verified" counts release decisions only: a reviewed item
// (reviewed_head_sha set, human_review decision) is not verified. Item cost
// is unknown while any item's cost is unknown.
func TestIntegration_RollupVerifiedMeansReleased(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	snap := strings.Repeat("5", 64)
	productID, released, reviewed := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO config_snapshots (id, schema_version, agent_type, document) VALUES ($1, 1, 'implementation-agent', '{}')`, []any{snap}},
		{`INSERT INTO products (id, name) VALUES ($1, $2)`, []any{productID, "m405-rollup-" + productID.String()[:8]}},
		{`INSERT INTO work_items (id, type, title, product_id, config_snapshot_id) VALUES ($1, 'feature', 'r1', $3, $2), ($4, 'feature', 'r2', $3, $2)`, []any{released, snap, productID, reviewed}},
		{`INSERT INTO work_item_outcomes (work_item_id, config_snapshot_id, verified_head_sha, reviewed_head_sha, release_decision, cost_usd)
			VALUES ($1, $3, 'v1', 'v1', 'release', 1.25), ($2, $3, NULL, 'r2', 'human_review', NULL)`, []any{released, reviewed, snap}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM work_items WHERE product_id = $1`, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM config_snapshots WHERE id = $1`, snap)
	})

	h := NewConfigVersionHandlers(pool, nil)
	rec := httptest.NewRecorder()
	h.ListSnapshots(rec, httptest.NewRequest(http.MethodGet, "/api/admin/config-snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Snapshots []snapshotRollup `json:"snapshots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var got *snapshotRollup
	for i := range body.Snapshots {
		if body.Snapshots[i].ID == snap {
			got = &body.Snapshots[i]
		}
	}
	if got == nil {
		t.Fatal("snapshot missing from the rollup")
	}
	if got.Items != 2 || got.Verified != 1 || got.VerifiedRate == nil || *got.VerifiedRate != 0.5 {
		t.Errorf("items=%d verified=%d rate=%v, want 2/1/0.5", got.Items, got.Verified, got.VerifiedRate)
	}
	if got.ItemCostUSD != nil || !got.ItemCostUnknown || got.ItemCostUnknownItems != 1 {
		t.Errorf("item cost = %v unknown=%t (%d), want unknown with 1 unpriced item", got.ItemCostUSD, got.ItemCostUnknown, got.ItemCostUnknownItems)
	}
}

// /api/work-items/cost: cost_usd only when every call with tokens is priced;
// never 0 for unknown.
func TestCostSummary(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name                        string
		calls, tokenCalls, unpriced int64
		sum                         *float64
		wantCost                    any
	}{
		{"all priced", 3, 3, 0, f64(0.42), 0.42},
		{"one unpriced", 3, 3, 1, f64(0.40), nil},
		{"no calls", 0, 0, 0, nil, nil},
		{"only zero-token calls", 2, 0, 0, nil, nil},
		{"no prices at all", 2, 2, 2, nil, nil},
	}
	for _, c := range cases {
		got := costSummary(id, 10, 5, c.calls, c.tokenCalls, c.unpriced, c.sum)
		if got["cost_usd"] != c.wantCost {
			t.Errorf("%s: cost_usd = %v, want %v", c.name, got["cost_usd"], c.wantCost)
		}
		if got["cost_unknown"] != (c.wantCost == nil) {
			t.Errorf("%s: cost_unknown = %v", c.name, got["cost_unknown"])
		}
		if got["total_tokens"] != int64(15) || got["total_llm_calls"] != c.calls {
			t.Errorf("%s: totals = %v", c.name, got)
		}
	}
}

func TestFinishRollup(t *testing.T) {
	s := snapshotRollup{Items: 4, Verified: 3, HumanEditKnown: 2, HumanEdited: 1, CostUSD: f64(1.5), ItemCostUSD: f64(4)}
	finishRollup(&s, 10)
	if s.ItemCostUSD == nil || s.ItemCostUnknown {
		t.Fatalf("fully priced item cost lost: %+v", s)
	}
	partial := snapshotRollup{Items: 2, ItemCostUSD: f64(4), ItemCostUnknownItems: 1}
	finishRollup(&partial, 0)
	if partial.ItemCostUSD != nil || !partial.ItemCostUnknown {
		t.Fatal("item cost must be unknown while any item's cost is unknown")
	}
	if s.VerifiedRate == nil || *s.VerifiedRate != 0.75 || s.HumanEditRate == nil || *s.HumanEditRate != 0.5 {
		t.Fatalf("rates = %v %v", s.VerifiedRate, s.HumanEditRate)
	}
	if s.CostUSD == nil || s.CostUnknown {
		t.Fatalf("priced cost lost: %+v", s)
	}
	s = snapshotRollup{CostUSD: f64(1.5), UnpricedCalls: 1}
	finishRollup(&s, 10)
	if s.VerifiedRate != nil || s.HumanEditRate != nil {
		t.Fatal("rates must be nil without a denominator")
	}
	if s.CostUSD != nil || !s.CostUnknown {
		t.Fatal("a partially priced snapshot must report unknown cost")
	}
}

func TestPriceRequestValidate(t *testing.T) {
	ok := priceRequest{Provider: " anthropic ", ModelID: "claude", InputPer1M: f64(3), OutputPer1M: f64(15)}
	eff, msg := ok.validate()
	if msg != "" || !eff.Equal(time.Unix(0, 0)) || ok.Provider != "anthropic" {
		t.Fatalf("valid request: %q %v %q", msg, eff, ok.Provider)
	}
	dated := priceRequest{Provider: "a", ModelID: "m", EffectiveFrom: "2026-10-01"}
	if eff, msg := dated.validate(); msg != "" || eff.Day() != 1 {
		t.Fatalf("date: %q %v", msg, eff)
	}
	for name, bad := range map[string]priceRequest{
		"no provider": {ModelID: "m"},
		"negative":    {Provider: "a", ModelID: "m", InputPer1M: f64(-1)},
		"bad date":    {Provider: "a", ModelID: "m", EffectiveFrom: "yesterday"},
	} {
		if _, msg := bad.validate(); msg == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}
