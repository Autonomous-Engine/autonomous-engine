// Package release holds source-scan tests that keep the public repository
// free of private references, default secrets and removed components.
package release

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// moduleRoot is the repository root (two levels above this package).
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// trackedFiles lists the repository's tracked files, relative to the root.
func trackedFiles(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = moduleRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files unavailable: %v", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// readTracked returns a tracked file's content with CRLF normalised.
func readTracked(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), rel))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// scanTracked reports every tracked file (except the excluded ones) that
// contains any of the needles.
func scanTracked(t *testing.T, needles []string, exclude func(string) bool) []string {
	t.Helper()
	var hits []string
	for _, f := range trackedFiles(t) {
		if exclude != nil && exclude(f) {
			continue
		}
		src := readTracked(t, f)
		for _, n := range needles {
			if strings.Contains(src, n) {
				hits = append(hits, f+": "+n)
			}
		}
	}
	return hits
}

func noticeFiles(f string) bool {
	return f == "LICENSE" || f == "NOTICE" || f == "THIRD_PARTY_NOTICES.md" || strings.HasPrefix(f, "third_party/")
}

func TestNoLicenseClient(t *testing.T) {
	hits := scanTracked(t, []string{
		"internal/license\"",
		"license.autonomousengine.org",
		"autonomousengine.org/account",
		"AE_LICENSE_KEY",
		"LICENSE_SERVER_URL",
	}, func(f string) bool { return noticeFiles(f) || f == "internal/release/oss_test.go" })
	for _, h := range hits {
		t.Errorf("licensing client reference: %s", h)
	}
}

func TestNoDefaultSecretsOrPrivateDefaults(t *testing.T) {
	hits := scanTracked(t, []string{
		"autonomous-engine-default-secret",
		"change-me-jwt-secret",
		"change-me-32-bytes-long-key-here",
		"10.27.27.10",
		"cwdaniel/ae-test-app",
	}, func(f string) bool { return f == "internal/release/oss_test.go" })
	for _, h := range hits {
		t.Errorf("default secret or private default: %s", h)
	}
}

func TestNoPrivateDocReferences(t *testing.T) {
	hits := scanTracked(t, []string{
		"docs/superpowers",
		"ROADMAP_DECISIONS",
		"PERFORMANCE_BENCHMARKING",
		"IMPLEMENTATION_ROADMAP",
		"SOFTWARE_AUDIT",
	}, func(f string) bool { return f == ".gitignore" || f == "internal/release/oss_test.go" })
	for _, h := range hits {
		t.Errorf("reference to a private document: %s", h)
	}
}

// TestReadmeReferencesExist checks that every repository path named in the
// README and the contributor guide exists, so a newcomer never follows a
// dead path.
func TestReadmeReferencesExist(t *testing.T) {
	tracked := map[string]bool{}
	dirs := map[string]bool{}
	for _, f := range trackedFiles(t) {
		tracked[f] = true
		for d := filepath.ToSlash(filepath.Dir(f)); d != "." && d != "/"; d = filepath.ToSlash(filepath.Dir(d)) {
			dirs[d] = true
		}
	}
	codeSpan := regexp.MustCompile("`([^`\\s]+)`")
	link := regexp.MustCompile(`\]\(([^)#\s]+)\)`)
	for _, doc := range []string{"README.md", "CONTRIBUTING.md"} {
		src := readTracked(t, doc)
		if src == "" {
			t.Fatalf("%s is missing", doc)
		}
		var cands []string
		for _, m := range codeSpan.FindAllStringSubmatch(src, -1) {
			cands = append(cands, m[1])
		}
		for _, m := range link.FindAllStringSubmatch(src, -1) {
			cands = append(cands, m[1])
		}
		// Paths inside fenced command blocks (for example `-f docker/x.yml`).
		for i, block := range strings.Split(src, "```") {
			if i%2 == 0 {
				continue
			}
			for _, tok := range strings.Fields(block) {
				if strings.Contains(tok, "/") && regexp.MustCompile(`\.[a-z]{1,5}$`).MatchString(tok) {
					cands = append(cands, tok)
				}
			}
		}
		for _, c := range cands {
			c = strings.TrimSuffix(strings.TrimPrefix(c, "./"), "/")
			if !looksLikeRepoPath(c) {
				continue
			}
			if !tracked[c] && !dirs[c] {
				t.Errorf("%s names %q, which is not in the repository", doc, c)
			}
		}
	}
}

