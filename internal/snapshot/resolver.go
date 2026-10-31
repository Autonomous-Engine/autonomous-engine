package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/gitremote"
	"github.com/autonomousengine/autonomous-engine/internal/products"
)

// Runner runs git with args and returns its standard output. An error carries
// git's standard error. Tests inject a fake.
type Runner func(ctx context.Context, args ...string) (stdout string, err error)

// GitRunner is the production Runner: the git binary on PATH, never
// prompting for credentials.
func GitRunner(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return stdout.String(), err
		}
		return stdout.String(), fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.String(), nil
}

// Resolution is a resolved base: the SHA, the ref it came from, and how.
type Resolution struct {
	SHA string
	Ref string
	// Method is "clone-origin" (ls-remote against the clone's
	// remote.origin.url) or "ls-remote" (against repositories.github_url
	// with the resolved token).
	Method string
	// Detail is a one-line, token-free account for the resolved event.
	Detail string
}

// Resolver turns a repository's base branch into a commit (M2-01 ruling):
//
//  1. when the repository has a clone_path: read the clone's
//     remote.origin.url (git -C <clone> config --get remote.origin.url; it
//     carries the credential the clone was made with) and ask that remote
//     directly: git -C <clone> ls-remote --exit-code <origin-url>
//     refs/heads/<base>;
//  2. when there is no clone_path or step 1 fails: git ls-remote
//     <github_url> refs/heads/<base>, with the token resolved the way
//     github-service does (gitremote.ResolveToken).
//
// There is no unauthenticated retry. Nothing is ever written: no fetch, no
// ref update, no FETCH_HEAD, so a read-only clone (product-agent mounts
// repo-cache read-only) resolves exactly like a writable one, and the clone's
// HEAD, refs and working tree are never touched.
type Resolver struct {
	Run Runner
	// Token returns the GitHub token for the repository ("" for none). nil
	// means no token.
	Token func(ctx context.Context, repo products.Repository) string
	// TokenSource, when set, is used instead of Token and also names where
	// the token came from (gitremote.SourceOverride, ...). A failure of the
	// github_url step then names that source — never the value — so the
	// operator knows which credential to fix.
	TokenSource func(ctx context.Context, repo products.Repository) (token, source string)
	// CredentialCheck, when set, is consulted only after both steps failed,
	// so the error can name a missing, expired or rejected credential (the
	// D8 path) instead of a bare git failure.
	CredentialCheck func(ctx context.Context, repo products.Repository) error
	// Timeout bounds each git command; zero means DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout bounds one git command of a resolution.
const DefaultTimeout = 60 * time.Second

// Resolve returns the base SHA of repo (see Resolver).
func (r *Resolver) Resolve(ctx context.Context, repo products.Repository) (string, error) {
	res, err := r.ResolveDetailed(ctx, repo)
	return res.SHA, err
}

// ResolveDetailed is Resolve, also reporting the ref and method used.
func (r *Resolver) ResolveDetailed(ctx context.Context, repo products.Repository) (Resolution, error) {
	ref := strings.TrimSpace(repo.BaseBranch)
	if ref == "" {
		return Resolution{}, fmt.Errorf("repository %s (%s) has no base branch configured", repo.Name, repo.ID)
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n") {
		return Resolution{}, fmt.Errorf("repository %s (%s) has an invalid base branch %q", repo.Name, repo.ID, ref)
	}
	token, source := "", ""
	switch {
	case r.TokenSource != nil:
		token, source = r.TokenSource(ctx, repo)
	case r.Token != nil:
		token = r.Token(ctx, repo)
	}

	var failures []string
	if repo.ClonePath != "" {
		sha, origin, err := r.viaCloneOrigin(ctx, repo.ClonePath, ref)
		if err == nil {
			return Resolution{SHA: sha, Ref: ref, Method: "clone-origin",
				Detail: fmt.Sprintf("refs/heads/%s on %s (origin of %s) via ls-remote", ref, gitremote.Redact(origin, token), repo.ClonePath)}, nil
		}
		failures = append(failures, "clone origin ls-remote: "+gitremote.Redact(err.Error(), token))
	} else {
		failures = append(failures, "clone origin ls-remote: repository has no clone_path")
	}

	kind := repo.Kind()
	switch {
	case repo.GitHubURL == "":
		failures = append(failures, "github_url ls-remote: repository has no github_url")
	case kind == gitremote.KindInvalid:
		failures = append(failures, fmt.Sprintf("github_url ls-remote: invalid remote %q", gitremote.Redact(repo.GitHubURL, token)))
	default:
		// AuthenticatedURL attaches the token only to an https://github.com
		// URL; a local or bare mirror, file:// URL or other host is asked
		// unauthenticated.
		sha, err := r.viaLsRemote(ctx, gitremote.AuthenticatedURL(repo.GitHubURL, token), ref)
		if err == nil {
			return Resolution{SHA: sha, Ref: ref, Method: "ls-remote",
				Detail: fmt.Sprintf("refs/heads/%s on %s via ls-remote (%s)", ref, gitremote.Redact(gitremote.HTTPSURL(repo.GitHubURL), token), failures[0])}, nil
		}
		f := "github_url ls-remote: " + gitremote.Redact(err.Error(), token)
		if source != "" {
			f += " (token source: " + source + ")"
		}
		failures = append(failures, f)
	}

	msg := fmt.Sprintf("could not resolve %s of repository %s: %s", ref, repo.Name, strings.Join(failures, "; "))
	// The GitHub credential has nothing to do with a remote that never gets
	// it (G1).
	if r.CredentialCheck != nil && kind.IsGitHub() {
		if cerr := r.CredentialCheck(ctx, repo); cerr != nil {
			msg += fmt.Sprintf("; GitHub credential: %s — replace or re-authorize it under Admin → Credentials, then retry", gitremote.Redact(cerr.Error(), token))
		}
	}
	return Resolution{}, errors.New(msg)
}

func (r *Resolver) run(ctx context.Context, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := r.Run
	if run == nil {
		run = GitRunner
	}
	return run(cctx, args...)
}

// viaCloneOrigin reads the clone's origin URL and asks that remote for the
// branch tip. Both commands only read: config --get reads .git/config and
// ls-remote talks to the remote without touching the repository. It returns
// the origin URL (unredacted; callers redact) for the event detail.
func (r *Resolver) viaCloneOrigin(ctx context.Context, clonePath, ref string) (sha, origin string, err error) {
	out, err := r.run(ctx, "-C", clonePath, "config", "--get", "remote.origin.url")
	if err != nil {
		return "", "", fmt.Errorf("read remote.origin.url: %w", err)
	}
	origin = strings.TrimSpace(out)
	if origin == "" {
		return "", "", errors.New("the clone has no remote.origin.url")
	}
	if strings.HasPrefix(origin, "-") {
		return "", origin, fmt.Errorf("refusing remote.origin.url %q", gitremote.Redact(origin, ""))
	}
	sha, err = r.lsRemote(ctx, []string{"-C", clonePath, "ls-remote", "--exit-code", "--", origin}, ref)
	if err != nil {
		// The origin's embedded credential may differ from the resolved
		// token; strip it here so no caller can leak it.
		err = errors.New(gitremote.Redact(err.Error(), urlSecret(origin)))
	}
	return sha, gitremote.Redact(origin, urlSecret(origin)), err
}

// urlSecret is the password (or lone user, the x-access-token form puts the
// token in the password) embedded in an http(s) URL, "" when there is none.
func urlSecret(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return ""
	}
	if p, ok := u.User.Password(); ok {
		return p
	}
	return u.User.Username()
}

func (r *Resolver) viaLsRemote(ctx context.Context, url, ref string) (string, error) {
	return r.lsRemote(ctx, []string{"ls-remote", "--", url}, ref)
}

// lsRemote runs prefix + refs/heads/<ref> and returns the SHA of exactly that
// ref.
func (r *Resolver) lsRemote(ctx context.Context, prefix []string, ref string) (string, error) {
	want := "refs/heads/" + ref
	out, err := r.run(ctx, append(append([]string{}, prefix...), want)...)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == want {
			if !ValidSHA(fields[0]) {
				return "", fmt.Errorf("ls-remote returned %q for %s, not a commit id", fields[0], want)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("branch %s does not exist on the remote", ref)
}
