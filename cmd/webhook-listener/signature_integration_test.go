package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
)

// The listener reads the webhook secret the dashboard writes: the
// Integrations page posts scope=global, scope_target=default,
// category=github, key_name=webhook_secret to /api/admin/credentials/create,
// which calls ConfigStore.SetCredential with exactly those values. The
// dashboard and the listener are separate processes with separate
// ConfigStores; a rotation reaches the listener through config_changed.
func TestIntegration_WebhookSecretIsReadFromTheDashboardCredentialStore(t *testing.T) {
	pool := integrationPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mk, _, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	dashboard := configstore.New(pool, mk)
	listener := configstore.New(pool, mk)

	clear := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM credential_registry WHERE category = 'github' AND key_name = 'webhook_secret'`)
	}
	clear()
	t.Cleanup(clear)

	// Nothing stored: the real store's not-found error is recognised, so an
	// env secret applies and, without one, deliveries are refused.
	if _, err := listener.ResolveCredential(ctx, webhookSecretCategory, webhookSecretKey, "", ""); !errors.Is(err, configstore.ErrCredentialNotFound) {
		t.Fatalf("empty store: err = %v, want ErrCredentialNotFound", err)
	}
	if rec := deliver(t, &githubSecretSource{store: listener}, "", false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret anywhere: status = %d, want 503", rec.Code)
	}
	if rec := deliver(t, &githubSecretSource{store: listener, env: "env-secret"}, signature(pingBody, "env-secret"), true); rec.Code != http.StatusOK {
		t.Fatalf("env fallback with an empty store: status = %d, want 200", rec.Code)
	}

	listener.StartWatcher(ctx)
	// Give the watcher time to LISTEN before the first write it must see.
	time.Sleep(500 * time.Millisecond)

	if err := dashboard.SetCredential(ctx, "global", "default", "github", "webhook_secret", "dash-1", true, "GitHub webhook_secret", nil); err != nil {
		t.Fatalf("store secret as the dashboard does: %v", err)
	}
	secrets := &githubSecretSource{store: listener, env: "env-secret"}
	if rec := deliver(t, secrets, signature(pingBody, "dash-1"), true); rec.Code != http.StatusOK {
		t.Fatalf("signed with the dashboard secret: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec := deliver(t, secrets, signature(pingBody, "env-secret"), true); rec.Code != http.StatusUnauthorized {
		t.Fatalf("env secret while a dashboard secret is stored: status = %d, want 401", rec.Code)
	}

	// Rotate from the other process: the listener's cached value is flushed
	// by config_changed, so the new secret applies without a restart.
	if err := dashboard.SetCredential(ctx, "global", "default", "github", "webhook_secret", "dash-2", true, "GitHub webhook_secret", nil); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if rec := deliver(t, secrets, signature(pingBody, "dash-2"), true); rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotated dashboard secret not picked up within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rec := deliver(t, secrets, signature(pingBody, "dash-1"), true); rec.Code != http.StatusUnauthorized {
		t.Errorf("old secret after rotation: status = %d, want 401", rec.Code)
	}
}
