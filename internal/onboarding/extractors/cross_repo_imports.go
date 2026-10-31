package extractors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

// manifestFiles lists the dependency manifests we scan for cross-repo imports.
// Kept intentionally small and well-known — we do not walk the full repo tree
// here because that would balloon cost for little signal.
var manifestFiles = []string{
	"go.mod",
	"package.json",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"requirements.txt",
	"pyproject.toml",
	"Pipfile",
	"Gemfile",
	"composer.json",
}

// DetectCrossRepoImports scans common dependency manifests (go.mod,
// package.json, etc.) for import paths that match any of the siblingURLs
// (other repositories belonging to the same product). Emits a
// QCrossRepoImports candidate for the MapDependenciesStep suggestion panel —
// never auto-saves an edge. The caller is responsible for providing a
// non-nil context even though we do not perform any cancellable work today.
func DetectCrossRepoImports(ctx context.Context, productID uuid.UUID, repoID *uuid.UUID, repoPath string, siblingURLs []string) onboarding.Candidate {
	_ = ctx
	if len(siblingURLs) == 0 {
		return onboarding.NotDetected(productID, repoID, onboarding.QCrossRepoImports,
			"detectCrossRepoImports", "single-repo product — no siblings to match against")
	}
	var bodies []string
	var manifestsFound []string
	for _, rel := range manifestFiles {
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err == nil {
			bodies = append(bodies, string(data))
			manifestsFound = append(manifestsFound, rel)
		}
	}
	if len(bodies) == 0 {
		return onboarding.NotDetected(productID, repoID, onboarding.QCrossRepoImports,
			"detectCrossRepoImports", "no manifest files found")
	}
	matches := MatchImports(strings.Join(bodies, "\n"), siblingURLs)
	if len(matches) == 0 {
		return onboarding.NotDetected(productID, repoID, onboarding.QCrossRepoImports,
			"detectCrossRepoImports", "no sibling-repo imports detected")
	}
	evidence := make([]onboarding.Evidence, 0, len(matches)+len(manifestsFound))
	for _, m := range matches {
		evidence = append(evidence, onboarding.Evidence{Kind: "import", Name: m})
	}
	for _, mf := range manifestsFound {
		evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: mf})
	}
	return onboarding.Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  onboarding.QCrossRepoImports,
		Value:        fmt.Sprintf("This repo imports %d sibling repo(s) in the same product: %s", len(matches), strings.Join(matches, ", ")),
		Confidence:   onboarding.ConfidenceMedium,
		Evidence:     evidence,
		Extractor:    "detectCrossRepoImports",
		UserStatus:   onboarding.StatusPending,
	}
}

// MatchImports returns the org/repo short paths from `siblings` whose
// identifier appears as a substring of `body`. Case-insensitive. Exposed for
// unit testing. Input URLs may be GitHub HTTPS URLs, scoped npm packages
// (e.g. "@acme/ui"), or bare Go module paths; we normalise each to a
// lowercase shortform before scanning.
func MatchImports(body string, siblings []string) []string {
	lower := strings.ToLower(body)
	var out []string
	seen := make(map[string]struct{}, len(siblings))
	for _, url := range siblings {
		short := shortForm(url)
		if short == "" {
			continue
		}
		key := strings.ToLower(short)
		if _, dup := seen[key]; dup {
			continue
		}
		if strings.Contains(lower, key) {
			out = append(out, short)
			seen[key] = struct{}{}
		}
	}
	return out
}

// shortForm strips common URL prefixes / suffixes so a sibling like
// "https://github.com/acme/billing.git" becomes "acme/billing", which is how
// it appears inside a go.mod require line or a package.json scope.
func shortForm(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimPrefix(s, "github.com:")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	return s
}
