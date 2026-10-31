package gitremote

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGitInvocations_DashDashBeforeUserURL is a source scan (G1): every git
// invocation that takes a user-controlled repository URL passes "--" before
// it, so a value such as "--upload-pack=..." can never be read as an option.
// Classify already rejects a leading "-"; this is the second fence.
func TestGitInvocations_DashDashBeforeUserURL(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	must := map[string][]string{
		// /clone and /clone-all share cloneArgs.
		"cmd/github-service/main.go": {`return append(args, "--", gitremote.AuthenticatedURL(repo.GitHubURL, token), repo.ClonePath)`},
		"internal/snapshot/resolver.go": {
			`"ls-remote", "--exit-code", "--", origin}`,
			`[]string{"ls-remote", "--", url}`,
		},
		"cmd/onboarding/main.go": {`"git", "ls-remote", "--symref", "--", url, "HEAD")`},
		// pr-coordinator fetches from the clone's origin URL into a
		// scratch repository (contract checks, base drift).
		"cmd/pr-coordinator/contracts.go": {`"fetch", "--quiet", "--no-tags", "--", origin, `},
		"cmd/pr-coordinator/drift.go":     {`"fetch", "--quiet", "--no-tags", "--", origin, `},
	}
	for file, needles := range must {
		src := read(file)
		for _, n := range needles {
			if !strings.Contains(src, n) {
				t.Errorf("%s: missing %s", file, n)
			}
		}
	}
	// No git argument list may put a repository URL straight after the
	// subcommand (the pre-G1 forms).
	forbidden := regexp.MustCompile(`"(clone|ls-remote|fetch)", (repoURL|url|origin|gitremote\.\w+\()|"--no-tags", origin|"--exit-code", origin`)
	for _, file := range []string{"cmd/github-service/main.go", "internal/snapshot/resolver.go", "cmd/onboarding/main.go", "cmd/pr-coordinator/contracts.go", "cmd/pr-coordinator/drift.go"} {
		if m := forbidden.FindString(read(file)); m != "" {
			t.Errorf("%s: git URL argument without a preceding \"--\": %s", file, m)
		}
	}
}
