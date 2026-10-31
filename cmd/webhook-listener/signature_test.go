package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/google/uuid"
)

// The GitHub webhook endpoint verifies X-Hub-Signature-256 against the secret
// saved on the dashboard's Integrations page (credential_registry, category
// github, key webhook_secret), falling back to GITHUB_WEBHOOK_SECRET. With no
// secret anywhere it refuses every delivery instead of accepting unsigned
// payloads.

const testWebhookSecret = "test-webhook-secret"

// envSecrets is a source with only GITHUB_WEBHOOK_SECRET set (no credential
// store), which is what the handler tests that are not about the secret use.
func envSecrets(secret string) *githubSecretSource {
	return &githubSecretSource{env: secret}
}

func signature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func signGitHub(req *http.Request, body []byte, secret string) {
	req.Header.Set("X-Hub-Signature-256", signature(body, secret))
}

// fakeCredentials stands in for the configstore. value "" with err nil is
// returned as configstore's not-found error, like the real store.
type fakeCredentials struct {
	mu                  sync.Mutex
	value               string
	err                 error
	gotCategory, gotKey string
	calls               int
}

func (f *fakeCredentials) set(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.value = v
}

func (f *fakeCredentials) ResolveCredential(_ context.Context, category, keyName, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotCategory, f.gotKey = category, keyName
	if f.err != nil {
		return "", f.err
	}
	if f.value == "" {
		return "", fmt.Errorf("%w: category=%s key=%s", configstore.ErrCredentialNotFound, category, keyName)
	}
	return f.value, nil
}

// pingBody is a delivery the handler accepts without touching the database
// (an event type it ignores), so these tests are only about the signature.
var pingBody = []byte(`{"zen":"Design for failure."}`)

func deliver(t *testing.T, secrets *githubSecretSource, sigHeader string, setSig bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(pingBody))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", uuid.NewString())
	if setSig {
		req.Header.Set("X-Hub-Signature-256", sigHeader)
	}
	rec := httptest.NewRecorder()
	githubWebhookHandler(context.Background(), &fakeDB{}, secrets)(rec, req)
	return rec
}

func TestGitHubWebhook_RefusesUnsignedDeliveryWhenNoSecretConfigured(t *testing.T) {
	for name, secrets := range map[string]*githubSecretSource{
		"no store, no env":      {},
		"store empty, no env":   {store: &fakeCredentials{}},
		"store failing, no env": {store: &fakeCredentials{err: errors.New("db down")}},
	} {
		t.Run(name, func(t *testing.T) {
			// An unsigned delivery is refused before the secret is looked up.
			if rec := deliver(t, secrets, "", false); rec.Code != http.StatusUnauthorized {
				t.Fatalf("unsigned delivery with no secret: status = %d, want 401", rec.Code)
			}
			// A signed delivery is refused too: there is nothing to check it
			// against, and the operator is told why.
			rec := deliver(t, secrets, signature(pingBody, "anything"), true)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("signed delivery with no secret: status = %d, want 503", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "webhook secret is not configured") {
				t.Errorf("body = %q, want it to say the secret is not configured", rec.Body.String())
			}
		})
	}
}

func TestGitHubWebhook_UsesTheDashboardStoredSecret(t *testing.T) {
	store := &fakeCredentials{value: "dashboard-secret"}
	secrets := &githubSecretSource{store: store, env: "env-secret"}

	if rec := deliver(t, secrets, signature(pingBody, "dashboard-secret"), true); rec.Code != http.StatusOK {
		t.Fatalf("signed with the dashboard secret: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if store.gotCategory != "github" || store.gotKey != "webhook_secret" {
		t.Errorf("resolved category=%q key=%q, want github/webhook_secret (what the Integrations page saves)", store.gotCategory, store.gotKey)
	}
	// The dashboard value wins over the environment.
	if rec := deliver(t, secrets, signature(pingBody, "env-secret"), true); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed with the env secret while a dashboard secret is set: status = %d, want 401", rec.Code)
	}
}

func TestGitHubWebhook_PicksUpAChangedDashboardSecretWithoutRestart(t *testing.T) {
	store := &fakeCredentials{value: "first"}
	secrets := &githubSecretSource{store: store}
	if rec := deliver(t, secrets, signature(pingBody, "first"), true); rec.Code != http.StatusOK {
		t.Fatalf("first secret: status = %d", rec.Code)
	}
	store.set("second")
	if rec := deliver(t, secrets, signature(pingBody, "second"), true); rec.Code != http.StatusOK {
		t.Errorf("rotated secret: status = %d, want 200", rec.Code)
	}
	if rec := deliver(t, secrets, signature(pingBody, "first"), true); rec.Code != http.StatusUnauthorized {
		t.Errorf("old secret after rotation: status = %d, want 401", rec.Code)
	}
}

