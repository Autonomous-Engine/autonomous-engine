package snapshot

import (
	"context"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/autonomousengine/autonomous-engine/internal/products"
	"github.com/google/uuid"
)

const (
	shaA = "7af0d7084cd8b7c8a96b50da4311b560bebcb8e8"
	shaB = "0123456789abcdef0123456789abcdef01234567"
)

const originWithToken = "https://x-access-token:ghp_origin@github.com/example-org/ae-test-app"

// fakeGit answers by the git subcommand and records every call. Any command
// that could write to the clone (fetch, update-ref, rev-parse of a
// remote-tracking ref after a fetch, ...) is answered with an error and
// flagged, so a test can assert it never happened.
type fakeGit struct {
	calls [][]string
	// origin / originErr answer `-C <clone> config --get remote.origin.url`.
	origin    string
	originErr error
	// cloneLs / cloneLsErr answer `-C <clone> ls-remote --exit-code ...`.
	cloneLs    string
	cloneLsErr error
	// ls / lsErr answer the github_url `ls-remote <url> <ref>`.
	ls    string
	lsErr error
	// anon, when set, answers an unauthenticated github_url ls-remote (URL
	// without userinfo); a correct resolver never makes one with a token.
	anon *string
	// forbidden lists every disallowed subcommand that was invoked.
	forbidden []string
}

func (f *fakeGit) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	clone := args[0] == "-C"
	sub := args[0]
	if clone {
		sub = args[2]
	}
	switch {
	case sub == "config" && clone && len(args) == 5 && args[3] == "--get" && args[4] == "remote.origin.url":
		return f.origin, f.originErr
	case sub == "ls-remote" && clone:
		return f.cloneLs, f.cloneLsErr
	case sub == "ls-remote":
		if f.anon != nil && !strings.Contains(args[len(args)-2], "@") {
			return *f.anon, nil
		}
		return f.ls, f.lsErr
	}
	f.forbidden = append(f.forbidden, strings.Join(args, " "))
	return "", errors.New("forbidden git " + strings.Join(args, " "))
}

func (f *fakeGit) joined() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func testRepo() products.Repository {
	return products.Repository{
		ID: uuid.MustParse("5034a9a4-316c-4900-9f31-0ca0114fa482"), Name: "ae-test-app",
		GitHubURL: "example-org/ae-test-app", BaseBranch: "main", ClonePath: "/repo-cache/p/ae-test-app/",
	}
}

