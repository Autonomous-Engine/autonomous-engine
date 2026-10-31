package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/google/uuid"
)

// Every M4-06 route is admin-only.
func TestRolloutRoutes_AreAdminOnly(t *testing.T) {
	fakeAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), contextKeyRole, r.Header.Get("X-Test-Role"))
			next(w, r.WithContext(ctx))
		}
	}
	mux := http.NewServeMux()
	registerRolloutRoutes(mux, fakeAuth, &RolloutHandlers{})
	for _, path := range []string{
		"/api/admin/rollouts", "/api/admin/rollouts/detail?id=x", "/api/admin/rollouts/approve?id=x",
		"/api/admin/rollouts/block?id=x", "/api/admin/rollouts/overrides", "/api/admin/products/replay",
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
	// An admin reaches the handler (a bad id is a 400, not a 403).
	req := httptest.NewRequest(http.MethodGet, "/api/admin/rollouts/detail?id=x", nil)
	req.Header.Set("X-Test-Role", "admin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("admin detail with a bad id = %d, want 400", rec.Code)
	}
}

func TestOverrideReasonFrom(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x?override_reason="+url.QueryEscape(" hotfix "), nil)
	if got := overrideReasonFrom(r); got != "hotfix" {
		t.Errorf("query: %q", got)
	}
	r = httptest.NewRequest(http.MethodPost, "/x", nil)
	r.Header.Set("X-Override-Reason", "via header")
	if got := overrideReasonFrom(r); got != "via header" {
		t.Errorf("header: %q", got)
	}
}

// A guarded route without a database refuses (fail closed) instead of
// changing configuration unchecked.
func TestGuardConfigChange_NoDatabaseRefuses(t *testing.T) {
	rec := httptest.NewRecorder()
	ok := guardConfigChange(rec, httptest.NewRequest(http.MethodPost, "/x", nil), nil, "agents.set_default", nil, nil)
	if ok || rec.Code != http.StatusInternalServerError {
		t.Errorf("ok=%t code=%d", ok, rec.Code)
	}
}