func TestGitHubWebhook_FallsBackToTheEnvSecret(t *testing.T) {
	for name, secrets := range map[string]*githubSecretSource{
		"no store":      {env: "env-secret"},
		"store empty":   {store: &fakeCredentials{}, env: "env-secret"},
		"store failing": {store: &fakeCredentials{err: errors.New("db down")}, env: "env-secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := deliver(t, secrets, signature(pingBody, "env-secret"), true); rec.Code != http.StatusOK {
				t.Errorf("signed with the env secret: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestGitHubWebhook_RefusesMissingOrWrongSignature(t *testing.T) {
	secrets := envSecrets(testWebhookSecret)
	cases := map[string]struct {
		header string
		set    bool
	}{
		"missing header":   {set: false},
		"empty header":     {header: "", set: true},
		"wrong secret":     {header: signature(pingBody, "not-the-secret"), set: true},
		"other body":       {header: signature([]byte(`{}`), testWebhookSecret), set: true},
		"sha1 prefix":      {header: strings.Replace(signature(pingBody, testWebhookSecret), "sha256=", "sha1=", 1), set: true},
		"not hex":          {header: "sha256=zzzz", set: true},
		"truncated digest": {header: signature(pingBody, testWebhookSecret)[:20], set: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := deliver(t, secrets, c.header, c.set); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestGitHubWebhook_AcceptsACorrectSignature(t *testing.T) {
	if rec := deliver(t, envSecrets(testWebhookSecret), signature(pingBody, testWebhookSecret), true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// A refused delivery must not reach the outbox or the dead-letter table.
func TestGitHubWebhook_RefusedDeliveryRecordsNothing(t *testing.T) {
	raw := []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"o/r"}}`)
	for name, secrets := range map[string]*githubSecretSource{
		"no secret":       {},
		"wrong signature": envSecrets(testWebhookSecret),
	} {
		t.Run(name, func(t *testing.T) {
			db := &fakeDB{repoID: uuid.New(), watched: true}
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(raw))
			req.Header.Set("X-GitHub-Event", "push")
			signGitHub(req, raw, "not-the-secret")
			rec := httptest.NewRecorder()
			githubWebhookHandler(context.Background(), db, secrets)(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("status = 200, want a refusal")
			}
			if rows := db.outboxRows(t); len(rows) != 0 {
				t.Errorf("outbox rows = %d, want 0", len(rows))
			}
			if db.commits != 0 || len(db.poolSQL) != 0 {
				t.Errorf("commits = %d, pool statements = %v; want nothing recorded", db.commits, db.poolSQL)
			}
		})
	}
}

// An unsigned delivery must not cost a credential-store lookup: junk POSTs
// would otherwise turn into database load before they are refused.
func TestGitHubWebhook_UnsignedDeliveryNeverReadsTheStore(t *testing.T) {
	store := &fakeCredentials{value: testWebhookSecret}
	rec := deliver(t, &githubSecretSource{store: store}, "", false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.calls != 0 {
		t.Errorf("unsigned delivery read the credential store %d times, want 0", store.calls)
	}
}

// Both webhook endpoints cap the body before reading it (GitHub's own limit is
// 25 MB), so an anonymous client cannot stream an unbounded payload into memory.
func TestWebhooks_OversizedBodyIsRefused(t *testing.T) {
	old := maxWebhookBodyBytes
	maxWebhookBodyBytes = 64
	defer func() { maxWebhookBodyBytes = old }()
	big := bytes.Repeat([]byte("x"), 65)

	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(big))
	req.Header.Set("X-GitHub-Event", "push")
	signGitHub(req, big, testWebhookSecret)
	rec := httptest.NewRecorder()
	githubWebhookHandler(context.Background(), &fakeDB{}, envSecrets(testWebhookSecret))(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("github: status = %d, want 413", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/webhook/jira", bytes.NewReader(big))
	rec = httptest.NewRecorder()
	jiraWebhookHandler(context.Background(), &fakeDB{})(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("jira: status = %d, want 413", rec.Code)
	}
}
