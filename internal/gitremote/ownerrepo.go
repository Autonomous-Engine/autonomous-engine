package gitremote

import (
	"fmt"
	"net/url"
	"strings"
)

// NotGitHubError reports a remote that is not on github.com, so nothing
// about it can be asked of the GitHub API.
type NotGitHubError struct {
	Kind Kind
}

func (e *NotGitHubError) Error() string {
	return fmt.Sprintf("repository has no GitHub remote (%s); access is not checked through the GitHub API", e.Kind)
}

// OwnerRepo extracts owner and repo from a GitHub remote in any accepted
// form: owner/repo, https://github.com/owner/repo(.git)(/),
// git@github.com:owner/repo.git or ssh://git@github.com/owner/repo. Any
// other remote is a *NotGitHubError. Callers that build GitHub API paths
// must use this rather than the raw input (F5).
func OwnerRepo(remote string) (owner, repo string, err error) {
	kind, norm := Classify(remote)
	if !kind.IsGitHub() {
		return "", "", &NotGitHubError{Kind: kind}
	}
	var path string
	if kind == KindGitHubSSH && !strings.Contains(norm, "://") {
		path = norm[strings.Index(norm, ":")+1:] // scp-like git@github.com:owner/repo
	} else {
		u, perr := url.Parse(norm)
		if perr != nil {
			return "", "", fmt.Errorf("parse GitHub remote: %w", perr)
		}
		path = u.Path
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("GitHub remote %q does not name owner/repo", remote)
	}
	return parts[0], strings.TrimSuffix(parts[1], "/"), nil
}