// looksLikeRepoPath is true for relative paths such as `internal/e2e` or
// `LICENSE`, and false for URLs, commands, flags, env vars and globs.
func looksLikeRepoPath(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.Contains(s, "...") || (strings.HasPrefix(s, ".") && !strings.Contains(s, "/")) || strings.Contains(s, "://") || strings.ContainsAny(s, " =*<>$:@'\"(") || strings.HasPrefix(s, "-") {
		return false
	}
	switch s {
	case "LICENSE", "NOTICE", "Makefile":
		return true
	}
	if strings.Contains(s, "/") {
		first := strings.SplitN(s, "/", 2)[0]
		return !strings.Contains(first, ".") || strings.HasPrefix(first, ".")
	}
	return regexp.MustCompile(`^[A-Za-z0-9_.-]+\.(md|ya?ml|json|toml|go|py|sh|sql|txt)$`).MatchString(s)
}

// TestEnvExampleDatabaseURLFollowsPassword keeps the example database URL
// built from DB_USER and DB_PASSWORD, so changing the password in .env (as
// the README says to) does not leave the services on the old one.
func TestEnvExampleDatabaseURLFollowsPassword(t *testing.T) {
	src := readTracked(t, ".env.example")
	var url string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "DATABASE_URL=") {
			url = line
		}
	}
	if !strings.Contains(url, "${DB_USER}") || !strings.Contains(url, "${DB_PASSWORD}") {
		t.Errorf(".env.example DATABASE_URL must use ${DB_USER} and ${DB_PASSWORD}, got %q", url)
	}
	if strings.Index(src, "DATABASE_URL=") < strings.Index(src, "DB_PASSWORD=") {
		t.Errorf(".env.example must define DB_PASSWORD before DATABASE_URL so a shell can source it")
	}
}

// TestShellScriptsCheckOutLF keeps scripts that run inside Linux containers
// on LF line endings, even in a Windows checkout with core.autocrlf=true.
func TestShellScriptsCheckOutLF(t *testing.T) {
	for _, f := range trackedFiles(t) {
		if !strings.HasSuffix(f, ".sh") {
			continue
		}
		cmd := exec.Command("git", "check-attr", "eol", "--", f)
		cmd.Dir = moduleRoot(t)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git check-attr: %v", err)
		}
		if !strings.HasSuffix(strings.TrimSpace(string(out)), ": eol: lf") {
			t.Errorf("%s is not pinned to LF in .gitattributes: %s", f, strings.TrimSpace(string(out)))
		}
	}
}

// decisionCode matches the private decision-log codes D1..D10.
var decisionCode = regexp.MustCompile(`\bD(10|[1-9])\b`)

// TestNoDecisionCodesInUserFacingStrings keeps the private decision-log codes
// (D1..D10) out of string literals in Go code and out of dashboard copy, where
// an operator would read them with nothing to resolve them against.
func TestNoDecisionCodesInUserFacingStrings(t *testing.T) {
	fset := token.NewFileSet()
	for _, f := range trackedFiles(t) {
		switch {
		case strings.HasSuffix(f, "_test.go") || strings.Contains(f, "/testdata/") || strings.Contains(f, "__tests__") || strings.Contains(f, ".test."):
			continue
		case strings.HasSuffix(f, ".go"):
			file, err := parser.ParseFile(fset, filepath.Join(moduleRoot(t), f), nil, 0)
			if err != nil {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && decisionCode.MatchString(lit.Value) {
					t.Errorf("%s: decision code in string %s", fset.Position(lit.Pos()), lit.Value)
				}
				return true
			})
		case strings.HasPrefix(f, "dashboard/src/") && (strings.HasSuffix(f, ".tsx") || strings.HasSuffix(f, ".ts")):
			for i, line := range strings.Split(readTracked(t, f), "\n") {
				trim := strings.TrimSpace(line)
				if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "*") || strings.HasPrefix(trim, "/*") || strings.HasPrefix(trim, "{/*") {
					continue
				}
				if decisionCode.MatchString(line) {
					t.Errorf("%s:%d: decision code in dashboard source: %s", f, i+1, trim)
				}
			}
		}
	}
}

