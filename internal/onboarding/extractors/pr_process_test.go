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
