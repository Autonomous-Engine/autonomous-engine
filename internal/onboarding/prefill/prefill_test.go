package prefill

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

func TestRunCandidateExtractors_ProducesAllQuestionKeys(t *testing.T) {
	dir := t.TempDir()
	// Write enough files for every extractor to have something to chew on.
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	must("README.md", "# example\nThis is the readme.\n")
	must(".github/CODEOWNERS", "* @org/platform\n")
	must(".github/workflows/ci.yml", "name: CI\n")
	must("docker-compose.yml", "version: '3'\n")
	must("LICENSE", "MIT\n")

	cands := RunCandidateExtractors(context.Background(), nil /*llmSvc*/, Inputs{
		ProductID:       uuid.New(),
		RepoPath:        dir,
		Languages:       []string{"Go"},
		ModuleNames:     []string{"internal/foo", "internal/bar"},
		HotspotFiles:    []string{"internal/foo/foo.go"},
		PatternGroups:   []PatternGroupSummary{{Pattern: "handler", Count: 3, RefFiles: []string{"a_handler.go", "b_handler.go"}}, {Pattern: "test", Count: 5, RefFiles: []string{"a_test.go"}}},
		TestFramework:   "go test ./... -count=1",
		TestFunctions:   42,
		HighRiskModules: []string{"internal/auth"},
		SiblingRepoURLs: []string{"https://github.com/org/other"},
	})

	expected := map[string]bool{
		onboarding.QArchitectureOverview: true, onboarding.QTechStack: true, onboarding.QPatternsDiscovered: true, onboarding.QAntiPatterns: true,
		onboarding.QBranchingStrategy: true, onboarding.QPRProcess: true, onboarding.QDeploymentProcess: true, onboarding.QDomainKnowledge: true,
		onboarding.QCriticalConstraints: true, onboarding.QTestingPhilosophy: true, onboarding.QTestConventions: true,
		onboarding.QAutonomousScope: true, onboarding.QReviewTriggers: true, onboarding.QRiskZones: true, onboarding.QCrossRepoImports: true,
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.QuestionKey] = true
	}
	for k := range expected {
		if !got[k] {
			t.Errorf("missing candidate for %q — Fail Honestly requires every question covered", k)
		}
	}
	if len(expected) != 15 {
		t.Fatalf("expected 15 distinct question keys, have %d in expected-set", len(expected))
	}
}
