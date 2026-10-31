package extractors

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

func TestDetectComplianceFiles_Found(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "LICENSE", "MIT")
	writeFile(t, dir, "SECURITY.md", "report @ security@")
	writeFile(t, dir, "docs/GDPR-notes.md", "cookies…")
	hits := ScanComplianceFiles(dir)
	if len(hits) < 3 {
		t.Fatalf("expected at least 3 hits, got %d: %v", len(hits), hits)
	}
}

func TestDetectComplianceFiles_NotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "hello")
	hits := ScanComplianceFiles(dir)
	if len(hits) != 0 {
		t.Fatalf("expected no hits, got %v", hits)
	}
}

func TestComplianceImplementsExtractor(t *testing.T) {
	var _ framework.Extractor = (*complianceExtractor)(nil)
}
