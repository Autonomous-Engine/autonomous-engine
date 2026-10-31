package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/rollout"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var testMasterKey = []byte("0123456789abcdef0123456789abcdef")

// F2: the guarded credential change carries the real write as Apply (it runs
// on the guard's transaction), not only the overlay.
func TestCredentialChange_WriteIsTheGuardsApply(t *testing.T) {
	model := "m"
	called := false
	ch := credentialChange("credentials.create", "global", "default", "llm", "model_name",
		&rollout.GlobalModelOverlay{ModelID: &model},
		func(context.Context, pgx.Tx) error { called = true; return nil })
	if ch.Apply == nil || ch.Overlay == nil {
		t.Fatalf("change = %+v, want Apply and Overlay", ch)
	}
	_ = ch.Apply(context.Background(), nil)
	if !called {
		t.Error("Apply is not the credential write")
	}
	if ch.Details["key_name"] != "model_name" || ch.Details["scope"] != "global" {
		t.Errorf("details = %v", ch.Details)
	}
}

// The guarded branches write with the *Tx variants inside the guard; the
// plain store calls remain only for credentials the guard passes through.
func TestCredentialHandlers_GuardedWriteIsTransactional(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "admin_handlers.go"))
	if err != nil {
		t.Fatal(err)
	}
	code := strings.ReplaceAll(string(src), "\r\n", "\n")
	for _, re := range []string{
		`h\.guardCredential\(w, r, "credentials\.create"[^\n]*\n\s*func\(ctx context\.Context, tx pgx\.Tx\) error \{[^}]*\}\s*return h\.store\.SetCredentialTx\(ctx, tx,`,
		`if !guarded \{\s*err := h\.store\.SetCredential\(`,
		`h\.guardCredential\(w, r, "credentials\.update"[^\n]*\n\s*func\(ctx context\.Context, tx pgx\.Tx\) error \{[^}]*\}\s*err := h\.store\.RotateCredentialTx\(ctx, tx, credID`,
		`if !guarded \{\s*if err := h\.store\.RotateCredential\(`,
	} {
		if !regexp.MustCompile(re).MatchString(code) {
			t.Errorf("admin_handlers.go does not match %s", re)
		}
	}
}

