package extractors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestParseCodeowners_ExtractsTeams(t *testing.T) {
	content := `# CODEOWNERS
* @org/platform
/cmd/ @org/backend @alice
/dashboard/ @org/frontend
`
	teams := ParseCodeowners(content)
	if len(teams) != 4 {
		t.Fatalf("expected 4 unique owners, got %d: %v", len(teams), teams)
	}
}

func TestDetectPRProcess_FullStack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".github/CODEOWNERS", "* @org/platform\n")
	writeFile(t, dir, ".github/pull_request_template.md", "## Summary\n")
	writeFile(t, dir, ".github/workflows/ci.yml", "name: CI\non: [pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n")
	writeFile(t, dir, ".github/workflows/lint.yml", "name: Lint\non: [pull_request]\n")

	value, evidence := ScanPRProcess(dir)
	if !strings.Contains(value, "CODEOWNERS") || !strings.Contains(value, "template") || !strings.Contains(value, "ci") {
		t.Fatalf("value missing expected summary fragments: %q", value)
	}
	if len(evidence) < 3 {
		t.Fatalf("expected at least 3 evidence items, got %d", len(evidence))
	}
}

func TestDetectPRProcess_NothingPresent(t *testing.T) {
	dir := t.TempDir()
	value, evidence := ScanPRProcess(dir)
	if !strings.Contains(strings.ToLower(value), "no") {
		t.Fatalf("expected a 'no ...' summary, got %q", value)
	}
	if len(evidence) != 0 {
		t.Fatalf("expected no evidence, got %d", len(evidence))
	}
}

func TestPRProcessImplementsExtractor(t *testing.T) {
	var _ framework.Extractor = (*prProcessExtractor)(nil)
}

// F26: "CI workflows detected: build-and-test, lint" is a statistic. The
// answer says what a pull request must pass (each workflow by its name,
// its jobs and what triggers it), who reviews (CODEOWNERS) and what the
// template asks for, so the agent can write a PR the way the team does.
func TestScanPRProcessDescribesChecksReviewersAndTemplate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".github/CODEOWNERS", "* @org/platform\n/dashboard/ @org/frontend\n")
	writeFile(t, dir, ".github/pull_request_template.md", "## Summary\n\n## Test plan\n\n- [ ] unit tests\n")
	writeFile(t, dir, ".github/workflows/build-and-test.yml",
		"name: Build and test\non:\n  push:\n    branches: [main]\n  pull_request:\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go build ./...\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n")
	writeFile(t, dir, ".github/workflows/lint.yml", "name: Lint\non: [pull_request]\njobs:\n  golangci:\n    runs-on: ubuntu-latest\n")

	value, evidence := ScanPRProcess(dir)
	for _, want := range []string{"Build and test", "build, test", "pull_request", "push", "Lint", "golangci", "@org/platform", "@org/frontend", "Summary", "Test plan", "must pass"} {
		if !strings.Contains(value, want) {
			t.Errorf("pr_process lacks %q:\n%s", want, value)
		}
	}
	for _, not := range []string{"CI workflows detected", "boilerplate is enforced", "lists 2 reviewer(s)"} {
		if strings.Contains(value, not) {
			t.Errorf("pr_process still says %q:\n%s", not, value)
		}
	}
	if len(evidence) < 4 {
		t.Fatalf("evidence = %+v", evidence)
	}
}

// Workflows alone (no CODEOWNERS, no template) still give the checks a PR
// must pass; the rest is asked of the user, not invented.
func TestScanPRProcessWorkflowsAloneNameTheChecks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".github/workflows/ci.yml", "name: CI\non: [push, pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n")
	value, _ := ScanPRProcess(dir)
	if !strings.Contains(value, "CI") || !strings.Contains(value, "test") || !strings.Contains(value, "must pass") {
		t.Errorf("pr_process = %q", value)
	}
	if !strings.Contains(value, "No CODEOWNERS") || !strings.Contains(value, "no pull-request template") {
		t.Errorf("pr_process does not say what is missing:\n%s", value)
	}
}
