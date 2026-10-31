package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

func TestSelectReturnsAdvancedForAdvancedMode(t *testing.T) {
	s := Select(models.OperatingModeAdvanced)
	if _, ok := s.(*AdvancedStrategy); !ok {
		t.Errorf("Select(Advanced) returned %T, want *AdvancedStrategy", s)
	}
}

func TestSelectReturnsStandardForStandardMode(t *testing.T) {
	s := Select(models.OperatingModeStandard)
	if _, ok := s.(*StandardStrategy); !ok {
		t.Errorf("Select(Standard) returned %T, want *StandardStrategy", s)
	}
}

func TestSelectReturnsConstrainedForConstrainedMode(t *testing.T) {
	s := Select(models.OperatingModeConstrained)
	if _, ok := s.(*ConstrainedStrategy); !ok {
		t.Errorf("Select(Constrained) returned %T, want *ConstrainedStrategy", s)
	}
}

func TestSelectReturnsStandardForUnknownMode(t *testing.T) {
	s := Select(models.OperatingMode("unknown"))
	if _, ok := s.(*StandardStrategy); !ok {
		t.Errorf("Select(unknown) returned %T, want *StandardStrategy", s)
	}
}

func TestBuildSystemPromptNonEmptyForAllIntents(t *testing.T) {
	intents := []IntentType{
		IntentAnalyzePainPoint,
		IntentGeneratePRD,
		IntentEvaluateFindings,
		IntentSynthesizeBrief,
		IntentDecomposeTasks,
		IntentVerifyAssumptions,
		IntentImplementChange,
		IntentWriteTests,
		IntentFixTestFailure,
		IntentGeneratePRDescription,
		IntentReviewCode,
		IntentValidateCriterion,
		IntentGenerateEdgeCaseTests,
		IntentOnboardingQuestion,
		IntentClassifyChange,
		IntentTraceCodePath,
		IntentAnalyzeHistory,
		IntentMatchPatterns,
		IntentAssessImpact,
		IntentSearchRelatedBugs,
		IntentMapJiraWorkflow,
	}
	ctx := context.Background()
	for _, intentType := range intents {
		prompt := buildSystemPrompt(ctx, Intent{Type: intentType})
		if strings.TrimSpace(prompt) == "" {
			t.Errorf("buildSystemPrompt(%q) returned empty string", intentType)
		}
	}
}

func TestBuildSystemPromptDefaultForUnknownIntent(t *testing.T) {
	prompt := buildSystemPrompt(context.Background(), Intent{Type: "unknown_intent_xyz"})
	if strings.TrimSpace(prompt) == "" {
		t.Error("buildSystemPrompt(unknown) returned empty string")
	}
}

func TestBuildUserPromptIncludesPriorityKeys(t *testing.T) {
	intent := Intent{
		Type: IntentImplementChange,
		Context: map[string]string{
			"technical_brief": "The brief content",
			"target_file":     "internal/foo/bar.go",
			"diff":            "some diff",
		},
	}
	prompt := buildUserPrompt(intent)

	if !strings.Contains(prompt, "The brief content") {
		t.Error("prompt should contain technical_brief content")
	}
	if !strings.Contains(prompt, "internal/foo/bar.go") {
		t.Error("prompt should contain target_file content")
	}
	if !strings.Contains(prompt, "some diff") {
		t.Error("prompt should contain diff content")
	}
}

func TestBuildUserPromptIncludesOutputFormat(t *testing.T) {
	intent := Intent{
		Type:         IntentGeneratePRD,
		Context:      map[string]string{},
		OutputFormat: "JSON only",
	}
	prompt := buildUserPrompt(intent)
	if !strings.Contains(prompt, "JSON only") {
		t.Error("prompt should contain the output format instruction")
	}
}

func TestBuildUserPromptIncludesTaskInstruction(t *testing.T) {
	intent := Intent{
		Type:    IntentReviewCode,
		Context: map[string]string{"diff": "some code"},
	}
	prompt := buildUserPrompt(intent)
	// The task section header should be present
	if !strings.Contains(prompt, "## Task") {
		t.Error("prompt should contain ## Task section")
	}
}

func TestBuildUserPromptSkipsEmptyContextValues(t *testing.T) {
	intent := Intent{
		Type: IntentImplementChange,
		Context: map[string]string{
			"technical_brief": "",
			"target_file":     "real_file.go",
		},
	}
	prompt := buildUserPrompt(intent)
	// Empty values should not create a section header
	if !strings.Contains(prompt, "real_file.go") {
		t.Error("non-empty context value should appear in prompt")
	}
}

// TestBuildUserPromptAnalyzePainPointIncludesTitleAndDescription guards the
// contract the product-agent depends on: for analyze_pain_point, the prompt
// MUST include both the title and the pain_point (description) sections.
// Regression coverage for the bug where an intake payload dropped description
// and the clarity gate then blocked every well-formed work item with
// "What is the description?".
func TestBuildUserPromptAnalyzePainPointIncludesTitleAndDescription(t *testing.T) {
	intent := Intent{
		Type: IntentAnalyzePainPoint,
		Context: map[string]string{
			"title":      "Add autonomous engine hero section",
			"pain_point": "The marketing homepage is missing a hero section that explains what the product does.",
		},
	}
	prompt := buildUserPrompt(intent)
	if !strings.Contains(prompt, "Add autonomous engine hero section") {
		t.Error("prompt should contain the title")
	}
	if !strings.Contains(prompt, "hero section that explains") {
		t.Error("prompt should contain the pain_point/description")
	}
	if !strings.Contains(prompt, "## Pain Point") {
		t.Error("prompt should contain the Pain Point heading")
	}
	if !strings.Contains(prompt, "## Title") {
		t.Error("prompt should contain the Title heading")
	}
}

func TestToTitleCase(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"hello world", "Hello World"},
		{"technical brief", "Technical Brief"},
		{"single", "Single"},
		{"", ""},
	}
	for _, tc := range cases {
		got := toTitleCase(tc.input)
		if got != tc.expected {
			t.Errorf("toTitleCase(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}
