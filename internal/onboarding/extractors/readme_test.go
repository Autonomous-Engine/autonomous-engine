package extractors

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

func TestSliceReadme_FindsAndTruncates(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("hello readme\n", 1000)
	writeFile(t, dir, "README.md", body)
	slice, path, err := SliceReadme(dir)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	if path != "README.md" {
		t.Fatalf("expected README.md path, got %q", path)
	}
	if strings.Count(slice, "\n") > 500 {
		t.Fatalf("expected <=500 lines, got %d", strings.Count(slice, "\n"))
	}
}

func TestSliceReadme_Missing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := SliceReadme(dir)
	if err == nil {
		t.Fatalf("expected error when README missing, got nil")
	}
}

func TestSliceReadme_RstFallback(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.rst", "RST docs\n")
	slice, path, err := SliceReadme(dir)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	if path != "README.rst" {
		t.Fatalf("expected README.rst path, got %q", path)
	}
	if !strings.Contains(slice, "RST docs") {
		t.Fatalf("expected RST body, got %q", slice)
	}
}

func TestSliceReadme_CandidateLowConfidence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "# Product\nDoes things.\n")
	c := ReadmeCandidate(uuid.New(), nil, dir)
	if c.Confidence != onboarding.ConfidenceLow {
		t.Fatalf("expected ConfidenceLow, got %q", c.Confidence)
	}
	if c.QuestionKey != onboarding.QDomainKnowledge {
		t.Fatalf("expected QDomainKnowledge, got %q", c.QuestionKey)
	}
	if !strings.Contains(c.Value, "Does things.") {
		t.Fatalf("expected README body in Value, got %q", c.Value)
	}
	if len(c.Evidence) != 1 || c.Evidence[0].Kind != "file" || c.Evidence[0].Path != "README.md" {
		t.Fatalf("expected single file evidence pointing at README.md, got %+v", c.Evidence)
	}
}

func TestSliceReadme_CandidateNotDetectedWhenMissing(t *testing.T) {
	dir := t.TempDir()
	c := ReadmeCandidate(uuid.New(), nil, dir)
	if c.Confidence != onboarding.ConfidenceNotDetected {
		t.Fatalf("expected ConfidenceNotDetected, got %q", c.Confidence)
	}
	if c.QuestionKey != onboarding.QDomainKnowledge {
		t.Fatalf("expected QDomainKnowledge, got %q", c.QuestionKey)
	}
}

func TestReadmeImplementsExtractor(t *testing.T) {
	var _ framework.Extractor = (*readmeExtractor)(nil)
}
