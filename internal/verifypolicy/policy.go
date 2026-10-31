// Package verifypolicy owns the per-repository verification policy (M3-03,
// migration 051): which checks a repository requires, what happens when no
// executable check exists, the operator's override of test-runner
// detection, and the paths whose changes require security review.
//
// Policy is fail closed: a repository with no
// stored row, or whose row cannot be read, is governed by Default, which
// blocks a run that has no executable check. Accepting such a run as
// partially_verified (always followed by mandatory human review) requires an
// explicit operator decision recorded with who set it and through which
// surface.
package verifypolicy

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/google/uuid"
)

// NoChecksPolicy is what happens when a repository has no executable check
// (no detected test runner and no operator override).
type NoChecksPolicy string

const (
	// NoChecksBlock fails the final verification gate: the run is recorded
	// as failed and the work item is blocked. The default (D2).
	NoChecksBlock NoChecksPolicy = "block"
	// NoChecksPartialWithHumanReview lets the run complete as
	// partially_verified (or unverified when nothing at all ran); the
	// release gate then sends it to mandatory human review when the product
	// has opted in to partial verification, and blocks it otherwise (D2).
	NoChecksPartialWithHumanReview NoChecksPolicy = "partial_with_human_review"
)

// Source records the surface that set a policy.
type Source string

const (
	SourceOnboarding Source = "onboarding"
	SourceAdmin      Source = "admin"
	// SourceDefault marks the built-in fail-closed policy (no row stored).
	SourceDefault Source = "default"
)

// FlagSecurityReviewRequired is the evidence flag added when a changed file
// matches the repository's security_review_paths (D3).
const FlagSecurityReviewRequired = evidence.FlagSecurityReviewRequired

// Policy is one repository's verification policy.
type Policy struct {
	RepositoryID uuid.UUID `json:"repository_id"`
	// TestCommand overrides detection when non-empty after trimming. nil or
	// blank means "use the detected command".
	TestCommand *string `json:"test_command"`
	// RequiredChecks is the subset of {test, build, lint} the operator
	// requires. Kinds the implementation agent cannot run yet are recorded
	// as not_run checks, so a policy that requires them can never be
	// satisfied vacuously.
	RequiredChecks      []evidence.CheckKind `json:"required_checks"`
	NoChecksPolicy      NoChecksPolicy       `json:"no_checks_policy"`
	SecurityReviewPaths []string             `json:"security_review_paths"`
	SetBy               string               `json:"set_by,omitempty"`
	SetAt               *time.Time           `json:"set_at,omitempty"`
	Source              Source               `json:"source"`
}

// SelectableCheckKinds are the check kinds an operator may require. The
// diagnostic check runs automatically whenever it applies and "other" is
// reserved for run-level failures, so neither is selectable.
var SelectableCheckKinds = []evidence.CheckKind{
	evidence.CheckKindTest,
	evidence.CheckKindBuild,
	evidence.CheckKindLint,
}

// GenericSecurityReviewPaths are the language-independent D3 risk triggers:
// authentication/authorization, credentials and secrets, cryptography,
// sandboxing / command execution, and schema migrations.
var GenericSecurityReviewPaths = []string{
	"auth*",
	"*credential*",
	"*secret*",
	"crypto*",
	"sandbox*",
	"migrations/**",
}

// languageSecurityReviewPaths adds dependency-provenance manifests (D3) per
// language.
var languageSecurityReviewPaths = map[string][]string{
	"go":     {"go.mod", "go.sum"},
	"node":   {"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml"},
	"python": {"requirements*.txt", "pyproject.toml", "poetry.lock", "Pipfile.lock"},
	"php":    {"composer.json", "composer.lock"},
	"rust":   {"Cargo.toml", "Cargo.lock"},
	"java":   {"pom.xml", "build.gradle", "build.gradle.kts"},
	"ruby":   {"Gemfile", "Gemfile.lock"},
	"dotnet": {"*.csproj", "packages.lock.json"},
}

// SuggestedSecurityReviewPaths returns the seed list onboarding and the
// admin editor offer for a repository of the given language ("" for
// unknown): the generic triggers plus that language's dependency manifests.
func SuggestedSecurityReviewPaths(language string) []string {
	out := append([]string{}, GenericSecurityReviewPaths...)
	return append(out, languageSecurityReviewPaths[language]...)
}

// DetectLanguage names the repository's primary language from its manifest
// files ("" when none is recognised). It only seeds suggestions; nothing is
// enforced from it.
func DetectLanguage(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	probes := []struct {
		pattern  string
		language string
	}{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
		{"setup.py", "python"},
		{"composer.json", "php"},
		{"Cargo.toml", "rust"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"build.gradle.kts", "java"},
		{"Gemfile", "ruby"},
		{"*.sln", "dotnet"},
		{"*.csproj", "dotnet"},
	}
	for _, p := range probes {
		matches, _ := filepath.Glob(filepath.Join(repoPath, p.pattern))
		if len(matches) > 0 {
			return p.language
		}
	}
	return ""
}

// Default is the fail-closed policy for a repository without a stored row:
// block when no executable check exists, require tests, and treat the
// generic risk paths as security sensitive.
func Default(repositoryID uuid.UUID) Policy {
	return Policy{
		RepositoryID:        repositoryID,
		RequiredChecks:      []evidence.CheckKind{evidence.CheckKindTest},
		NoChecksPolicy:      NoChecksBlock,
		SecurityReviewPaths: append([]string{}, GenericSecurityReviewPaths...),
		Source:              SourceDefault,
	}
}

