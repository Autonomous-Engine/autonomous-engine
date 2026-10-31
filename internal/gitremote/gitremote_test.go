package gitremote

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeStore struct {
	token string
	err   error
	calls int
}

func (f *fakeStore) ResolveCredential(_ context.Context, category, keyName, agentType, instanceID string) (string, error) {
	f.calls++
	if category != "github" || keyName != "token" || agentType != "" || instanceID != "" {
		return "", errors.New("unexpected lookup " + category + "/" + keyName)
	}
	return f.token, f.err
}

func TestResolveToken_Order(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		store    CredentialResolver
		override string
		env      string
		want     string
	}{
		{"override wins", &fakeStore{token: "registry"}, "override", "env", "override"},
		{"registry before env", &fakeStore{token: "registry"}, "", "env", "registry"},
		{"registry error falls to env", &fakeStore{err: errors.New("boom")}, "", "env", "env"},
		{"empty registry falls to env", &fakeStore{}, "", "env", "env"},
		{"nil store falls to env", nil, "", "env", "env"},
		{"nothing anywhere", nil, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveToken(ctx, tc.store, tc.override, tc.env); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveTokenSource(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		store            CredentialResolver
		override, env    string
		wantTok, wantSrc string
	}{
		{&fakeStore{token: "r"}, "o", "e", "o", SourceOverride},
		{&fakeStore{token: "r"}, "", "e", "r", SourceRegistry},
		{&fakeStore{err: errors.New("x")}, "", "e", "e", SourceEnv},
		{&fakeStore{}, "", "e", "e", SourceEnv},
		{nil, "", "e", "e", SourceEnv},
		{&fakeStore{err: errors.New("x")}, "", "", "", SourceNone},
		{nil, "", "", "", SourceNone},
	}
	for _, tc := range cases {
		tok, src := ResolveTokenSource(ctx, tc.store, tc.override, tc.env)
		if tok != tc.wantTok || src != tc.wantSrc {
			t.Errorf("got (%q, %q), want (%q, %q)", tok, src, tc.wantTok, tc.wantSrc)
		}
	}
}

func TestResolveToken_OverrideSkipsRegistry(t *testing.T) {
	s := &fakeStore{token: "registry"}
	ResolveToken(context.Background(), s, "override", "")
	if s.calls != 0 {
		t.Errorf("registry consulted %d times despite an override", s.calls)
	}
	if _, src := ResolveTokenSource(context.Background(), s, "override", ""); src != SourceOverride || s.calls != 0 {
		t.Errorf("ResolveTokenSource: source %q, registry consulted %d times despite an override", src, s.calls)
	}
}

func TestURLs(t *testing.T) {
	cases := []struct{ in, token, https, auth string }{
		{"example-org/ae-test-app", "", "https://github.com/example-org/ae-test-app", "https://github.com/example-org/ae-test-app"},
		{"example-org/ae-test-app", "tok", "https://github.com/example-org/ae-test-app", "https://x-access-token:tok@github.com/example-org/ae-test-app"},
		{"https://github.com/o/r.git", "tok", "https://github.com/o/r.git", "https://x-access-token:tok@github.com/o/r.git"},
		// http:// is kept as is and never gets a token (a PAT is never sent
		// in clear text, and never to a host other than github.com).
		{"http://example.com/o/r", "tok", "http://example.com/o/r", "http://example.com/o/r"},
	}
	for _, tc := range cases {
		if got := HTTPSURL(tc.in); got != tc.https {
			t.Errorf("HTTPSURL(%q) = %q, want %q", tc.in, got, tc.https)
		}
		if got := AuthenticatedURL(tc.in, tc.token); got != tc.auth {
			t.Errorf("AuthenticatedURL(%q) = %q, want %q", tc.in, got, tc.auth)
		}
	}
}

// TestClassifyTable pins, for every remote form G1 names, the kind, the
// normalized URL, the clone URL HTTPSURL gives git, and whether
// AuthenticatedURL attaches the token. Only a github_https URL whose host is
// exactly github.com may ever carry the PAT.
func TestClassifyTable(t *testing.T) {
	const tok = "tok"
	cases := []struct {
		in    string
		kind  Kind
		norm  string // Classify's normalized URL
		https string // HTTPSURL
		auth  string // AuthenticatedURL(in, tok)
	}{
		{"owner/repo", KindGitHubShorthand, "https://github.com/owner/repo", "https://github.com/owner/repo", "https://x-access-token:tok@github.com/owner/repo"},
		{"https://github.com/o/r.git", KindGitHubHTTPS, "https://github.com/o/r.git", "https://github.com/o/r.git", "https://x-access-token:tok@github.com/o/r.git"},
		// GitHub over plain http: a GitHub remote, but the token is never
		// sent in clear text.
		{"http://github.com/o/r", KindGitHubHTTPS, "http://github.com/o/r", "http://github.com/o/r", "http://github.com/o/r"},
		{"git@github.com:o/r.git", KindGitHubSSH, "git@github.com:o/r.git", "git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"/abs/path.git", KindLocalPath, "/abs/path.git", "/abs/path.git", "/abs/path.git"},
		{"file:///abs/path.git", KindFileURL, "file:///abs/path.git", "file:///abs/path.git", "file:///abs/path.git"},
		{"https://gitlab.example/o/r.git", KindOtherRemote, "https://gitlab.example/o/r.git", "https://gitlab.example/o/r.git", "https://gitlab.example/o/r.git"},
		{"https://github.com.evil.example/o/r", KindOtherRemote, "https://github.com.evil.example/o/r", "https://github.com.evil.example/o/r", "https://github.com.evil.example/o/r"},
		{"https://evil.example/github.com/o/r", KindOtherRemote, "https://evil.example/github.com/o/r", "https://evil.example/github.com/o/r", "https://evil.example/github.com/o/r"},
		{"https://github.com@evil.example/x", KindOtherRemote, "https://github.com@evil.example/x", "https://github.com@evil.example/x", "https://github.com@evil.example/x"},
		{"https://evil.example/o/r.git", KindOtherRemote, "https://evil.example/o/r.git", "https://evil.example/o/r.git", "https://evil.example/o/r.git"},
		// Userinfo already present: a GitHub remote, but the token is not
		// layered on top of someone else's credentials.
		{"https://user@github.com/o/r", KindGitHubHTTPS, "https://user@github.com/o/r", "https://user@github.com/o/r", "https://user@github.com/o/r"},
		// A query or fragment before "@" is where URL parsers disagree on
		// the host; a non-default port is not GitHub's. No token for any.
		{"https://github.com#@evil.example/", KindGitHubHTTPS, "https://github.com#@evil.example/", "https://github.com#@evil.example/", "https://github.com#@evil.example/"},
		{"https://github.com?@evil.example/", KindGitHubHTTPS, "https://github.com?@evil.example/", "https://github.com?@evil.example/", "https://github.com?@evil.example/"},
		{"https://github.com/o/r?x=1", KindGitHubHTTPS, "https://github.com/o/r?x=1", "https://github.com/o/r?x=1", "https://github.com/o/r?x=1"},
		{"https://github.com:8443/o/r", KindGitHubHTTPS, "https://github.com:8443/o/r", "https://github.com:8443/o/r", "https://github.com:8443/o/r"},
		{"https://github.com:443/o/r", KindGitHubHTTPS, "https://github.com:443/o/r", "https://github.com:443/o/r", "https://x-access-token:tok@github.com:443/o/r"},
		// Invalid values normalize to "": HTTPSURL / AuthenticatedURL never
		// hand them to git.
		{"-flag", KindInvalid, "", "", ""},
		{"-oProxyCommand=x", KindInvalid, "", "", ""},
		{"", KindInvalid, "", "", ""},
		{"o/r x", KindInvalid, "", "", ""},
		{"https://github.com/o\n/r", KindInvalid, "", "", ""},
		{"ext::sh -c id", KindInvalid, "", "", ""},
		{"ext::sh%20-c%20id", KindInvalid, "", "", ""},
		// UNC / SMB paths: Windows git would authenticate to the host.
		{"//evil.example/share/r.git", KindInvalid, "", "", ""},
		{`\\evil.example\share\r.git`, KindInvalid, "", "", ""},
		// Surrounding whitespace is trimmed everywhere, token included.
		{" https://github.com/o/r\n", KindGitHubHTTPS, "https://github.com/o/r", "https://github.com/o/r", "https://x-access-token:tok@github.com/o/r"},
		{"  /abs/path.git ", KindLocalPath, "/abs/path.git", "/abs/path.git", "/abs/path.git"},
	}
	for _, tc := range cases {
		kind, norm := Classify(tc.in)
		if kind != tc.kind || norm != tc.norm {
			t.Errorf("Classify(%q) = (%q, %q), want (%q, %q)", tc.in, kind, norm, tc.kind, tc.norm)
		}
		if got := HTTPSURL(tc.in); got != tc.https {
			t.Errorf("HTTPSURL(%q) = %q, want %q", tc.in, got, tc.https)
		}
		if got := AuthenticatedURL(tc.in, tok); got != tc.auth {
			t.Errorf("AuthenticatedURL(%q, tok) = %q, want %q", tc.in, got, tc.auth)
		}
	}
}

func TestClassify_MoreForms(t *testing.T) {
	cases := map[string]Kind{
		"Autonomous-Engine/example-repo": KindGitHubShorthand,
		"  owner/repo  ":                 KindGitHubShorthand, // surrounding space is trimmed
		"https://GitHub.com/o/r":         KindGitHubHTTPS,
		"https://github.com:443/o/r":     KindGitHubHTTPS,
		"ssh://git@github.com/o/r.git":   KindGitHubSSH,
		"git@gitlab.example:o/r.git":     KindOtherRemote,
		"ssh://git@gitlab.example/o/r":   KindOtherRemote,
		"./rel/path.git":                 KindLocalPath,
		"../rel/path.git":                KindLocalPath,
		`C:\repos\x.git`:                 KindLocalPath,
		"C:/repos/x.git":                 KindLocalPath,
		"just-a-word":                    KindInvalid,
		"ftp://example/o/r":              KindInvalid,
		"https:///nohost":                KindInvalid,
		"o/r\x00":                        KindInvalid,
		"o/\tr":                          KindInvalid,
		"/abs/-path":                     KindLocalPath,
	}
	for in, want := range cases {
		if got, _ := Classify(in); got != want {
			t.Errorf("Classify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKind_IsGitHub(t *testing.T) {
	for _, k := range []Kind{KindGitHubShorthand, KindGitHubHTTPS, KindGitHubSSH} {
		if !k.IsGitHub() {
			t.Errorf("%q.IsGitHub() = false", k)
		}
	}
	for _, k := range []Kind{KindLocalPath, KindFileURL, KindOtherRemote, KindInvalid, ""} {
		if k.IsGitHub() {
			t.Errorf("%q.IsGitHub() = true", k)
		}
	}
}

func TestRedact(t *testing.T) {
	in := "fatal: unable to access 'https://x-access-token:s3cr3t@github.com/o/r/': The requested URL returned error: 403 (s3cr3t)"
	got := Redact(in, "s3cr3t")
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("token survived redaction: %q", got)
	}
	if !strings.Contains(got, "https://***@github.com/o/r/") {
		t.Errorf("userinfo not masked: %q", got)
	}
	if got := Redact("https://user:pw@host/x", ""); strings.Contains(got, "pw") {
		t.Errorf("userinfo without a known token survived: %q", got)
	}
}
