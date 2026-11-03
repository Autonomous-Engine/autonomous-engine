package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
)

// A legacy GitHub token saved under any token-like key name is found for
// migration; only the exact name "token" was matched (F8).
func TestIntegration_LegacyGitHubTokenFoundUnderAnyTokenKey(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	store := configstore.New(pool, testMasterKey)
	if err := store.SetCredential(ctx, "global", "default", "github", "github token", "ghp_x", true, "", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM credential_registry WHERE scope='global' AND category='github' AND key_name='github token'`)
	})
	id, found, err := findLegacyGitHubToken(ctx, pool)
	if err != nil || !found || id.String() == "" {
		t.Fatalf("found=%v id=%s err=%v", found, id, err)
	}
}

// Non-secret values came back as "" from the list endpoint (secrets are
// masked), so pages showing stored endpoints and keys were blank (F9).
func TestIntegration_ListCredentialsReturnsNonSecretValues(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	store := configstore.New(pool, testMasterKey)
	if err := store.SetCredential(ctx, "global", "default", "jira", "url", "https://jira.example.test", false, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCredential(ctx, "global", "default", "jira", "api_token", "s3cret", true, "", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM credential_registry WHERE scope='global' AND category='jira' AND key_name IN ('url','api_token')`)
	})
	entries, err := store.ListCredentials(ctx, "jira", "global")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.KeyName] = e.Value
	}
	if got["url"] != "https://jira.example.test" {
		t.Fatalf("url = %q, want the stored value", got["url"])
	}
	if got["api_token"] == "s3cret" || got["api_token"] == "" {
		t.Fatalf("api_token = %q, want masked", got["api_token"])
	}
}

// The Integrations page loaded /api/admin/config/jira-ingest on every
// visit and got a 404: the route did not exist (F11).
func TestIntegration_JiraIngestConfigRoute(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	store := configstore.New(pool, testMasterKey)
	if err := store.SetConfig(ctx, "global", "default", "ingest_jql", "project = X", "string", "jira", "", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM system_config WHERE category='jira' AND key_name='ingest_jql'`) })
	h := &AdminHandlers{db: pool, store: store}
	rec := httptest.NewRecorder()
	h.GetJiraIngestConfig(rec, httptest.NewRequest(http.MethodGet, "/api/admin/config/jira-ingest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ingest_jql"] != "project = X" || body["ingest_enabled"] != "" {
		t.Fatalf("body = %v", body)
	}
}
