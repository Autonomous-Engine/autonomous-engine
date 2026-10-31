package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/products"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareMirror is a bare repository (git init --bare) holding one commit on
// branch, the shape of a local mirror under /repo-cache. It returns the
// mirror's path and the commit.
func bareMirror(t *testing.T, branch string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	work := filepath.Join(root, "work")
	gitT(t, root, "init", "--bare", bare)
	gitT(t, root, "init", work)
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("mirror\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "README")
	gitT(t, work, "commit", "-m", "init")
	gitT(t, work, "push", bare, "HEAD:refs/heads/"+branch)
	return bare, gitT(t, work, "rev-parse", "HEAD")
}

// G1: /clone works against a local bare mirror (path and file:// URL): the
// path is no longer rewritten to https://github.com/<path>, and the token is
// never attached to it.
func TestCloneRepository_LocalBareMirror(t *testing.T) {
	bare, sha := bareMirror(t, "trunk")
	fileURL := "file://" + filepath.ToSlash(bare)
	if !strings.HasPrefix(filepath.ToSlash(bare), "/") {
		fileURL = "file:///" + filepath.ToSlash(bare) // Windows drive path
	}
	for _, remote := range []string{bare, fileURL} {
		clonePath := filepath.Join(t.TempDir(), "clone")
		if err := os.MkdirAll(clonePath, 0o755); err != nil { // the handler creates it first
			t.Fatal(err)
		}
		repo := &products.Repository{Name: "mirror", GitHubURL: remote, BaseBranch: "trunk", ClonePath: clonePath}
		if args := cloneArgs(repo, "ghp_secret"); strings.Contains(strings.Join(args, " "), "ghp_secret") {
			t.Fatalf("clone args for %q carry the token: %v", remote, args)
		}
		out, err := cloneRepository(repo, "ghp_secret")
		if err != nil {
			t.Fatalf("cloneRepository(%q): %v\n%s", remote, err, out)
		}
		if got := gitT(t, clonePath, "rev-parse", "HEAD"); got != sha {
			t.Errorf("clone of %q at %s, want %s", remote, got, sha)
		}
		if origin := gitT(t, clonePath, "config", "--get", "remote.origin.url"); strings.Contains(origin, "ghp_secret") || strings.Contains(origin, "github.com") {
			t.Errorf("clone of %q has origin %q", remote, origin)
		}
	}
}

// A clone made before G1 of a non-GitHub https repository carries the PAT in
// remote.origin.url; resolveRepo scrubs it. A GitHub clone is left alone.
func TestScrubOriginCredentials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, tc := range []struct {
		githubURL, origin, want string
	}{
		{"https://evil.example/o/r.git", "https://x-access-token:ghp_secret@evil.example/o/r.git", "https://evil.example/o/r.git"},
		{"https://github.com/o/r.git", "https://x-access-token:ghp_secret@github.com/o/r.git", "https://x-access-token:ghp_secret@github.com/o/r.git"},
		{"/repo-cache/mirror.git", "/repo-cache/mirror.git", "/repo-cache/mirror.git"},
	} {
		dir := t.TempDir()
		gitT(t, dir, "init", "-q")
		gitT(t, dir, "remote", "add", "origin", tc.origin)
		scrubOriginCredentials(&products.Repository{Name: "r", GitHubURL: tc.githubURL, ClonePath: dir})
		if got := gitT(t, dir, "config", "--get", "remote.origin.url"); got != tc.want {
			t.Errorf("%s: origin = %q, want %q", tc.githubURL, got, tc.want)
		}
	}
}

func TestCloneArgs_DashDashBeforeURL(t *testing.T) {
	repo := &products.Repository{GitHubURL: "o/r", BaseBranch: "main", ClonePath: "/repo-cache/p/r/"}
	got := strings.Join(cloneArgs(repo, "tok"), " ")
	want := "clone --branch main -- https://x-access-token:tok@github.com/o/r /repo-cache/p/r/"
	if got != want {
		t.Errorf("cloneArgs = %q, want %q", got, want)
	}
	repo.GitHubURL = "https://evil.example/o/r.git"
	if got := strings.Join(cloneArgs(repo, "tok"), " "); strings.Contains(got, "tok@") {
		t.Errorf("cloneArgs sends the token to evil.example: %q", got)
	}
}

func TestCloneRepository_RejectsInvalidRemote(t *testing.T) {
	repo := &products.Repository{Name: "x", GitHubURL: "--upload-pack=touch /tmp/pwned", ClonePath: filepath.Join(t.TempDir(), "c")}
	if _, err := cloneRepository(repo, ""); err == nil || !strings.Contains(err.Error(), "invalid remote") {
		t.Fatalf("cloneRepository with an option-shaped remote: err = %v", err)
	}
}

func TestResolveOwnerRepo(t *testing.T) {
	ok := map[string][2]string{
		"o/r":                          {"o", "r"},
		"https://github.com/o/r.git":   {"o", "r"},
		"http://github.com/o/r":        {"o", "r"},
		"https://github.com/o/r/":      {"o", "r"},
		"git@github.com:o/r.git":       {"o", "r"},
		"ssh://git@github.com/o/r.git": {"o", "r"},
	}
	for in, want := range ok {
		o, r, err := resolveOwnerRepo(in)
		if err != nil || o != want[0] || r != want[1] {
			t.Errorf("resolveOwnerRepo(%q) = (%q, %q, %v), want %v", in, o, r, err, want)
		}
	}
	for _, in := range []string{"/repo-cache/x.git", "file:///repo-cache/x.git", "https://gitlab.example/o/r.git", "https://github.com.evil.example/o/r", ""} {
		_, _, err := resolveOwnerRepo(in)
		var ng *noGitHubRemoteError
		if !errors.As(err, &ng) {
			t.Errorf("resolveOwnerRepo(%q) err = %v, want noGitHubRemoteError", in, err)
		}
	}
	if _, _, err := resolveOwnerRepo("https://github.com/onlyowner"); err == nil {
		t.Error("resolveOwnerRepo(https://github.com/onlyowner) did not fail")
	}
}

// A GitHub API handler answers 409 with a clear message for a non-GitHub
// remote instead of calling the API with a garbage owner/repo.
func TestOwnerRepoOrRespond_NonGitHubIs409(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, ok := ownerRepoOrRespond(rec, &products.Repository{Name: "mirror", GitHubURL: "/repo-cache/x.git"}, "PR creation via the GitHub API")
	if ok || rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no GitHub remote") {
		t.Fatalf("ok=%v code=%d body=%q, want 409 no GitHub remote", ok, rec.Code, rec.Body.String())
	}
}
