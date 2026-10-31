package guards

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/credmonitor"
	"github.com/google/uuid"
)

// fakeGitHub is a fake GitHub API: GET /user answers 200 for tokens in
// valid, 401 otherwise, and counts requests per token.
type fakeGitHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	valid map[string]bool
	calls map[string]int
}

func newFakeGitHub(t *testing.T, validTokens ...string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{valid: map[string]bool{}, calls: map[string]int{}}
	for _, v := range validTokens {
		f.valid[v] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.calls[tok]++
		ok := f.valid[tok]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"login":"x"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) probe(ctx context.Context, token string) credmonitor.CheckResult {
	return credmonitor.ProbeGitHubAt(ctx, f.srv.URL, token, 5*time.Second)
}

func (f *fakeGitHub) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeGitHub) callsFor(tok string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[tok]
}

// fakeStore resolves github/token through the same scope chain as
// ConfigStore (integration, then global/default) and records status writes.
type fakeStore struct {
	rows      []*configstore.CredentialEntry
	listErr   error
	updates   []statusUpdate
	updateErr error
}

type statusUpdate struct {
	id     uuid.UUID
	status string
}

func (s *fakeStore) ResolveCredential(_ context.Context, category, keyName, _, _ string) (string, error) {
	for _, sc := range [][2]string{{"integration", ""}, {"global", "default"}} {
		for _, r := range s.rows {
			if r.Category == category && r.KeyName == keyName && r.Scope == sc[0] && r.ScopeTarget == sc[1] {
				return r.Value, nil
			}
		}
	}
	return "", errors.New("credential not found")
}

