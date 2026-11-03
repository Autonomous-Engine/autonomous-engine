package gitremote

import (
	"errors"
	"testing"
)

// TestRepoAccess built api.github.com/repos/<raw input>, so a full https URL
// produced /repos/https://github.com/o/r and always 404ed (F5). Every
// GitHub remote form resolves to owner/repo.
func TestOwnerRepoParsesEveryGitHubForm(t *testing.T) {
	for _, tc := range []struct{ in, owner, repo string }{
		{"jhillyerd/enmime", "jhillyerd", "enmime"},
		{"https://github.com/jhillyerd/enmime", "jhillyerd", "enmime"},
		{"https://github.com/jhillyerd/enmime.git", "jhillyerd", "enmime"},
		{"https://github.com/jhillyerd/enmime/", "jhillyerd", "enmime"},
		{"git@github.com:jhillyerd/enmime.git", "jhillyerd", "enmime"},
		{"ssh://git@github.com/jhillyerd/enmime", "jhillyerd", "enmime"},
	} {
		owner, repo, err := OwnerRepo(tc.in)
		if err != nil || owner != tc.owner || repo != tc.repo {
			t.Errorf("OwnerRepo(%q) = %q/%q, %v; want %s/%s", tc.in, owner, repo, err, tc.owner, tc.repo)
		}
	}
}

func TestOwnerRepoRejectsNonGitHubRemotes(t *testing.T) {
	for _, in := range []string{"/repo-cache/mirrors/enmime.git", "file:///srv/x.git", "https://gitlab.com/o/r"} {
		_, _, err := OwnerRepo(in)
		var ng *NotGitHubError
		if !errors.As(err, &ng) {
			t.Errorf("OwnerRepo(%q) err = %v, want *NotGitHubError", in, err)
		}
	}
}
