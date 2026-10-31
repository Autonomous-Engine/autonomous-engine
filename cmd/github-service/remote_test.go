package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
)

// M2-01 extracted github-service's token and clone-URL resolution into
// internal/gitremote so product-agent resolves base SHAs the same way. These
// tests pin that github-service's behaviour did not change.

// legacyCloneURL is the inline formula the clone handlers used before the
// extraction, kept verbatim as the reference.
func legacyCloneURL(githubURL, token string) string {
	repoURL := githubURL
	if !strings.HasPrefix(repoURL, "https://") && !strings.HasPrefix(repoURL, "http://") {
		repoURL = "https://github.com/" + repoURL
	}
	if token != "" {
		repoURL = strings.Replace(repoURL, "https://", fmt.Sprintf("https://x-access-token:%s@", token), 1)
	}
	return repoURL
}

// For GitHub remotes (shorthand and https://github.com) the clone URL is
// still exactly the legacy formula's.
func TestCloneURL_MatchesLegacyFormula(t *testing.T) {
	for _, u := range []string{"example-org/ae-test-app", "https://github.com/o/r.git", "http://ghe.local/o/r", "Autonomous-Engine/example-repo"} {
		for _, tok := range []string{"", "ghp_x"} {
			if got, want := gitremote.AuthenticatedURL(u, tok), legacyCloneURL(u, tok); got != want {
				t.Errorf("AuthenticatedURL(%q, %q) = %q, legacy %q", u, tok, got, want)
			}
		}
	}
}

// G1 deliberately departs from the legacy formula for every remote that is
// not on github.com: the legacy formula sent the PAT to any https host and
// rewrote local paths / file:// / ssh to github.com. Now they pass through
// unchanged and never carry the token.
func TestCloneURL_NonGitHubDepartsFromLegacy(t *testing.T) {
	for _, u := range []string{"https://evil.example/o/r.git", "https://github.com.evil.example/o/r", "/repo-cache/x.git", "file:///repo-cache/x.git", "git@github.com:o/r.git"} {
		got := gitremote.AuthenticatedURL(u, "ghp_x")
		if got != u {
			t.Errorf("AuthenticatedURL(%q) = %q, want it unchanged", u, got)
		}
		if strings.Contains(got, "ghp_x") {
			t.Errorf("AuthenticatedURL(%q) carries the token: %q", u, got)
		}
		if legacy := legacyCloneURL(u, "ghp_x"); legacy == got {
			t.Errorf("%q: legacy formula unexpectedly agrees (%q); the test no longer pins the fix", u, legacy)
		}
	}
}

// legacyToken is resolveRepo's pre-extraction token order, verbatim, over a
// resolver function.
func legacyToken(ctx context.Context, resolve func() (string, error), hasStore bool, override, env string) string {
	token := override
	if token == "" && hasStore {
		t, err := resolve()
		if err == nil && t != "" {
			token = t
		}
	}
	if token == "" {
		token = env
	}
	return token
}

type stubResolver struct {
	token string
	err   error
}

func (s stubResolver) ResolveCredential(context.Context, string, string, string, string) (string, error) {
	return s.token, s.err
}

func TestTokenOrder_MatchesLegacy(t *testing.T) {
	ctx := context.Background()
	for _, st := range []*stubResolver{nil, {token: "registry"}, {token: ""}, {err: fmt.Errorf("db down")}} {
		for _, override := range []string{"", "override"} {
			for _, env := range []string{"", "env"} {
				var store gitremote.CredentialResolver
				resolve := func() (string, error) { return "", nil }
				if st != nil {
					store = *st
					s := *st
					resolve = func() (string, error) { return s.token, s.err }
				}
				got := gitremote.ResolveToken(ctx, store, override, env)
				want := legacyToken(ctx, resolve, st != nil, override, env)
				if got != want {
					t.Errorf("store=%+v override=%q env=%q: got %q, legacy %q", st, override, env, got, want)
				}
			}
		}
	}
}

// A nil *ConfigStore must reach gitremote as a nil interface, or ResolveToken
// would call a method on a nil pointer.
func TestCredentials_NilStoreIsNilInterface(t *testing.T) {
	s := &svc{cfg: &config.Config{}}
	if s.credentials() != nil {
		t.Fatal("credentials() returned a non-nil interface for a nil config store")
	}
}

func TestResolveRepo_UsesSharedResolution(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	if strings.Contains(code, `ResolveCredential(ctx, "github"`) || strings.Contains(code, `ResolveCredential(r.Context(), "github"`) {
		t.Error("github-service still resolves the GitHub token inline; use gitremote.ResolveToken")
	}
	if n := strings.Count(code, "gitremote.ResolveToken("); n != 2 {
		t.Errorf("gitremote.ResolveToken used %d times, want 2 (resolveRepo, clone-all)", n)
	}
	if strings.Contains(code, `"https://x-access-token:%s@"`) {
		t.Error("github-service still builds the authenticated URL inline; use gitremote.AuthenticatedURL")
	}
}
