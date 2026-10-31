package extractors

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

func TestDetectCrossRepoImports_MatchImportsGoMod(t *testing.T) {
	goMod := "module example.com/app\n\nrequire (\n    github.com/acme/billing v1.2.3\n    github.com/acme/orders v0.4.0\n)\n"
	urls := []string{"https://github.com/acme/billing", "https://github.com/acme/orders", "https://github.com/acme/unused"}
	matches := MatchImports(goMod, urls)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d: %v", len(matches), matches)
	}
	joined := strings.Join(matches, ",")
	if !strings.Contains(joined, "acme/billing") || !strings.Contains(joined, "acme/orders") {
		t.Fatalf("expected both billing and orders, got %v", matches)
	}
}

func TestDetectCrossRepoImports_MatchImportsPackageJson(t *testing.T) {
	pkg := `{"dependencies":{"@acme/ui":"1.0.0","left-pad":"1.0.0"}}`
	urls := []string{"https://github.com/acme/ui", "https://github.com/acme/other"}
	matches := MatchImports(pkg, urls)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d: %v", len(matches), matches)
	}
	if !strings.Contains(matches[0], "acme/ui") {
		t.Fatalf("expected acme/ui, got %v", matches)
	}
}

func TestDetectCrossRepoImports_MatchImportsNoMatch(t *testing.T) {
	matches := MatchImports("nothing interesting", []string{"https://github.com/acme/x"})
	if len(matches) != 0 {
		t.Fatalf("expected no matches, got %v", matches)
	}
}

func TestDetectCrossRepoImports_MatchImportsCaseInsensitive(t *testing.T) {
	body := "require GitHub.com/ACME/Billing v1.0.0\n"
	urls := []string{"https://github.com/acme/billing"}
	matches := MatchImports(body, urls)
	if len(matches) != 1 {
		t.Fatalf("expected 1 case-insensitive match, got %v", matches)
	}
}

func TestDetectCrossRepoImports_MatchImportsGitSuffix(t *testing.T) {
	body := "require github.com/acme/billing v1.0.0\n"
	urls := []string{"https://github.com/acme/billing.git"}
	matches := MatchImports(body, urls)
	if len(matches) != 1 {
		t.Fatalf("expected .git suffix to be stripped, got %v", matches)
	}
}

func TestDetectCrossRepoImports_NoSiblings(t *testing.T) {
	dir := t.TempDir()
	c := DetectCrossRepoImports(context.Background(), uuid.New(), nil, dir, nil)
	if c.Confidence != onboarding.ConfidenceNotDetected {
		t.Fatalf("expected not_detected with no siblings, got %q", c.Confidence)
	}
	if c.QuestionKey != onboarding.QCrossRepoImports {
		t.Fatalf("expected QCrossRepoImports, got %q", c.QuestionKey)
	}
}

func TestDetectCrossRepoImports_NoManifests(t *testing.T) {
	dir := t.TempDir()
	c := DetectCrossRepoImports(context.Background(), uuid.New(), nil, dir,
		[]string{"https://github.com/acme/billing"})
	if c.Confidence != onboarding.ConfidenceNotDetected {
		t.Fatalf("expected not_detected with no manifests, got %q", c.Confidence)
	}
}

func TestDetectCrossRepoImports_MatchesGoMod(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/app\n\nrequire github.com/acme/billing v1.2.3\n")
	c := DetectCrossRepoImports(context.Background(), uuid.New(), nil, dir,
		[]string{"https://github.com/acme/billing", "https://github.com/acme/unused"})
	if c.Confidence != onboarding.ConfidenceMedium {
		t.Fatalf("expected medium confidence on match, got %q", c.Confidence)
	}
	if !strings.Contains(c.Value, "acme/billing") {
		t.Fatalf("expected billing in Value, got %q", c.Value)
	}
	var importEvidence int
	var fileEvidence int
	for _, e := range c.Evidence {
		switch e.Kind {
		case "import":
			importEvidence++
		case "file":
			fileEvidence++
		}
	}
	if importEvidence != 1 {
		t.Fatalf("expected 1 import evidence, got %d (%+v)", importEvidence, c.Evidence)
	}
	if fileEvidence < 1 {
		t.Fatalf("expected at least 1 file evidence pointing at the manifest, got %+v", c.Evidence)
	}
}

func TestDetectCrossRepoImports_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/app\n\nrequire github.com/other/lib v1.0.0\n")
	c := DetectCrossRepoImports(context.Background(), uuid.New(), nil, dir,
		[]string{"https://github.com/acme/billing"})
	if c.Confidence != onboarding.ConfidenceNotDetected {
		t.Fatalf("expected not_detected when no sibling import matches, got %q", c.Confidence)
	}
}

func TestDetectCrossRepoImports_PackageJsonScope(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"dependencies":{"@acme/ui":"1.0.0"}}`)
	c := DetectCrossRepoImports(context.Background(), uuid.New(), nil, dir,
		[]string{"https://github.com/acme/ui"})
	if c.Confidence != onboarding.ConfidenceMedium {
		t.Fatalf("expected medium confidence on package.json scope match, got %q (value=%q)", c.Confidence, c.Value)
	}
}