// Step 1: read the clone's origin URL and ls-remote it. Only reads; no fetch,
// no ref update, so a read-only clone is fine.
func TestResolve_CloneOriginLsRemoteWritesNothing(t *testing.T) {
	g := &fakeGit{origin: originWithToken + "\n", cloneLs: shaA + "\trefs/heads/main\n"}
	r := &Resolver{Run: g.run, Token: func(context.Context, products.Repository) string { return "ghp_resolved" }}
	res, err := r.ResolveDetailed(context.Background(), testRepo())
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA != shaA || res.Method != "clone-origin" || res.Ref != "main" {
		t.Fatalf("got %+v", res)
	}
	want := []string{
		"-C /repo-cache/p/ae-test-app/ config --get remote.origin.url",
		"-C /repo-cache/p/ae-test-app/ ls-remote --exit-code -- " + originWithToken + " refs/heads/main",
	}
	if got := g.joined(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for _, c := range g.joined() {
		for _, w := range []string{" fetch", "update-ref", "FETCH_HEAD", "pull", "checkout"} {
			if strings.Contains(c, w) {
				t.Fatalf("resolver ran a writing command: %q", c)
			}
		}
	}
	if strings.Contains(res.Detail, "ghp_origin") || !strings.Contains(res.Detail, "github.com/example-org/ae-test-app") {
		t.Fatalf("detail = %q (must name the origin, never its credential)", res.Detail)
	}
}

func TestResolve_CloneOriginFailureFallsBackToGitHubURLWithToken(t *testing.T) {
	g := &fakeGit{origin: originWithToken, cloneLsErr: errors.New("exit status 128: remote: Invalid username or token (ghp_origin)"),
		ls: shaB + "\trefs/heads/main\n"}
	r := &Resolver{Run: g.run, Token: func(context.Context, products.Repository) string { return "ghp_secret" }}
	res, err := r.ResolveDetailed(context.Background(), testRepo())
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA != shaB || res.Method != "ls-remote" {
		t.Fatalf("got %+v", res)
	}
	last := strings.Join(g.calls[len(g.calls)-1], " ")
	if last != "ls-remote -- https://x-access-token:ghp_secret@github.com/example-org/ae-test-app refs/heads/main" {
		t.Fatalf("github_url call = %q", last)
	}
	if strings.Contains(res.Detail, "ghp_secret") || strings.Contains(res.Detail, "ghp_origin") {
		t.Fatalf("detail leaks a credential: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "Invalid username or token") {
		t.Errorf("detail does not say why the clone's origin failed: %q", res.Detail)
	}
	if len(g.forbidden) != 0 {
		t.Fatalf("forbidden commands: %q", g.forbidden)
	}
}

func TestResolve_UnreadableOriginFallsBack(t *testing.T) {
	g := &fakeGit{originErr: errors.New("exit status 1"), ls: shaA + "\trefs/heads/main\n"}
	sha, err := (&Resolver{Run: g.run}).Resolve(context.Background(), testRepo())
	if err != nil || sha != shaA {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	if len(g.calls) != 2 {
		t.Fatalf("calls = %q, want config then github_url ls-remote", g.joined())
	}
}

func TestResolve_NoClonePathGoesStraightToLsRemote(t *testing.T) {
	repo := testRepo()
	repo.ClonePath = ""
	g := &fakeGit{ls: "junk\n" + shaA + "\trefs/heads/main\n"}
	sha, err := (&Resolver{Run: g.run}).Resolve(context.Background(), repo)
	if err != nil || sha != shaA {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	if len(g.calls) != 1 || g.calls[0][0] != "ls-remote" {
		t.Fatalf("calls = %q, want one ls-remote", g.joined())
	}
	if g.calls[0][1] != "--" || g.calls[0][2] != "https://github.com/example-org/ae-test-app" {
		t.Errorf("ls-remote args = %q, want -- then the unauthenticated URL", g.calls[0])
	}
}

// G1: a repository whose remote is not on github.com is asked through
// ls-remote with the URL unchanged and no token, even when a token is
// configured, and the GitHub credential check is not consulted for it.
func TestResolve_NonGitHubRemoteGetsNoToken(t *testing.T) {
	for _, u := range []string{"/repo-cache/mirror.git", "file:///repo-cache/mirror.git", "https://evil.example/o/r.git"} {
		repo := testRepo()
		repo.ClonePath = ""
		repo.GitHubURL = u
		g := &fakeGit{ls: shaA + "\trefs/heads/main\n"}
		checked := false
		r := &Resolver{Run: g.run,
			Token:           func(context.Context, products.Repository) string { return "ghp_secret" },
			CredentialCheck: func(context.Context, products.Repository) error { checked = true; return errors.New("x") }}
		sha, err := r.Resolve(context.Background(), repo)
		if err != nil || sha != shaA {
			t.Fatalf("%s: sha=%q err=%v", u, sha, err)
		}
		if got := g.joined(); len(got) != 1 || got[0] != "ls-remote -- "+u+" refs/heads/main" {
			t.Errorf("%s: calls = %q", u, got)
		}
		g.lsErr = errors.New("unreachable")
		if _, err := r.Resolve(context.Background(), repo); err == nil || strings.Contains(err.Error(), "GitHub credential") || checked {
			t.Errorf("%s: failure err=%v credential checked=%v; want no GitHub credential check", u, err, checked)
		}
	}
	// An invalid remote never reaches git.
	repo := testRepo()
	repo.ClonePath = ""
	repo.GitHubURL = "--upload-pack=touch /tmp/x"
	g := &fakeGit{ls: shaA + "\trefs/heads/main\n"}
	if _, err := (&Resolver{Run: g.run}).Resolve(context.Background(), repo); err == nil || len(g.calls) != 0 {
		t.Fatalf("invalid remote: err=%v calls=%q", err, g.joined())
	}
}

func TestResolve_BothFailNamesRepoBothAttemptsAndCredential(t *testing.T) {
	g := &fakeGit{origin: originWithToken,
		cloneLsErr: errors.New("fatal: Authentication failed for 'https://x-access-token:ghp_origin@github.com/o/r/' (ghp_origin)"),
		lsErr:      errors.New("exit status 128: remote: Invalid username or token (ghp_secret)")}
	r := &Resolver{
		Run:             g.run,
		Token:           func(context.Context, products.Repository) string { return "ghp_secret" },
		CredentialCheck: func(context.Context, products.Repository) error { return errors.New("github token rejected (401)") },
	}
	_, err := r.Resolve(context.Background(), testRepo())
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"ae-test-app", "main", "clone origin ls-remote:", "Authentication failed", "github_url ls-remote:", "Invalid username or token", "github token rejected (401)", "Admin → Credentials"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "ghp_secret") || strings.Contains(msg, "ghp_origin") {
		t.Fatalf("error leaks a credential: %q", msg)
	}
}

// The failure names where the token came from, never its value.
func TestResolve_FailureNamesTokenSourceNotValue(t *testing.T) {
	g := &fakeGit{origin: originWithToken, cloneLsErr: errors.New("exit status 128"),
		lsErr: errors.New("exit status 128: remote: Invalid username or token")}
	r := &Resolver{Run: g.run, TokenSource: func(context.Context, products.Repository) (string, string) { return "ghp_reg", "registry" }}
	_, err := r.Resolve(context.Background(), testRepo())
	if err == nil || !strings.Contains(err.Error(), "(token source: registry)") || strings.Contains(err.Error(), "ghp_reg") {
		t.Fatalf("err = %v", err)
	}
	if last := strings.Join(g.calls[len(g.calls)-1], " "); !strings.Contains(last, "x-access-token:ghp_reg@") {
		t.Fatalf("TokenSource token not used: %q", last)
	}
}

// A rejected token is a resolution failure (the D8 path): there is no
// unauthenticated retry.
func TestResolve_RejectedTokenFailsWithoutAnonymousRetry(t *testing.T) {
	anon := shaA + "\trefs/heads/main\n"
	g := &fakeGit{origin: originWithToken, cloneLsErr: errors.New("exit status 128: auth"),
		lsErr: errors.New("exit status 128: remote: Invalid username or token"), anon: &anon}
	r := &Resolver{Run: g.run, Token: func(context.Context, products.Repository) string { return "ghp_bad" }}
	if _, err := r.Resolve(context.Background(), testRepo()); err == nil || !strings.Contains(err.Error(), "Invalid username or token") {
		t.Fatalf("err = %v, want the rejected-token failure", err)
	}
	if len(g.calls) != 3 {
		t.Fatalf("calls = %q, want config, clone ls-remote, one authenticated github_url ls-remote", g.joined())
	}
}

// Without a clone and with ls-remote failing there is exactly one attempt.
func TestResolve_NoTokenNoSecondAttempt(t *testing.T) {
	repo := testRepo()
	repo.ClonePath = ""
	g := &fakeGit{lsErr: errors.New("repository not found")}
	if _, err := (&Resolver{Run: g.run}).Resolve(context.Background(), repo); err == nil {
		t.Fatal("want an error")
	}
	if len(g.calls) != 1 {
		t.Fatalf("calls = %q, want one ls-remote", g.joined())
	}
}

func TestResolve_RejectsBadInput(t *testing.T) {
	g := &fakeGit{}
	r := &Resolver{Run: g.run}
	for _, base := range []string{"", "--upload-pack=x", "a b"} {
		repo := testRepo()
		repo.BaseBranch = base
		if _, err := r.Resolve(context.Background(), repo); err == nil {
			t.Errorf("base %q: want an error", base)
		}
	}
	if len(g.calls) != 0 {
		t.Fatalf("git ran for invalid input: %q", g.joined())
	}
	// An origin URL that would be parsed as an option is refused.
	g = &fakeGit{origin: "--upload-pack=touch /tmp/x", ls: shaA + "\trefs/heads/main\n"}
	if sha, err := (&Resolver{Run: g.run}).Resolve(context.Background(), testRepo()); err != nil || sha != shaA {
		t.Fatalf("sha=%q err=%v", sha, err)
	}
	for _, c := range g.joined() {
		if strings.Contains(c, "upload-pack") && strings.Contains(c, "ls-remote") {
			t.Fatalf("option-like origin reached ls-remote: %q", c)
		}
	}
}

// G1 with real git: a repository whose github_url is a local bare mirror
// (git init --bare) resolves through step 2 when the clone's origin cannot
// be read (no clone there), and gets the mirror's SHA — the path is no
// longer rewritten to https://github.com/<path>.
func TestResolve_RealGitLocalBareMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(wd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare := filepath.Join(dir, "mirror.git")
	work := filepath.Join(dir, "work")
	git(dir, "init", "--bare", bare)
	git(dir, "init", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "one")
	git(work, "push", "-q", bare, "HEAD:refs/heads/main")
	want := git(work, "rev-parse", "HEAD")

	repo := testRepo()
	repo.GitHubURL = bare
	repo.ClonePath = filepath.Join(dir, "no-clone-here") // step 1 cannot read an origin
	res, err := (&Resolver{Token: func(context.Context, products.Repository) string { return "ghp_secret" }}).ResolveDetailed(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.SHA != want || res.Method != "ls-remote" {
		t.Fatalf("got %+v, want %s via ls-remote", res, want)
	}
	if strings.Contains(res.Detail, "ghp_secret") || strings.Contains(res.Detail, "github.com") {
		t.Errorf("detail %q", res.Detail)
	}
}

func TestResolve_RejectsNonCommitOutput(t *testing.T) {
	g := &fakeGit{origin: originWithToken, cloneLs: "abc123\trefs/heads/main\n", ls: "\n"}
	_, err := (&Resolver{Run: g.run}).Resolve(context.Background(), testRepo())
	if err == nil || !strings.Contains(err.Error(), "not a commit id") || !strings.Contains(err.Error(), "does not exist on the remote") {
		t.Fatalf("err = %v", err)
	}
}

// With real git: a clone whose .git is read-only resolves through its origin,
// and nothing under .git changes (no FETCH_HEAD, no ref moved).
func TestResolve_RealGitReadOnlyCloneIsUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(wd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	work := filepath.Join(dir, "work")
	remote := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")
	git(dir, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "one")
	git(dir, "clone", "-q", "--bare", work, remote)
	git(dir, "clone", "-q", remote, clone)
	// The remote moves on after the clone was made: the clone's own
	// origin/main is now stale, and the resolver must report the remote's tip.
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(work, "commit", "-q", "-am", "two")
	git(work, "push", "-q", remote, "main")
	tip := git(remote, "rev-parse", "main")
	stale := git(clone, "rev-parse", "origin/main")
	if tip == stale {
		t.Fatal("fixture: the clone should be behind the remote")
	}

	gitDir := filepath.Join(clone, ".git")
	before := treeState(t, gitDir)
	setWritable(t, gitDir, false)
	t.Cleanup(func() { setWritable(t, gitDir, true) })

	res, err := (&Resolver{Run: GitRunner}).ResolveDetailed(context.Background(),
		products.Repository{ID: uuid.New(), Name: "fixture", GitHubURL: "o/never-used", BaseBranch: "main", ClonePath: clone})
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA != tip || res.Method != "clone-origin" {
		t.Fatalf("got %+v, want the remote tip %s via the clone's origin", res, tip)
	}
	setWritable(t, gitDir, true)
	if after := treeState(t, gitDir); after != before {
		t.Fatalf(".git changed during resolution:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// treeState lists every file under dir with its size and mtime.
func treeState(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, rel+" "+info.ModTime().Format(time.RFC3339Nano)+" "+itoa(info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// setWritable removes (or restores) the write bits on every directory under
// dir. Best effort: root, and Windows, ignore directory permissions, which is
// why the test also compares the tree before and after.
func setWritable(t *testing.T, dir string, on bool) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		mode := os.FileMode(0o555)
		if on {
			mode = 0o755
		}
		_ = os.Chmod(p, mode)
		return nil
	})
}

func TestSHAFor(t *testing.T) {
	r1, r2 := uuid.New(), uuid.New()
	one := []Pin{{RepositoryID: r1, BaseSHA: shaA}}
	two := []Pin{{RepositoryID: r1, BaseSHA: shaA}, {RepositoryID: r2, BaseSHA: shaB}}
	same := []Pin{{RepositoryID: r1, BaseSHA: shaA}, {RepositoryID: r2, BaseSHA: shaA}}
	cases := []struct {
		name string
		pins []Pin
		repo *uuid.UUID
		want string
	}{
		{"none", nil, nil, ""},
		{"single", one, nil, shaA},
		{"two differ", two, nil, ""},
		{"two agree", same, nil, shaA},
		{"by repo", two, &r2, shaB},
		{"repo not pinned", one, &r2, ""},
	}
	for _, tc := range cases {
		if got := SHAFor(tc.pins, tc.repo); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPinValidate(t *testing.T) {
	ok := Pin{WorkItemID: uuid.New(), RepositoryID: uuid.New(), BaseRef: "main", BaseSHA: shaA}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Pin{
		{RepositoryID: uuid.New(), BaseRef: "main", BaseSHA: shaA},
		{WorkItemID: uuid.New(), RepositoryID: uuid.New(), BaseSHA: shaA},
		{WorkItemID: uuid.New(), RepositoryID: uuid.New(), BaseRef: "main", BaseSHA: strings.ToUpper(shaA)},
		{WorkItemID: uuid.New(), RepositoryID: uuid.New(), BaseRef: "main", BaseSHA: shaA[:12]},
	} {
		if bad.validate() == nil {
			t.Errorf("%+v validated", bad)
		}
	}
}

func TestRegisterUnpinned_SeriesExistsAtZero(t *testing.T) {
	svc := "snapshot-reg-" + uuid.NewString()
	RegisterUnpinned(svc)
	rec := httptest.NewRecorder()
	metrics.Handler()(rec, httptest.NewRequest("GET", "/metrics", nil))
	if want := `ae_snapshot_unpinned_total{service="` + svc + `"} 0`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("metrics output lacks %q", want)
	}
	if n := strings.Count(rec.Body.String(), "# TYPE ae_snapshot_unpinned_total counter"); n != 1 {
		t.Fatalf("TYPE header printed %d times, want 1", n)
	}
}

func TestNoteUnpinned_LogsOncePerItemCountsEveryRead(t *testing.T) {
	svc := "snapshot-test-" + uuid.NewString()
	item := uuid.New()
	if !NoteUnpinned(svc, item, "test") {
		t.Fatal("first read did not log")
	}
	if NoteUnpinned(svc, item, "test") {
		t.Fatal("second read of the same item logged again")
	}
	if !NoteUnpinned(svc, uuid.New(), "test") {
		t.Fatal("another item did not log")
	}
	if got := UnpinnedCount(svc); got != 3 {
		t.Fatalf("ae_snapshot_unpinned_total{service=%q} = %d, want 3", svc, got)
	}
}
