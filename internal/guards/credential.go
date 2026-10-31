package guards

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/credmonitor"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/google/uuid"
)

// credSnapshot is the minimal view the decision logic needs. Separated from
// the full CredentialEntry so the decision function is trivially testable.
type credSnapshot struct {
	ID        uuid.UUID
	Status    string
	LastError string
	ExpiresAt *time.Time
}

// decideCredential is the pure decision function — no I/O, easy to test.
func decideCredential(c *credSnapshot, now time.Time) ValidationResult {
	if c == nil {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"credential"},
			Message: "No credential configured. Add one under Dashboard → Admin → Credentials.",
		}
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.After(now) {
		return ValidationResult{
			Valid:   false,
			Missing: []string{"credential"},
			Message: "Credential expired at " + c.ExpiresAt.Format(time.RFC3339) + ". Rotate it under Dashboard → Admin → Credentials.",
		}
	}
	switch c.Status {
	case credmonitor.StatusInvalid, credmonitor.StatusExpired:
		return ValidationResult{
			Valid:   false,
			Missing: []string{"credential"},
			Message: "Credential is " + c.Status + ": " + c.LastError + ". Rotate it under Dashboard → Admin → Credentials.",
		}
	}
	return ValidationResult{Valid: true}
}

// DefaultCredentialProbeTTL is how long a recorded probe outcome is trusted
// before the guard probes the credential again. CredentialProbeTTLEnv
// overrides it with a Go duration ("5m", "30s"); an unparsable or
// non-positive value falls back to the default.
const (
	DefaultCredentialProbeTTL = 10 * time.Minute
	CredentialProbeTTLEnv     = "CREDENTIAL_PROBE_TTL"
)

// githubTokenEnv is the backward-compatible environment fallback the git
// consumers use (config.GitHub.Token reads the same variable).
const githubTokenEnv = "GITHUB_TOKEN"

// CheckGitHubCredential returns ValidationResult for the GitHub credential
// that git operations will actually use: the token is resolved in the
// consumers' order (gitremote.ResolveTokenSource: repository override →
// credential registry scope chain → GITHUB_TOKEN). A registry token's
// recorded status is trusted only while it is younger than the probe TTL;
// otherwise (stale, never probed, last probe failed, or the value changed
// since) GitHub is probed and the outcome recorded on the row. Override and
// environment tokens have no row; their probe outcomes are remembered in
// process for the same TTL.
func CheckGitHubCredential(ctx context.Context, cs *configstore.ConfigStore, repoTokenOverride string) ValidationResult {
	return checkCategoryCredential(ctx, storeOrNil(cs), "github", "token", repoTokenOverride, func(ctx context.Context, token string) credmonitor.CheckResult {
		return credmonitor.ProbeGitHub(ctx, token, 5*time.Second)
	})
}

// CheckJiraCredential returns ValidationResult for the effective Jira
// credential. Not used for commit-blocking (Jira failures don't stop git
// operations), but available for dashboard and ingestion paths.
func CheckJiraCredential(ctx context.Context, cs *configstore.ConfigStore, jiraBase, emailForProbe string) ValidationResult {
	return checkCategoryCredential(ctx, storeOrNil(cs), "jira", "api_token", "", func(ctx context.Context, token string) credmonitor.CheckResult {
		return credmonitor.ProbeJira(ctx, jiraBase, emailForProbe, token, 5*time.Second)
	})
}

// storeOrNil converts a nil *ConfigStore to a nil interface (not a typed nil).
func storeOrNil(cs *configstore.ConfigStore) credentialStore {
	if cs == nil {
		return nil
	}
	return cs
}

// credentialStore is the slice of *configstore.ConfigStore the guard uses:
// the scope-chain resolution the consumers use, the rows with their recorded
// probe outcomes, and the write path credential-monitor records probes with.
type credentialStore interface {
	gitremote.CredentialResolver
	CredentialsByCategory(ctx context.Context, category string) ([]*configstore.CredentialEntry, error)
	UpdateValidationStatus(ctx context.Context, credentialID uuid.UUID, status, errMsg string, expiresAt *time.Time) (string, error)
}

