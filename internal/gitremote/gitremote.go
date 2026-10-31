// Package gitremote resolves how a service reaches a repository's GitHub
// remote: which token to use and the URL to give git. It is the resolution
// github-service has always done (resolveRepo and the clone handlers),
// extracted so other services (product-agent's base-SHA resolution, M2-01)
// use the same order instead of a copy that can drift.
package gitremote

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// CredentialResolver is the slice of *configstore.ConfigStore needed here.
// Callers holding a nil *ConfigStore must pass a nil CredentialResolver, not
// the typed nil pointer.
type CredentialResolver interface {
	ResolveCredential(ctx context.Context, category, keyName, agentType, instanceID string) (string, error)
}

// ResolveToken returns the GitHub token for a repository, in github-service's
// order: the repository's token_override; else the credential registry's
// github/token (scope chain resolved by the store; a lookup error is treated
// as "not set"); else envToken (GITHUB_TOKEN, backward compatibility). ""
// means no token is configured anywhere and git runs unauthenticated.
func ResolveToken(ctx context.Context, store CredentialResolver, override, envToken string) string {
	token, _ := ResolveTokenSource(ctx, store, override, envToken)
	return token
}

// Token sources reported by ResolveTokenSource. They name where a token came
// from so errors can point an operator at it without ever showing the value.
const (
	SourceOverride = "override" // repositories.token_override
	SourceRegistry = "registry" // credential_registry github/token
	SourceEnv      = "env"      // GITHUB_TOKEN
	SourceNone     = "none"     // no token anywhere: git runs unauthenticated
)

// ResolveTokenSource is ResolveToken that also reports which source the token
// came from (SourceOverride, SourceRegistry, SourceEnv or SourceNone).
func ResolveTokenSource(ctx context.Context, store CredentialResolver, override, envToken string) (token, source string) {
	if override != "" {
		return override, SourceOverride
	}
	if store != nil {
		if t, err := store.ResolveCredential(ctx, "github", "token", "", ""); err == nil && t != "" {
			return t, SourceRegistry
		}
	}
	if envToken != "" {
		return envToken, SourceEnv
	}
	return "", SourceNone
}

// Kind classifies a repositories.github_url value: which kind of remote it
// names, and therefore whether the GitHub PAT may be attached to it and
// whether the GitHub API (PR create/merge/link) can be used for it.
type Kind string

const (
	// KindGitHubShorthand is "owner/repo"; it expands to
	// https://github.com/owner/repo.
	KindGitHubShorthand Kind = "github_shorthand"
	// KindGitHubHTTPS is an http(s) URL whose host is exactly github.com.
	// Only this kind (over https, without userinfo) ever carries the PAT.
	KindGitHubHTTPS Kind = "github_https"
	// KindGitHubSSH is git@github.com:o/r or ssh://git@github.com/o/r.
	KindGitHubSSH Kind = "github_ssh"
	// KindLocalPath is a filesystem path (absolute, ./ or ../ relative, or a
	// Windows drive path): a local or bare mirror.
	KindLocalPath Kind = "local_path"
	// KindFileURL is a file:// URL.
	KindFileURL Kind = "file_url"
	// KindOtherRemote is every network remote that is not on github.com:
	// http(s) to any other host, ssh / scp-style to any other host, and
	// git://. It never gets the PAT and has no GitHub API.
	KindOtherRemote Kind = "other_remote"
	// KindInvalid is rejected at repository creation: empty, a leading "-"
	// (git option injection), whitespace or control characters inside, a
	// "transport::address" form (ext::, fd::), an unknown scheme, or a value
	// that is none of the forms above.
	KindInvalid Kind = "invalid"
)

// IsGitHub reports whether the remote is on github.com, so the GitHub API
// (PR create/merge/link, webhooks, test-access) applies to it.
func (k Kind) IsGitHub() bool {
	return k == KindGitHubShorthand || k == KindGitHubHTTPS || k == KindGitHubSSH
}

var (
	shorthandRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	transportRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)
	winDriveRE  = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	// scpRE is git's scp-like syntax, [user@]host:path, recognised (as git
	// does) only when no slash comes before the first colon.
	scpRE = regexp.MustCompile(`^(?:[^@/:]+@)?([^@/:]+):(.+)$`)
)

const githubHost = "github.com"

