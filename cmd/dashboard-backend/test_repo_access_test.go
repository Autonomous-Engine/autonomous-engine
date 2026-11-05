package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A full https URL was appended raw to api.github.com/repos/ and always
// 404ed; the check must hit /repos/<owner>/<repo> (F5).
func TestCheckRepoAccessUsesOwnerRepoPath(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if r.URL.Path == "/repos/jhillyerd/enmime" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	res := checkRepoAccess(context.Background(), srv.URL, "tok", "https://github.com/jhillyerd/enmime.git")
	if !res.Accessible || gotPath != "/repos/jhillyerd/enmime" || gotAuth != "Bearer tok" {
		t.Fatalf("res=%+v path=%q auth=%q", res, gotPath, gotAuth)
	}
	if res := checkRepoAccess(context.Background(), srv.URL, "tok", "jhillyerd/missing"); res.Accessible || res.StatusCode != 404 {
		t.Fatalf("missing repo: %+v", res)
	}
}

func TestCheckRepoAccessExplainsNonGitHubAndMissingToken(t *testing.T) {
	res := checkRepoAccess(context.Background(), "http://127.0.0.1:1", "tok", "/repo-cache/mirrors/enmime.git")
	if res.Accessible || res.RemoteKind != "local_path" || !strings.Contains(res.Error, "GitHub") {
		t.Fatalf("local path: %+v", res)
	}
	res = checkRepoAccess(context.Background(), "http://127.0.0.1:1", "", "jhillyerd/enmime")
	if res.Accessible || !strings.Contains(res.Error, "GitHub account") {
		t.Fatalf("no token: %+v", res)
	}
}

// Any key name that means "token" under the github category is a GitHub
// PAT; only the exact name "token" was recognised before (F7, F8).
func TestLooksLikeGitHubTokenKey(t *testing.T) {
	for _, k := range []string{"token", "github token", "GitHub_Token", " pat ", "personal_access_token", "access-token", "PAT"} {
		if !looksLikeGitHubTokenKey(k) {
			t.Errorf("%q should be a token key", k)
		}
	}
	for _, k := range []string{"webhook_secret", "username", "app_id"} {
		if looksLikeGitHubTokenKey(k) {
			t.Errorf("%q should not be a token key", k)
		}
	}
}