func checkCategoryCredential(
	ctx context.Context,
	cs credentialStore,
	category, keyName, override string,
	probe func(context.Context, string) credmonitor.CheckResult,
) ValidationResult {
	return defaultCredentialChecker.check(ctx, cs, category, keyName, override, probe)
}

// credentialChecker holds the guard's clock, environment and in-process
// probe memory, so tests can substitute them.
type credentialChecker struct {
	getenv func(string) string
	now    func() time.Time
	memo   *probeMemo
}

var defaultCredentialChecker = &credentialChecker{getenv: os.Getenv, now: time.Now, memo: newProbeMemo()}

func (c *credentialChecker) ttl() time.Duration {
	if raw := c.getenv(CredentialProbeTTLEnv); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return DefaultCredentialProbeTTL
}

// envToken is the environment fallback for category/keyName ("" when there
// is none — only GitHub's token has one).
func (c *credentialChecker) envToken(category, keyName string) string {
	if category == "github" && keyName == "token" {
		return c.getenv(githubTokenEnv)
	}
	return ""
}

// resolve returns the token the consumers would use and where it came from.
func (c *credentialChecker) resolve(ctx context.Context, cs credentialStore, category, keyName, override string) (string, string) {
	var resolver gitremote.CredentialResolver
	if cs != nil {
		resolver = cs
	}
	if category == "github" && keyName == "token" {
		return gitremote.ResolveTokenSource(ctx, resolver, override, c.envToken(category, keyName))
	}
	// Same order for other categories (no environment fallback).
	if override != "" {
		return override, gitremote.SourceOverride
	}
	if resolver != nil {
		if t, err := resolver.ResolveCredential(ctx, category, keyName, "", ""); err == nil && t != "" {
			return t, gitremote.SourceRegistry
		}
	}
	return "", gitremote.SourceNone
}

func (c *credentialChecker) check(
	ctx context.Context,
	cs credentialStore,
	category, keyName, override string,
	probe func(context.Context, string) credmonitor.CheckResult,
) ValidationResult {
	token, source := c.resolve(ctx, cs, category, keyName, override)
	if source == gitremote.SourceNone {
		return decideCredential(nil, c.now())
	}
	ttl := c.ttl()

	if source == gitremote.SourceRegistry {
		rows, err := cs.CredentialsByCategory(ctx, category)
		if err != nil {
			return ValidationResult{Valid: false, Missing: []string{"credential"}, Message: fmt.Sprintf("credential lookup failed: %v", err)}
		}
		if row := resolvedRow(rows, keyName, token); row != nil {
			return c.checkRow(ctx, cs, row, token, ttl, probe)
		}
		// The resolved value matches no row (a cached value the store has
		// since replaced): judge the token itself, as for an env token.
	}
	return c.checkUnrecorded(ctx, category, token, ttl, probe)
}

// resolvedRow returns the registry row ResolveCredential took token from:
// the row with token's value, tried in ResolveCredential's scope order for
// a resolution with no agent type or instance (integration, then
// global/default). Other rows of the category (github_account PATs,
// agent-scoped tokens) are never what the consumers use here.
func resolvedRow(rows []*configstore.CredentialEntry, keyName, token string) *configstore.CredentialEntry {
	for _, sc := range [...]struct{ scope, target string }{{"integration", ""}, {"global", "default"}} {
		for _, r := range rows {
			if r.KeyName == keyName && r.Scope == sc.scope && r.ScopeTarget == sc.target && r.Value == token {
				return r
			}
		}
	}
	return nil
}

// fresh reports whether a recorded outcome can be trusted without a probe:
// a definitive status (valid, invalid, expired) checked within ttl. Never
// probed, unknown, or a failed probe (network_error) is not fresh. A token
// rejected within the TTL is therefore reported invalid without a probe and
// is never reported valid until the TTL has passed or its value changes.
func fresh(status string, checkedAt *time.Time, now time.Time, ttl time.Duration) bool {
	switch status {
	case credmonitor.StatusValid, credmonitor.StatusInvalid, credmonitor.StatusExpired:
	default:
		return false
	}
	return checkedAt != nil && now.Sub(*checkedAt) < ttl
}

