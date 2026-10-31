package extractors

import (
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

func TestClassifyBranches_GitFlow(t *testing.T) {
	branches := []string{"origin/main", "origin/develop", "origin/feature/user-login", "origin/release/1.2.0", "origin/hotfix/crash-on-start"}
	c, evidence := ClassifyBranches(branches)
	if !strings.Contains(strings.ToLower(c), "gitflow") {
		t.Fatalf("expected gitflow classification, got %q", c)
	}
	if len(evidence) == 0 {
		t.Fatalf("expected evidence, got none")
	}
}

func TestClassifyBranches_GitHubFlow(t *testing.T) {
	branches := []string{"origin/main", "origin/feature/signup", "origin/feature/checkout", "origin/bugfix/null-cart"}
	c, _ := ClassifyBranches(branches)
	if !strings.Contains(strings.ToLower(c), "github-flow") {
		t.Fatalf("expected github-flow, got %q", c)
	}
}

func TestClassifyBranches_TrunkBased(t *testing.T) {
	branches := []string{"origin/main", "origin/dev/alice/fix", "origin/dev/bob/refactor"}
	c, _ := ClassifyBranches(branches)
	if !strings.Contains(strings.ToLower(c), "trunk") {
		t.Fatalf("expected trunk-based, got %q", c)
	}
}

func TestClassifyBranches_Custom(t *testing.T) {
	branches := []string{"origin/prod", "origin/staging", "origin/wip-foo"}
	c, _ := ClassifyBranches(branches)
	if !strings.Contains(strings.ToLower(c), "custom") {
		t.Fatalf("expected custom, got %q", c)
	}
}

func TestClassifyBranches_Empty(t *testing.T) {
	c, ev := ClassifyBranches(nil)
	if !strings.Contains(strings.ToLower(c), "no branches") {
		t.Fatalf("expected fallback message, got %q", c)
	}
	if len(ev) != 0 {
		t.Fatalf("expected no evidence for empty input, got %d", len(ev))
	}
}

func TestBranchingImplementsExtractor(t *testing.T) {
	var _ framework.Extractor = (*branchingExtractor)(nil)
}
