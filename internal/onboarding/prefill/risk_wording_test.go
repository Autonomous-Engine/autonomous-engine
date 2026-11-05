package prefill

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding/extractors"
	"github.com/google/uuid"
)

// With no module over 5 dependents the prefill proposed "All modules are
// low-risk … Autonomous changes allowed across the codebase" for a library
// whose root package is the public API (F25). The default must never be
// blanket autonomy.
func TestRiskAnswersWithoutHighRiskModulesDoNotGrantBlanketAutonomy(t *testing.T) {
	cands := RunCandidateExtractors(context.Background(), nil, Inputs{
		ProductID:   uuid.New(),
		RepoPath:    t.TempDir(),
		Languages:   []string{"Go"},
		ModuleNames: []string{"enmime", "coding", "mediatype", "stringutil", "textproto", "cmd/mime-dump", "cmd/mime-extractor"},
	})
	byKey := map[string]onboarding.Candidate{}
	for _, c := range cands {
		byKey[c.QuestionKey] = c
	}
	scope := byKey[onboarding.QAutonomousScope].Value
	if strings.Contains(scope, "Autonomous changes allowed across the codebase") || !strings.Contains(scope, "human review") {
		t.Fatalf("autonomous_scope = %q", scope)
	}
	if byKey[onboarding.QAutonomousScope].Confidence != onboarding.ConfidenceLow {
		t.Fatalf("autonomous_scope confidence = %s, want low", byKey[onboarding.QAutonomousScope].Confidence)
	}
	zones := byKey[onboarding.QRiskZones].Value
	if strings.Contains(zones, "Risk profile is flat") || !strings.Contains(zones, "exported API") {
		t.Fatalf("risk_zones = %q", zones)
	}
	if !strings.Contains(byKey[onboarding.QReviewTriggers].Value, "exported API") {
		t.Fatalf("review_triggers = %q", byKey[onboarding.QReviewTriggers].Value)
	}
}

// CI workflows alone are not a deployment pipeline: a library "deploys"
// nothing, yet the prefill said "CI/CD via workflow files" (F26).
func TestDeploymentSignalsCIOnlyIsNotADeployment(t *testing.T) {
	label, _ := extractors.ClassifyDeploymentSignals([]string{".github/workflows/build-and-test.yml", ".github/workflows/lint.yml"})
	if !strings.Contains(label, "No deployment pipeline detected") {
		t.Fatalf("label = %q", label)
	}
}