func (c *credentialChecker) checkRow(
	ctx context.Context,
	cs credentialStore,
	row *configstore.CredentialEntry,
	token string,
	ttl time.Duration,
	probe func(context.Context, string) credmonitor.CheckResult,
) ValidationResult {
	now := c.now()
	snap := &credSnapshot{
		ID:        row.ID,
		Status:    row.LastValidationStatus,
		LastError: row.LastValidationError,
		ExpiresAt: row.ExpiresAt,
	}
	// A value written after the last probe (SetCredential/RotateCredential
	// leave the status alone) has never been checked.
	rotated := row.LastValidatedAt != nil && row.UpdatedAt.After(*row.LastValidatedAt)
	if !rotated && fresh(row.LastValidationStatus, row.LastValidatedAt, now, ttl) {
		return decideCredential(snap, now)
	}

	res := probe(ctx, token)
	if _, err := cs.UpdateValidationStatus(ctx, row.ID, res.Status, res.Error, res.ExpiresAt); err != nil {
		log.Printf("guards: record %s/%s probe outcome on credential %s: %v", row.Category, row.KeyName, row.ID, err)
	}
	snap.Status, snap.LastError = res.Status, res.Error
	if res.ExpiresAt != nil {
		snap.ExpiresAt = res.ExpiresAt
	}
	return decideCredential(snap, c.now())
}

// checkUnrecorded judges a token with no registry row (a repository
// override or the environment fallback): the outcome is remembered in
// process, keyed by a hash of the token, for ttl.
func (c *credentialChecker) checkUnrecorded(
	ctx context.Context,
	category, token string,
	ttl time.Duration,
	probe func(context.Context, string) credmonitor.CheckResult,
) ValidationResult {
	now := c.now()
	key := sha256.Sum256([]byte(category + "\x00" + token))
	prev, ok := c.memo.get(key)
	if ok && fresh(prev.Status, &prev.CheckedAt, now, ttl) {
		return decideCredential(&credSnapshot{Status: prev.Status, LastError: prev.Error, ExpiresAt: prev.ExpiresAt}, now)
	}
	res := probe(ctx, token)
	res.CheckedAt = now
	c.memo.put(key, res)
	return decideCredential(&credSnapshot{Status: res.Status, LastError: res.Error, ExpiresAt: res.ExpiresAt}, c.now())
}

// probeMemo remembers probe outcomes of tokens that have no registry row.
type probeMemo struct {
	mu sync.Mutex
	m  map[[32]byte]credmonitor.CheckResult
}

func newProbeMemo() *probeMemo { return &probeMemo{m: map[[32]byte]credmonitor.CheckResult{}} }

func (p *probeMemo) get(k [32]byte) (credmonitor.CheckResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.m[k]
	return r, ok
}

// probeMemoMax bounds the in-process memo: entries older than probeMemoMaxAge
// are pruned on every put, and when the map is still full the oldest entry is
// evicted, so distinct override/env tokens seen over a long process lifetime
// cannot grow it without limit.
const (
	probeMemoMax    = 256
	probeMemoMaxAge = 24 * time.Hour
)

func (p *probeMemo) put(k [32]byte, r credmonitor.CheckResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, v := range p.m {
		if now.Sub(v.CheckedAt) > probeMemoMaxAge {
			delete(p.m, key)
		}
	}
	if len(p.m) >= probeMemoMax {
		var oldest [32]byte
		var oldestAt time.Time
		first := true
		for key, v := range p.m {
			if first || v.CheckedAt.Before(oldestAt) {
				oldest, oldestAt, first = key, v.CheckedAt, false
			}
		}
		delete(p.m, oldest)
	}
	p.m[k] = r
}