func (s *fakeStore) CredentialsByCategory(_ context.Context, category string) ([]*configstore.CredentialEntry, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*configstore.CredentialEntry
	for _, r := range s.rows {
		if r.Category == category {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) UpdateValidationStatus(_ context.Context, id uuid.UUID, status, errMsg string, _ *time.Time) (string, error) {
	s.updates = append(s.updates, statusUpdate{id, status})
	for _, r := range s.rows {
		if r.ID == id {
			prev := r.LastValidationStatus
			now := time.Now()
			r.LastValidationStatus, r.LastValidationError, r.LastValidatedAt = status, errMsg, &now
			return prev, s.updateErr
		}
	}
	return "", s.updateErr
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time { t := t0.Add(-d); return &t }

func row(scope, target, value, status string, checked *time.Time) *configstore.CredentialEntry {
	return &configstore.CredentialEntry{
		ID: uuid.New(), Scope: scope, ScopeTarget: target, Category: "github", KeyName: "token",
		Value: value, UpdatedAt: t0.Add(-24 * time.Hour),
		LastValidationStatus: status, LastValidatedAt: checked,
	}
}

func newChecker(env map[string]string) *credentialChecker {
	clock := t0
	return &credentialChecker{
		getenv: func(k string) string { return env[k] },
		now:    func() time.Time { return clock },
		memo:   newProbeMemo(),
	}
}

func (c *credentialChecker) checkGitHub(cs credentialStore, gh *fakeGitHub, override string) ValidationResult {
	return c.check(context.Background(), cs, "github", "token", override, gh.probe)
}

// A "valid" recorded longer ago than the TTL is not trusted: GitHub is
// probed, rejects the token, the rejection is recorded, and the guard blocks.
func TestGitHubGuard_StaleValid_ProbedAndRejected(t *testing.T) {
	gh := newFakeGitHub(t) // every token is rejected now
	r := row("global", "default", "ghp_revoked", credmonitor.StatusValid, ago(11*time.Minute))
	cs := &fakeStore{rows: []*configstore.CredentialEntry{r}}

	v := newChecker(nil).checkGitHub(cs, gh, "")
	if v.Valid {
		t.Fatal("a token GitHub rejects must block even though the stale recorded status said valid")
	}
	if gh.callsFor("ghp_revoked") != 1 {
		t.Fatalf("probes = %d, want 1", gh.callsFor("ghp_revoked"))
	}
	if len(cs.updates) != 1 || cs.updates[0].id != r.ID || cs.updates[0].status != credmonitor.StatusInvalid {
		t.Fatalf("recorded = %+v, want one invalid on %s", cs.updates, r.ID)
	}
}

// A "valid" recorded within the TTL is trusted: no probe, no write.
func TestGitHubGuard_FreshValid_NoProbe(t *testing.T) {
	gh := newFakeGitHub(t)
	cs := &fakeStore{rows: []*configstore.CredentialEntry{row("global", "default", "ghp_ok", credmonitor.StatusValid, ago(2*time.Minute))}}

	if v := newChecker(nil).checkGitHub(cs, gh, ""); !v.Valid {
		t.Fatalf("fresh valid must pass: %s", v.Message)
	}
	if gh.total() != 0 || len(cs.updates) != 0 {
		t.Fatalf("probes = %d, writes = %d; want none", gh.total(), len(cs.updates))
	}
}

// A rejection recorded within the TTL blocks without a probe, even if GitHub
// would now accept the token.
func TestGitHubGuard_RejectedWithinTTL_NeverValid(t *testing.T) {
	gh := newFakeGitHub(t, "ghp_flaky")
	r := row("global", "default", "ghp_flaky", credmonitor.StatusInvalid, ago(time.Minute))
	r.LastValidationError = "HTTP 401 Unauthorized"
	cs := &fakeStore{rows: []*configstore.CredentialEntry{r}}

	if v := newChecker(nil).checkGitHub(cs, gh, ""); v.Valid {
		t.Fatal("a token rejected within the TTL must not be reported valid")
	}
	if gh.total() != 0 {
		t.Fatalf("probes = %d, want 0", gh.total())
	}
}

// GITHUB_TOKEN with no registry row (the consumers' fallback) is probed —
// the old guard reported "no credential" — and the outcome is remembered
// for the TTL.
func TestGitHubGuard_EnvTokenNoRow_Probed(t *testing.T) {
	gh := newFakeGitHub(t, "ghp_env")
	cs := &fakeStore{}
	c := newChecker(map[string]string{"GITHUB_TOKEN": "ghp_env"})

	if v := c.checkGitHub(cs, gh, ""); !v.Valid {
		t.Fatalf("a valid env token must pass: %s", v.Message)
	}
	if v := c.checkGitHub(cs, gh, ""); !v.Valid {
		t.Fatalf("second check: %s", v.Message)
	}
	if gh.callsFor("ghp_env") != 1 {
		t.Fatalf("probes = %d, want 1 (second check within the TTL is remembered)", gh.callsFor("ghp_env"))
	}
	if len(cs.updates) != 0 {
		t.Fatalf("an env token has no row to record on, got writes %+v", cs.updates)
	}
}

func TestGitHubGuard_RejectedEnvToken_Blocks(t *testing.T) {
	gh := newFakeGitHub(t)
	c := newChecker(map[string]string{"GITHUB_TOKEN": "ghp_bad_env"})
	if v := c.checkGitHub(nil, gh, ""); v.Valid {
		t.Fatal("a rejected env token must block (also with no store)")
	}
	if v := c.checkGitHub(nil, gh, ""); v.Valid {
		t.Fatal("still rejected within the TTL")
	}
	if gh.callsFor("ghp_bad_env") != 1 {
		t.Fatalf("probes = %d, want 1", gh.callsFor("ghp_bad_env"))
	}
}

// The row judged is the one the consumers resolve (integration, then
// global/default), not the first github/token row of the category.
func TestGitHubGuard_JudgesResolvedRow(t *testing.T) {
	gh := newFakeGitHub(t)
	account := row("github_account", "someone", "ghp_account", credmonitor.StatusInvalid, ago(time.Minute))
	global := row("global", "default", "ghp_global", credmonitor.StatusValid, ago(time.Minute))
	cs := &fakeStore{rows: []*configstore.CredentialEntry{account, global}}

	if v := newChecker(nil).checkGitHub(cs, gh, ""); !v.Valid {
		t.Fatalf("the resolved global token is valid; got blocked by another row: %s", v.Message)
	}

	integration := row("integration", "", "ghp_integration", credmonitor.StatusInvalid, ago(time.Minute))
	cs.rows = []*configstore.CredentialEntry{global, integration}
	if v := newChecker(nil).checkGitHub(cs, gh, ""); v.Valid {
		t.Fatal("the integration row wins the scope chain and is invalid: must block")
	}
}

// Never probed, unknown, or a failed last probe is probed and recorded.
func TestGitHubGuard_UnknownOrFailedLastProbe_Probed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		checked *time.Time
	}{
		{"never", "", nil},
		{"unknown", credmonitor.StatusUnknown, ago(time.Minute)},
		{"network_error", credmonitor.StatusNetworkError, ago(time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub(t, "ghp_ok")
			cs := &fakeStore{rows: []*configstore.CredentialEntry{row("global", "default", "ghp_ok", tc.status, tc.checked)}}
			if v := newChecker(nil).checkGitHub(cs, gh, ""); !v.Valid {
				t.Fatalf("blocked: %s", v.Message)
			}
			if gh.total() != 1 || len(cs.updates) != 1 || cs.updates[0].status != credmonitor.StatusValid {
				t.Fatalf("probes = %d, writes = %+v; want one probe recorded valid", gh.total(), cs.updates)
			}
		})
	}
}

// A value written after the last probe (rotation leaves the status alone)
// is probed even when the recorded rejection is within the TTL.
func TestGitHubGuard_RotatedSinceLastProbe_Probed(t *testing.T) {
	gh := newFakeGitHub(t, "ghp_new")
	r := row("global", "default", "ghp_new", credmonitor.StatusInvalid, ago(5*time.Minute))
	r.UpdatedAt = t0.Add(-time.Minute)
	cs := &fakeStore{rows: []*configstore.CredentialEntry{r}}
	if v := newChecker(nil).checkGitHub(cs, gh, ""); !v.Valid {
		t.Fatalf("the rotated token is valid: %s", v.Message)
	}
	if gh.total() != 1 {
		t.Fatalf("probes = %d, want 1", gh.total())
	}
}

func TestGitHubGuard_TTLFromEnv(t *testing.T) {
	gh := newFakeGitHub(t, "ghp_ok")
	cs := &fakeStore{rows: []*configstore.CredentialEntry{row("global", "default", "ghp_ok", credmonitor.StatusValid, ago(2*time.Minute))}}
	c := newChecker(map[string]string{CredentialProbeTTLEnv: "1m"})
	if v := c.checkGitHub(cs, gh, ""); !v.Valid {
		t.Fatalf("blocked: %s", v.Message)
	}
	if gh.total() != 1 {
		t.Fatalf("with a 1m TTL a 2m-old status must be re-probed; probes = %d", gh.total())
	}
	if d := newChecker(map[string]string{CredentialProbeTTLEnv: "bogus"}).ttl(); d != DefaultCredentialProbeTTL {
		t.Fatalf("bogus TTL = %v, want default", d)
	}
}

// Fail closed: nothing configured anywhere, or the rows cannot be read.
func TestGitHubGuard_FailClosed(t *testing.T) {
	gh := newFakeGitHub(t)
	if v := newChecker(nil).checkGitHub(nil, gh, ""); v.Valid {
		t.Fatal("no store, no env token: must block")
	}
	if v := newChecker(nil).checkGitHub(&fakeStore{}, gh, ""); v.Valid {
		t.Fatal("no row, no env token: must block")
	}
	cs := &fakeStore{rows: []*configstore.CredentialEntry{row("global", "default", "ghp_ok", credmonitor.StatusValid, ago(time.Minute))}, listErr: errors.New("db down")}
	if v := newChecker(nil).checkGitHub(cs, gh, ""); v.Valid {
		t.Fatal("a row lookup failure must block")
	}
}

// A repository override is probed (it has no row) and remembered.
func TestGitHubGuard_Override(t *testing.T) {
	gh := newFakeGitHub(t, "ghp_global")
	cs := &fakeStore{rows: []*configstore.CredentialEntry{row("global", "default", "ghp_global", credmonitor.StatusValid, ago(time.Minute))}}
	c := newChecker(nil)
	if v := c.checkGitHub(cs, gh, "ghp_repo_bad"); v.Valid {
		t.Fatal("the rejected override is what git uses: must block")
	}
	if gh.callsFor("ghp_repo_bad") != 1 || len(cs.updates) != 0 {
		t.Fatalf("override probes = %d, writes = %+v", gh.callsFor("ghp_repo_bad"), cs.updates)
	}
}