// Classify returns the kind of remote a repositories.github_url value names
// and its normalized form: surrounding whitespace trimmed, and the
// "owner/repo" shorthand expanded to https://github.com/owner/repo. An
// invalid value normalizes to "".
func Classify(githubURL string) (Kind, string) {
	v := strings.TrimSpace(githubURL)
	if v == "" || strings.HasPrefix(v, "-") || transportRE.MatchString(v) {
		return KindInvalid, ""
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return KindInvalid, ""
		}
	}
	switch {
	case strings.HasPrefix(v, "//"), strings.HasPrefix(v, `\\`), strings.HasPrefix(v, `/\`), strings.HasPrefix(v, `\/`):
		// A UNC / SMB path (//host/share, \\host\share): under Windows git
		// opening it can send NTLM credentials to that host.
		return KindInvalid, ""
	case strings.HasPrefix(v, "/"), strings.HasPrefix(v, "./"), strings.HasPrefix(v, "../"), winDriveRE.MatchString(v):
		return KindLocalPath, v
	case strings.Contains(v, "://"):
		if k := classifyURL(v); k != KindInvalid {
			return k, v
		}
		return KindInvalid, ""
	case shorthandRE.MatchString(v):
		return KindGitHubShorthand, "https://" + githubHost + "/" + v
	}
	if m := scpRE.FindStringSubmatch(v); m != nil {
		host := m[1]
		if strings.HasPrefix(host, "-") || strings.HasPrefix(m[2], "-") {
			return KindInvalid, ""
		}
		if strings.EqualFold(host, githubHost) {
			return KindGitHubSSH, v
		}
		return KindOtherRemote, v
	}
	return KindInvalid, ""
}

func classifyURL(v string) Kind {
	u, err := url.Parse(v)
	if err != nil {
		return KindInvalid
	}
	host := u.Hostname()
	if strings.HasPrefix(host, "-") {
		return KindInvalid
	}
	switch u.Scheme {
	case "file":
		return KindFileURL
	case "https", "http":
		if host == "" {
			return KindInvalid
		}
		if strings.EqualFold(host, githubHost) {
			return KindGitHubHTTPS
		}
		return KindOtherRemote
	case "ssh", "git+ssh", "ssh+git":
		if host == "" {
			return KindInvalid
		}
		if strings.EqualFold(host, githubHost) {
			return KindGitHubSSH
		}
		return KindOtherRemote
	case "git":
		if host == "" {
			return KindInvalid
		}
		return KindOtherRemote
	}
	return KindInvalid
}

// HTTPSURL returns the URL to give git for a repositories.github_url value:
// Classify's normalized form, i.e. the value with surrounding whitespace
// trimmed and the "owner/repo" shorthand expanded to
// https://github.com/owner/repo; every other valid value (full URLs, ssh,
// local paths, file://) is otherwise unchanged. An invalid value yields "",
// so it never reaches git as an argument.
func HTTPSURL(githubURL string) string {
	_, n := Classify(githubURL)
	return n
}

// AuthenticatedURL is HTTPSURL with token embedded as the x-access-token user
// (the form github-service clones with, so the clone's origin carries it).
// The URL is the normalized one (HTTPSURL). The token is attached only to an
// https URL whose host is exactly github.com, on the default port, with no
// userinfo, query or fragment of its own; every other value (other hosts,
// http, ssh, local paths, file://, invalid) is returned unauthenticated, so
// the PAT never goes to a host that is not GitHub. An empty token leaves the
// URL unauthenticated. Never log the result; pass anything that may contain
// it through Redact.
func AuthenticatedURL(githubURL, token string) string {
	u := HTTPSURL(githubURL)
	if token == "" || !carriesToken(u) {
		return u
	}
	return fmt.Sprintf("https://x-access-token:%s@", token) + u[len("https://"):]
}

// carriesToken reports whether u may carry the GitHub PAT.
func carriesToken(u string) bool {
	if k, _ := Classify(u); k != KindGitHubHTTPS {
		return false
	}
	// A query or fragment has no place in a clone URL, and "?@" / "#@"
	// are the shapes that make parsers disagree about the host.
	if strings.ContainsAny(u, "?#") {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || p.User != nil || p.Scheme != "https" || !strings.EqualFold(p.Hostname(), githubHost) ||
		p.RawQuery != "" || p.Fragment != "" || p.ForceQuery || p.Opaque != "" {
		return false
	}
	if port := p.Port(); port != "" && port != "443" {
		return false
	}
	// AuthenticatedURL splices after the literal scheme prefix.
	return strings.HasPrefix(strings.ToLower(u), "https://")
}

// userinfo matches the credentials part of an http(s) URL.
var userinfo = regexp.MustCompile(`(https?://)[^/@\s]+@`)

// Redact removes token and any URL userinfo from s, so git output and errors
// can be logged and stored.
func Redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	return userinfo.ReplaceAllString(s, "${1}***@")
}
