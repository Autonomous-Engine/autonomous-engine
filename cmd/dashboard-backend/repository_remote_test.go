package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// G1: a repository remote that is not a remote git can be given safely is
// rejected with 400 before anything is stored (the handler here has no
// database at all, so reaching the INSERT would panic).
func TestAddRepository_RejectsInvalidRemote(t *testing.T) {
	h := &AdminHandlers{}
	for _, u := range []string{"-oProxyCommand=x", "o/r x", "ext::sh%20-c%20id", "   "} {
		body := `{"product_id":"00000000-0000-0000-0000-000000000001","name":"r","github_url":"` + u + `"}`
		rec := httptest.NewRecorder()
		h.AddRepository(rec, httptest.NewRequest(http.MethodPost, "/api/admin/repositories/create", bytes.NewBufferString(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid repository remote") {
			t.Errorf("github_url %q: %d %q, want 400 invalid repository remote", u, rec.Code, rec.Body.String())
		}
	}
}

// test-access never sends the PAT to the GitHub API for a remote that is not
// on GitHub (the handler has no credential store: reaching it would panic).
func TestRepoAccess_NonGitHubRemote(t *testing.T) {
	h := &AdminHandlers{}
	for _, u := range []string{"/repo-cache/mirror.git", "https://evil.example/o/r.git", "-x"} {
		rec := httptest.NewRecorder()
		h.TestRepoAccess(rec, httptest.NewRequest(http.MethodPost, "/api/admin/repositories/test-access", bytes.NewBufferString(`{"github_url":"`+u+`"}`)))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accessible":false`) {
			t.Errorf("test-access %q: %d %q, want accessible false", u, rec.Code, rec.Body.String())
		}
	}
}