// TestCISecretScanNeedsNoLicense keeps the CI secret scan runnable in an
// organization repository: gitleaks-action requires a paid license key there,
// so CI downloads a pinned gitleaks release and runs it with the project config.
func TestCISecretScanNeedsNoLicense(t *testing.T) {
	ci := readTracked(t, ".github/workflows/ci.yml")
	if strings.Contains(ci, "gitleaks/gitleaks-action") {
		t.Errorf("ci.yml uses gitleaks-action, which fails without GITLEAKS_LICENSE in an organization repository")
	}
	if !strings.Contains(ci, "gitleaks git") || !strings.Contains(ci, "--config .gitleaks.toml") {
		t.Errorf("ci.yml must run gitleaks over the git history with the project config")
	}
}

// TestReadmeDatabasePasswordIsURLSafe: DATABASE_URL is built from
// DB_PASSWORD, so a password with "@", "/" or ":" breaks every service. The
// README must steer newcomers to a URL-safe value.
func TestReadmeDatabasePasswordIsURLSafe(t *testing.T) {
	src := readTracked(t, "README.md")
	if strings.Contains(src, "DB_PASSWORD=<any password>") {
		t.Errorf("README invites any DB_PASSWORD; characters such as @ / : break DATABASE_URL")
	}
	if !strings.Contains(src, "DB_PASSWORD=<output of: openssl rand -hex 16>") {
		t.Errorf("README must suggest a URL-safe DB_PASSWORD (openssl rand -hex 16)")
	}
}

// TestNoForeignProjectContent keeps other projects out of this repository:
// no copy of, or reference to, the external applications and repositories
// used for early live runs (or the tile provider one of them used), no
// personal links, no "tenant" wording (the engine scopes by product and is
// not multi-tenant), and no upstream code excerpts in the benchmark corpus
// (the verifier rebuilds reference patches from upstream instead).
func TestNoForeignProjectContent(t *testing.T) {
	needles := []string{
		"radrview", "cwdaniel", "donate.stripe.com",
		"cincinnati", "ae-portal",
		"cartocdn", "carto.com", "carto_api_key", "cartoapikey",
		"tenant",
	}
	for _, f := range trackedFiles(t) {
		if f == "internal/release/oss_test.go" {
			continue
		}
		if strings.HasPrefix(f, "benchmarks/tasks/") && strings.Contains(f, "/reference/") {
			t.Errorf("%s: upstream code excerpt in the benchmark corpus", f)
			continue
		}
		low := strings.ToLower(f + "\n" + readTracked(t, f))
		for _, n := range needles {
			if strings.Contains(low, n) {
				t.Errorf("%s: contains %q", f, n)
			}
		}
	}
}

// TestSecurityContactIsSet: the security policy and code of conduct must name
// a real reporting address, not the template placeholder.
func TestSecurityContactIsSet(t *testing.T) {
	for _, f := range []string{"SECURITY.md", "CODE_OF_CONDUCT.md"} {
		src := readTracked(t, f)
		if strings.Contains(src, "SECURITY_CONTACT_EMAIL") {
			t.Errorf("%s still has the SECURITY_CONTACT_EMAIL placeholder", f)
		}
		if !strings.Contains(src, "security@autonomousengine.org") {
			t.Errorf("%s must name security@autonomousengine.org", f)
		}
	}
}
