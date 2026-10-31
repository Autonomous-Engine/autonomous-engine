// Package extractors holds deterministic, offline candidate-answer producers
// for the proactive-onboarding pre-fill. Every function must: (1) return a
// Candidate (or values from which one is constructed in the orchestrator),
// (2) cite evidence with file/branch paths, (3) never panic.
package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

// --- framework.Extractor wrapper ---

type branchingExtractor struct{}

func init() { framework.Register(&branchingExtractor{}) }

func (*branchingExtractor) Name() string        { return "branching" }
func (*branchingExtractor) Family() string       { return "process" }
func (*branchingExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*branchingExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{
		".github/PULL_REQUEST_TEMPLATE.md",
		"CONTRIBUTING.md",
		"docs/branch*.md",
		".gitlab/*",
	})
}

func (*branchingExtractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	candidate := DetectBranchingStrategy(ctx, ec.ProductID, &ec.RepoID, ec.RepoPath)
	payload, _ := json.Marshal(candidate)
	return []*framework.Blob{{
		Section: "branching_strategy", Payload: payload, Confidence: 0.9,
	}}, nil
}

func (e *branchingExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

// DetectBranchingStrategy shells out to `git branch -r` against repoPath and
// classifies the remote branches into a branching-strategy bucket.
func DetectBranchingStrategy(ctx context.Context, productID uuid.UUID, repoID *uuid.UUID, repoPath string) onboarding.Candidate {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "branch", "-r", "--format=%(refname:short)")
	out, err := cmd.Output()
	if err != nil {
		return onboarding.NotDetected(productID, repoID, onboarding.QBranchingStrategy,
			"detectBranchingStrategy", fmt.Sprintf("git branch -r failed: %v", err))
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, "/HEAD") {
			continue
		}
		branches = append(branches, line)
	}

	value, evidence := ClassifyBranches(branches)
	confidence := onboarding.ConfidenceMedium
	if len(branches) == 0 {
		confidence = onboarding.ConfidenceNotDetected
	}

	return onboarding.Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  onboarding.QBranchingStrategy,
		Value:        value,
		Confidence:   confidence,
		Evidence:     evidence,
		Extractor:    "detectBranchingStrategy",
		UserStatus:   onboarding.StatusPending,
	}
}

// ClassifyBranches is a pure function separated from DetectBranchingStrategy
// so it can be unit-tested without a git repo. Returns a human-readable
// classification string and a slice of Evidence items citing branch names.
func ClassifyBranches(branches []string) (string, []onboarding.Evidence) {
	if len(branches) == 0 {
		return "No branches detected - repo may be empty or git metadata missing.", nil
	}

	hasDevelop := false
	hasRelease := false
	hasHotfix := false
	hasFeature := false
	hasBugfix := false
	mainLike := 0
	other := 0

	for _, b := range branches {
		short := strings.TrimPrefix(b, "origin/")
		switch {
		case short == "develop":
			hasDevelop = true
		case strings.HasPrefix(short, "release/"):
			hasRelease = true
		case strings.HasPrefix(short, "hotfix/"):
			hasHotfix = true
		case strings.HasPrefix(short, "feature/"):
			hasFeature = true
		case strings.HasPrefix(short, "bugfix/"):
			hasBugfix = true
		case short == "main" || short == "master":
			mainLike++
		case strings.HasPrefix(short, "dev/") || strings.HasPrefix(short, "user/"):
			// trunk-based short-lived branches with dev/<user>/ prefix
		default:
			other++
		}
	}

	evidence := make([]onboarding.Evidence, 0, len(branches))
	for _, b := range branches {
		if len(evidence) >= 8 {
			break
		}
		evidence = append(evidence, onboarding.Evidence{Kind: "branch", Name: b})
	}

	switch {
	case hasDevelop && (hasRelease || hasHotfix):
		return "gitflow - long-lived main + develop branches, with release/* and hotfix/* branches for stabilisation.", evidence
	case hasFeature || hasBugfix:
		return "github-flow - short-lived feature/* branches merged back into main.", evidence
	case mainLike >= 1 && other <= 2:
		return "trunk-based - most work lands on main with short-lived personal branches.", evidence
	default:
		return "custom - branches do not match gitflow/github-flow/trunk-based conventions.", evidence
	}
}