// Integration (TEST_POSTGRES_DSN; -p 1): setting a candidate definition as
// the default through the admin API is refused without an approved
// rollout, allowed with an audited override reason, and allowed without
// one once the rollout is approved.
func TestIntegration_SetDefaultAgentRequiresApprovedRollout(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()

	var enabled []uuid.UUID
	rows, err := pool.Query(ctx, `SELECT id FROM agent_definitions WHERE enabled`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		enabled = append(enabled, id)
	}
	rows.Close()
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET enabled = false WHERE id = ANY($1)`, enabled); err != nil {
		t.Fatal(err)
	}
	prod, cand := uuid.New(), uuid.New()
	sfx := prod.String()[:8]
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (id, name, provider, model_id, enabled, is_default)
		VALUES ($1, $2, 'm406dash', 'prod', true, true), ($3, $4, 'm406dash', 'cand', true, false)`,
		prod, "m406-dash-prod-"+sfx, cand, "m406-dash-cand-"+sfx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM rollout_overrides WHERE details->>'agent_definition_id' = $1`, cand.String())
		_, _ = pool.Exec(ctx, `DELETE FROM rollouts WHERE created_by = 'm406-dash-test'`)
		_, _ = pool.Exec(ctx, `DELETE FROM agent_definitions WHERE id = ANY($1)`, []uuid.UUID{prod, cand})
		_, _ = pool.Exec(ctx, `UPDATE agent_definitions SET enabled = true WHERE id = ANY($1)`, enabled)
	})

	h := &AdminHandlers{db: pool, agentRegistry: agentregistry.New(pool), promptStore: prompts.NewStore(pool)}
	setDefault := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.SetDefaultAgent(rec, httptest.NewRequest(http.MethodPost, "/api/admin/agents/set-default?id="+cand.String()+query, nil))
		return rec
	}
	isDefault := func() bool {
		var d bool
		_ = pool.QueryRow(ctx, `SELECT is_default FROM agent_definitions WHERE id = $1`, cand).Scan(&d)
		return d
	}

	// 1. Refused: 409 rollout_required, nothing changed.
	rec := setDefault("")
	if rec.Code != http.StatusConflict {
		t.Fatalf("no rollout: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "rollout_required" || isDefault() {
		t.Fatalf("refusal body %v, default=%t", body, isDefault())
	}

	// 2. Override with a reason: applied and audited.
	rec = setDefault("&override_reason=" + url.QueryEscape("hotfix for incident 42"))
	if rec.Code != http.StatusOK || !isDefault() {
		t.Fatalf("override: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM rollout_overrides WHERE action = 'agents.set_default'
		AND override_reason = 'hotfix for incident 42' AND details->>'agent_definition_id' = $1`, cand.String()).Scan(&n)
	if n != 1 {
		t.Errorf("override audit rows = %d, want 1", n)
	}

	// 3. Back to prod (override), then an approved rollout for the
	// candidate makes the same change allowed without a reason.
	rec = httptest.NewRecorder()
	h.SetDefaultAgent(rec, httptest.NewRequest(http.MethodPost, "/api/admin/agents/set-default?id="+prod.String()+"&override_reason=revert", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body.String())
	}
	candID, doc, err := rollout.CaptureUnit(ctx, pool, &cand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configsnap.NewStore(pool).GetOrCreate(ctx, doc); err != nil {
		t.Fatal(err)
	}
	// The rollout is measured against the current production default (a
	// rollout with another baseline is stale for the default entry).
	baseID, baseDoc, err := rollout.ProductionSnapshot(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := configsnap.NewStore(pool).GetOrCreate(ctx, baseDoc); err != nil {
		t.Fatal(err)
	}
	store := rollout.NewStore(pool)
	ro, err := store.Propose(ctx, rollout.ProposeInput{CandidateSnapshotID: candID, BaselineSnapshotID: baseID, CreatedBy: "m406-dash-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDecision(ctx, ro.ID, rollout.Decision{Verdict: rollout.VerdictApproved}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ctx, ro.ID, "m406-dash-test", ""); err != nil {
		t.Fatal(err)
	}
	rec = setDefault("")
	if rec.Code != http.StatusOK || !isDefault() {
		t.Fatalf("approved rollout: %d %s", rec.Code, rec.Body.String())
	}

	// The rollout API shows it.
	rh := NewRolloutHandlers(pool)
	rec = httptest.NewRecorder()
	rh.Detail(rec, httptest.NewRequest(http.MethodGet, "/api/admin/rollouts/detail?id="+ro.ID.String(), nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"approved"`) || !strings.Contains(rec.Body.String(), "candidate_performance") {
		t.Errorf("detail: %d %s", rec.Code, rec.Body.String())
	}
}

// Integration (TEST_POSTGRES_DSN; -p 1): the global-model credential and the
// default embedding provider (retrieval, D7) are guarded too. With an
// enabled definition that uses the global model, writing llm/model_name at
// global/default or switching the default embedding provider is refused
// without an approved rollout; nothing is written.
func TestIntegration_CredentialAndEmbeddingPathsAreGuarded(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	var enabled []uuid.UUID
	rows, err := pool.Query(ctx, `SELECT id FROM agent_definitions WHERE enabled`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		enabled = append(enabled, id)
	}
	rows.Close()
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET enabled = false WHERE id = ANY($1)`, enabled); err != nil {
		t.Fatal(err)
	}
	def, emb := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (id, name, enabled, is_default) VALUES ($1, $2, true, true)`, def, "m406-global-"+def.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO embedding_providers (id, name, provider_type, endpoint, model, is_default, enabled)
		VALUES ($1, $2, 'ollama', 'http://example.invalid', 'm406-embed', false, true)`, emb, "m406-embed-"+emb.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM embedding_providers WHERE id = $1`, emb)
		_, _ = pool.Exec(ctx, `DELETE FROM agent_definitions WHERE id = $1`, def)
		_, _ = pool.Exec(ctx, `UPDATE agent_definitions SET enabled = true WHERE id = ANY($1)`, enabled)
	})

	// F2: the guarded write runs inside the guard, so the handler needs its
	// store even when the change is refused (the write is rolled back).
	h := &AdminHandlers{db: pool, store: configstore.New(pool, testMasterKey), agentRegistry: agentregistry.New(pool), promptStore: prompts.NewStore(pool)}
	rec := httptest.NewRecorder()
	body := `{"scope":"global","scope_target":"default","category":"llm","key_name":"model_name","value":"some-other-model"}`
	h.CreateCredential(rec, httptest.NewRequest(http.MethodPost, "/api/admin/credentials/create", strings.NewReader(body)))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "rollout_required") {
		t.Errorf("global model credential: %d %s", rec.Code, rec.Body.String())
	}

	eh := NewEmbeddingProviderHandlers(pool, nil)
	rec = httptest.NewRecorder()
	eh.SetDefaultEmbeddingProvider(rec, httptest.NewRequest(http.MethodPost, "/api/admin/embedding-providers/default?id="+emb.String(), nil))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "rollout_required") {
		t.Errorf("embedding default: %d %s", rec.Code, rec.Body.String())
	}
	var isDefault bool
	_ = pool.QueryRow(ctx, `SELECT is_default FROM embedding_providers WHERE id = $1`, emb).Scan(&isDefault)
	if isDefault {
		t.Error("a refused embedding default change must not be written")
	}
}