// Integration (TEST_POSTGRES_DSN; -p 1): a guarded credential write, its
// credential_audit_log row and the override row commit together. A write
// that fails (a trigger rejects it) leaves no audit row of either kind; an
// allowed one leaves all three. Rotation is guarded the same way.
func TestIntegration_GuardedCredentialWriteCommitsWithItsAudit(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	const reason = "f2-guard-test"
	var existing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM credential_registry
		WHERE scope = 'global' AND scope_target = 'default' AND category = 'llm' AND key_name = 'model_name'`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Skip("the test database already has a global llm/model_name credential")
	}

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
	def := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (id, name, enabled, is_default) VALUES ($1, $2, true, true)`, def, "f2-global-"+def.String()[:8]); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE OR REPLACE FUNCTION f2_fail_credential_fn() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'f2 injected credential write failure';
		END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS f2_fail_credential_trg ON credential_registry`,
		`CREATE TRIGGER f2_fail_credential_trg BEFORE INSERT OR UPDATE ON credential_registry
			FOR EACH ROW WHEN (NEW.description = 'f2-fail') EXECUTE FUNCTION f2_fail_credential_fn()`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DROP TRIGGER IF EXISTS f2_fail_credential_trg ON credential_registry`)
		_, _ = pool.Exec(c, `DROP FUNCTION IF EXISTS f2_fail_credential_fn()`)
		_, _ = pool.Exec(c, `DELETE FROM credential_audit_log WHERE credential_id IN (SELECT id FROM credential_registry
			WHERE scope = 'global' AND scope_target = 'default' AND category = 'llm' AND key_name = 'model_name')`)
		_, _ = pool.Exec(c, `DELETE FROM credential_registry
			WHERE scope = 'global' AND scope_target = 'default' AND category = 'llm' AND key_name = 'model_name'`)
		_, _ = pool.Exec(c, `DELETE FROM rollout_overrides WHERE override_reason = $1`, reason)
		_, _ = pool.Exec(c, `DELETE FROM agent_definitions WHERE id = $1`, def)
		_, _ = pool.Exec(c, `UPDATE agent_definitions SET enabled = true WHERE id = ANY($1)`, enabled)
	})

	h := &AdminHandlers{db: pool, store: configstore.New(pool, testMasterKey), agentRegistry: agentregistry.New(pool), promptStore: prompts.NewStore(pool)}
	audits := func() int {
		return countRows(t, ctx, pool, `SELECT count(*) FROM credential_audit_log WHERE metadata->>'key_name' = 'model_name' AND metadata->>'category' = 'llm'`)
	}
	overrides := func() int {
		return countRows(t, ctx, pool, `SELECT count(*) FROM rollout_overrides WHERE override_reason = '`+reason+`'`)
	}
	credentials := func() int {
		return countRows(t, ctx, pool, `SELECT count(*) FROM credential_registry
			WHERE scope = 'global' AND scope_target = 'default' AND category = 'llm' AND key_name = 'model_name'`)
	}
	create := func(description string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body := `{"scope":"global","scope_target":"default","category":"llm","key_name":"model_name","value":"f2-model","description":"` + description + `"}`
		h.CreateCredential(rec, httptest.NewRequest(http.MethodPost, "/api/admin/credentials/create?override_reason="+reason, strings.NewReader(body)))
		return rec
	}
	audits0, overrides0 := audits(), overrides()

	// The write fails inside the guard: nothing is recorded.
	if rec := create("f2-fail"); rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "nothing changed") {
		t.Fatalf("failing write: %d %s", rec.Code, rec.Body.String())
	}
	if c, a, o := credentials(), audits(), overrides(); c != 0 || a != audits0 || o != overrides0 {
		t.Fatalf("after a failed write: credentials=%d audits=%d (was %d) overrides=%d (was %d)", c, a, audits0, o, overrides0)
	}

	// Allowed with an override: credential, credential audit and override
	// row all commit.
	if rec := create("f2 ok"); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	// One override row per surface entry the change alters (the default
	// and the enabled default definition).
	overrides1 := overrides()
	if c, a := credentials(), audits(); c != 1 || a != audits0+1 || overrides1 <= overrides0 {
		t.Fatalf("after create: credentials=%d audits=%d (was %d) overrides=%d (was %d)", c, a, audits0, overrides1, overrides0)
	}
	var id uuid.UUID
	var ct, nonce []byte
	if err := pool.QueryRow(ctx, `SELECT id, encrypted_value, nonce FROM credential_registry
		WHERE scope = 'global' AND scope_target = 'default' AND category = 'llm' AND key_name = 'model_name'`).Scan(&id, &ct, &nonce); err != nil {
		t.Fatal(err)
	}
	if v, err := crypto.Decrypt(testMasterKey, id, ct, nonce); err != nil || v != "f2-model" {
		t.Fatalf("stored value = %q, %v", v, err)
	}

	// Rotation goes through the guard too.
	rec := httptest.NewRecorder()
	h.UpdateCredential(rec, httptest.NewRequest(http.MethodPut, "/api/admin/credentials/update?id="+id.String()+"&override_reason="+reason,
		strings.NewReader(`{"value":"f2-model-2"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	overrides2 := overrides()
	if a := audits(); a != audits0+2 || overrides2 <= overrides1 {
		t.Fatalf("after rotate: audits=%d overrides=%d (was %d)", a, overrides2, overrides1)
	}
	if err := pool.QueryRow(ctx, `SELECT encrypted_value, nonce FROM credential_registry WHERE id = $1`, id).Scan(&ct, &nonce); err != nil {
		t.Fatal(err)
	}
	if v, err := crypto.Decrypt(testMasterKey, id, ct, nonce); err != nil || v != "f2-model-2" {
		t.Fatalf("rotated value = %q, %v", v, err)
	}

	// Refused without an override: nothing is written.
	rec = httptest.NewRecorder()
	h.UpdateCredential(rec, httptest.NewRequest(http.MethodPut, "/api/admin/credentials/update?id="+id.String(), strings.NewReader(`{"value":"f2-model-3"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("refused update: %d %s", rec.Code, rec.Body.String())
	}
	if a, o := audits(), overrides(); a != audits0+2 || o != overrides2 {
		t.Fatalf("after a refused rotate: audits=%d overrides=%d", a, o)
	}
}

func countRows(t *testing.T, ctx context.Context, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}
