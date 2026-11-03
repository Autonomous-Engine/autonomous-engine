package main

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
)

// The guard's "before" baseline read the stored value through
// ResolveCredential, whose uncached path logs a "read" audit row — so every
// guarded credential write added a spurious audit entry and the audit
// counts became order-dependent (rerun NEW-6). The baseline uses a peek
// that decrypts the exact row without auditing.
func TestIntegration_PeekCredentialDoesNotAudit(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	store := configstore.New(pool, testMasterKey)
	if err := store.SetCredential(ctx, "global", "default", "llm", "peek_key", "v1", false, "", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM credential_registry WHERE scope='global' AND scope_target='default' AND category='llm' AND key_name='peek_key'`)
	})
	count := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM credential_audit_log WHERE metadata->>'key_name' = 'peek_key'`).Scan(&n)
		return n
	}
	before := count()
	v, found, err := store.PeekCredential(ctx, "global", "default", "llm", "peek_key")
	if err != nil || !found || v != "v1" {
		t.Fatalf("peek = %q found=%v err=%v", v, found, err)
	}
	if _, found, _ := store.PeekCredential(ctx, "global", "default", "llm", "absent_key"); found {
		t.Fatal("absent key reported found")
	}
	if after := count(); after != before {
		t.Fatalf("peek wrote %d audit row(s)", after-before)
	}
}