// Normalize trims and de-duplicates the list fields and the override, and
// fills an empty no_checks_policy with block. It never relaxes a policy.
func (p Policy) Normalize() Policy {
	if p.TestCommand != nil {
		cmd := strings.TrimSpace(*p.TestCommand)
		if cmd == "" {
			p.TestCommand = nil
		} else {
			p.TestCommand = &cmd
		}
	}
	seen := map[evidence.CheckKind]bool{}
	checks := []evidence.CheckKind{}
	for _, k := range p.RequiredChecks {
		k = evidence.CheckKind(strings.ToLower(strings.TrimSpace(string(k))))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		checks = append(checks, k)
	}
	p.RequiredChecks = checks
	globSeen := map[string]bool{}
	globs := []string{}
	for _, g := range p.SecurityReviewPaths {
		g = strings.TrimSpace(strings.ReplaceAll(g, "\\", "/"))
		if g == "" || globSeen[g] {
			continue
		}
		globSeen[g] = true
		globs = append(globs, g)
	}
	p.SecurityReviewPaths = globs
	if p.NoChecksPolicy == "" {
		p.NoChecksPolicy = NoChecksBlock
	}
	return p
}

// Validate reports the first problem with a policy an operator submitted.
func (p Policy) Validate() error {
	switch p.NoChecksPolicy {
	case NoChecksBlock, NoChecksPartialWithHumanReview:
	default:
		return fmt.Errorf("no_checks_policy %q must be %q or %q", p.NoChecksPolicy, NoChecksBlock, NoChecksPartialWithHumanReview)
	}
	for _, k := range p.RequiredChecks {
		if !selectable(k) {
			return fmt.Errorf("required_checks: %q is not one of test, build, lint", k)
		}
	}
	for _, g := range p.SecurityReviewPaths {
		if err := validGlob(g); err != nil {
			return fmt.Errorf("security_review_paths: %w", err)
		}
	}
	switch p.Source {
	case SourceOnboarding, SourceAdmin, SourceDefault:
	default:
		return fmt.Errorf("source %q must be onboarding, admin or default", p.Source)
	}
	if p.TestCommand != nil && strings.ContainsAny(*p.TestCommand, "\n\r\x00") {
		return errors.New("test_command must be a single line")
	}
	return nil
}

func selectable(k evidence.CheckKind) bool {
	for _, s := range SelectableCheckKinds {
		if k == s {
			return true
		}
	}
	return false
}

// EffectiveTestCommand is the command the final gate runs: the operator
// override when set, otherwise the detected command ("" when none).
func (p Policy) EffectiveTestCommand(detected string) string {
	if p.TestCommand != nil {
		if cmd := strings.TrimSpace(*p.TestCommand); cmd != "" {
			return cmd
		}
	}
	return strings.TrimSpace(detected)
}

// Requires reports whether kind is in RequiredChecks.
func (p Policy) Requires(kind evidence.CheckKind) bool {
	for _, k := range p.RequiredChecks {
		if k == kind {
			return true
		}
	}
	return false
}

// SecurityReviewMatches returns the changed files (sorted, de-duplicated)
// that match any of the policy's security_review_paths.
func (p Policy) SecurityReviewMatches(changedFiles []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range changedFiles {
		if seen[f] {
			continue
		}
		for _, g := range p.SecurityReviewPaths {
			if MatchPath(g, f) {
				seen[f] = true
				out = append(out, f)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// MatchPath reports whether file (a repository-relative path, either
// separator) matches pattern, case-insensitively:
//
//   - a pattern without "/" matches when any single path segment matches it
//     (path.Match syntax), so "auth*" matches "internal/auth/login.go" and
//     "src/authService.ts", and "go.mod" matches "go.mod" and "tools/go.mod";
//   - a pattern with "/" matches a contiguous run of segments ending at the
//     file, starting at any directory depth; "**" matches zero or more
//     segments, so "migrations/**" matches "migrations/001.sql" and
//     "db/migrations/2024/001.sql".
//
// A malformed pattern matches nothing.
func MatchPath(pattern, file string) bool {
	pattern = strings.ToLower(strings.Trim(strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/"), "/"))
	file = strings.ToLower(strings.Trim(strings.ReplaceAll(strings.TrimSpace(file), "\\", "/"), "/"))
	if pattern == "" || file == "" {
		return false
	}
	segs := strings.Split(file, "/")
	if !strings.Contains(pattern, "/") {
		if pattern == "**" {
			return true
		}
		for _, s := range segs {
			if ok, err := path.Match(pattern, s); err == nil && ok {
				return true
			}
		}
		return false
	}
	pats := strings.Split(pattern, "/")
	for start := 0; start < len(segs); start++ {
		if matchSegments(pats, segs[start:]) {
			return true
		}
	}
	return false
}

func matchSegments(pats, segs []string) bool {
	if len(pats) == 0 {
		return len(segs) == 0
	}
	if pats[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pats[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pats[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pats[1:], segs[1:])
}

// ValidGlob reports a malformed glob (a segment path.Match rejects); zones
// validate their patterns with it so both use one dialect.
func ValidGlob(g string) error { return validGlob(g) }

func validGlob(g string) error {
	for _, seg := range strings.Split(strings.Trim(g, "/"), "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("malformed glob %q: %v", g, err)
		}
	}
	return nil
}
