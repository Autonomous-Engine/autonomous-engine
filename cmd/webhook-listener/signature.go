package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The dashboard's Integrations page saves the GitHub webhook secret with
// POST /api/admin/credentials/create as scope=global, scope_target=default,
// category=github, key_name=webhook_secret. ResolveCredential walks
// integration/"" and then global/default, so it finds that row.
const (
	webhookSecretCategory = "github"
	webhookSecretKey      = "webhook_secret"
)

// noSecretMessage is the response body for a delivery refused because no
// secret is configured. GitHub shows it on the webhook's "Recent Deliveries"
// page, so it says what to do.
const noSecretMessage = "GitHub webhook secret is not configured: save one on the dashboard " +
	"(Admin -> Integrations -> GitHub -> Webhook Secret) or set GITHUB_WEBHOOK_SECRET; " +
	"unsigned webhooks are refused"

// credentialResolver is the slice of *configstore.ConfigStore the listener
// needs to read the dashboard-stored webhook secret.
type credentialResolver interface {
	ResolveCredential(ctx context.Context, category, keyName, agentType, instanceID string) (string, error)
}

// githubSecretSource resolves the GitHub webhook secret for each delivery:
// the dashboard-stored credential first, then GITHUB_WEBHOOK_SECRET.
//
// Resolution runs per request, so a secret saved or rotated on the dashboard
// applies without a restart: the configstore caches the decrypted value and
// its watcher (LISTEN config_changed) flushes that cache whenever a
// credential is written, so the next delivery re-reads it.
// maxWebhookBodyBytes caps a webhook body before it is read (GitHub's own
// payload limit is 25 MB). A variable so tests can lower it.
var maxWebhookBodyBytes int64 = 25 << 20

type githubSecretSource struct {
	store credentialResolver // nil when MASTER_ENCRYPTION_KEY is unset
	env   string             // GITHUB_WEBHOOK_SECRET
}

// newGitHubSecretSource builds the listener's secret source: the credential
// store (when MASTER_ENCRYPTION_KEY is set, as for every service that reads
// dashboard credentials) with its config_changed watcher running, plus the
// GITHUB_WEBHOOK_SECRET fallback. It logs which secret is in effect at start.
func newGitHubSecretSource(ctx context.Context, pool *pgxpool.Pool, envSecret string) *githubSecretSource {
	src := &githubSecretSource{env: envSecret}
	if raw := os.Getenv("MASTER_ENCRYPTION_KEY"); raw != "" {
		mk, err := crypto.LoadMasterKey(raw)
		if err != nil {
			log.Printf("webhook-listener: MASTER_ENCRYPTION_KEY unusable (%v): the dashboard-stored webhook secret cannot be read", err)
		} else {
			cs := configstore.New(pool, mk)
			cs.StartWatcher(ctx)
			src.store = cs
		}
	} else {
		log.Printf("webhook-listener: MASTER_ENCRYPTION_KEY unset: the dashboard-stored webhook secret cannot be read")
	}
	if _, origin := src.secret(ctx); origin != "" {
		log.Printf("webhook-listener: GitHub webhook signatures are verified with the %s secret", origin)
	} else {
		log.Printf("webhook-listener: no GitHub webhook secret configured: every GitHub delivery is refused " +
			"until one is saved on the dashboard (Admin -> Integrations) or GITHUB_WEBHOOK_SECRET is set")
	}
	return src
}

// secret returns the secret to verify against and where it came from
// ("dashboard" or "env"); value is "" when none is configured anywhere.
func (s *githubSecretSource) secret(ctx context.Context) (value, origin string) {
	if s.store != nil {
		v, err := s.store.ResolveCredential(ctx, webhookSecretCategory, webhookSecretKey, "", "")
		switch {
		case err == nil && v != "":
			return v, "dashboard"
		case err != nil && !errors.Is(err, configstore.ErrCredentialNotFound):
			// The store is unreachable: fall back to the environment rather
			// than fail open. With no env secret the delivery is refused.
			log.Printf("webhook-listener: read dashboard webhook secret: %v (falling back to GITHUB_WEBHOOK_SECRET)", err)
		}
	}
	if s.env != "" {
		return s.env, "env"
	}
	return "", ""
}

// authorizeGitHubDelivery checks X-Hub-Signature-256 on body. It writes the
// refusal and returns false when the delivery must not be processed: 503 when
// no secret is configured (nothing to verify against, so nothing is
// accepted), 401 when the signature is missing or does not match.
func authorizeGitHubDelivery(w http.ResponseWriter, r *http.Request, secrets *githubSecretSource, body []byte) bool {
	// The header is checked before the secret is resolved, so an unsigned
	// request never costs a credential-store lookup.
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		log.Printf("webhook-listener: refused GitHub delivery %q: missing X-Hub-Signature-256", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "missing X-Hub-Signature-256 signature", http.StatusUnauthorized)
		return false
	}
	secret, _ := secrets.secret(r.Context())
	if secret == "" {
		log.Printf("webhook-listener: refused GitHub delivery %q: no webhook secret configured "+
			"(dashboard Integrations page or GITHUB_WEBHOOK_SECRET); unsigned webhooks are not accepted",
			r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, noSecretMessage, http.StatusServiceUnavailable)
		return false
	}
	if !verifyGitHubSignature(body, secret, sig) {
		log.Printf("webhook-listener: refused GitHub delivery %q: invalid signature", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return false
	}
	return true
}

// verifyGitHubSignature validates an X-Hub-Signature-256 header value
// ("sha256=<hex HMAC-SHA256 of body>") in constant time.
func verifyGitHubSignature(body []byte, secret, signature string) bool {
	if secret == "" || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}
