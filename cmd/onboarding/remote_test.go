package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/config"
)

// bareMirror creates a bare repository whose HEAD is refs/heads/<branch>
// with one commit, the shape of a local mirror under /repo-cache.
func bareMirror(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "--bare", bare)
	run(bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	run(root, "init", work)
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "README")
	run(work, "commit", "-m", "init")
	run(work, "push", bare, "HEAD:refs/heads/"+branch)
	return bare
}

// A local bare mirror resolves its default branch: the path is no longer
// rewritten to https://github.com/<path>, and the token is not attached.
func TestResolveRemoteDefaultBranch_LocalBareMirror(t *testing.T) {
	bare := bareMirror(t, "trunk")
	for _, u := range []string{bare, "file://" + filepath.ToSlash(bare)} {
		if strings.HasPrefix(u, "file://") && !strings.HasPrefix(filepath.ToSlash(bare), "/") {
			u = "file:///" + filepath.ToSlash(bare) // Windows drive path
		}
		got, err := resolveRemoteDefaultBranch(context.Background(), u, "tok")
		if err != nil {
			t.Fatalf("resolveRemoteDefaultBranch(%q): %v", u, err)
		}
		if got != "trunk" {
			t.Errorf("resolveRemoteDefaultBranch(%q) = %q, want trunk", u, got)
		}
	}
}

func TestResolveRemoteDefaultBranch_RejectsInvalid(t *testing.T) {
	for _, u := range []string{"-oProxyCommand=x", "--upload-pack=touch /tmp/x", "ext::sh%20-c%20id"} {
		if _, err := resolveRemoteDefaultBranch(context.Background(), u, ""); err == nil || !strings.Contains(err.Error(), "invalid repository remote") {
			t.Errorf("resolveRemoteDefaultBranch(%q) err = %v, want invalid repository remote", u, err)
		}
	}
}

// add-repo rejects an invalid remote with 400 before it runs git or needs a
// token or a database.
func TestAddRepoHandler_RejectsInvalidRemote(t *testing.T) {
	s := &server{cfg: &config.Config{}}
	body := `{"product_id":"` + "00000000-0000-0000-0000-000000000001" + `","name":"r","github_url":"-oProxyCommand=x"}`
	rec := httptest.NewRecorder()
	s.addRepoHandler(rec, httptest.NewRequest(http.MethodPost, "/api/onboarding/add-repo", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid repository remote") {
		t.Fatalf("add-repo with -oProxyCommand: %d %q, want 400 invalid repository remote", rec.Code, rec.Body.String())
	}
}

// Onboarding must not keep its own copy of the shorthand rewrite or the
// token injection.
func TestResolveRemoteDefaultBranch_UsesGitremote(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	code := strings.ReplaceAll(string(src), "\r\n", "\n")
	for _, bad := range []string{`"https://x-access-token:"`, `"https://github.com/" + url`} {
		if strings.Contains(code, bad) {
			t.Errorf("onboarding still builds remote URLs inline (%s); use gitremote", bad)
		}
	}
	if !strings.Contains(code, "gitremote.AuthenticatedURL(githubURL, token)") {
		t.Error("resolveRemoteDefaultBranch does not use gitremote.AuthenticatedURL")
	}
}
