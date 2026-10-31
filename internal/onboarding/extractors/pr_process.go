package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

// --- framework.Extractor wrapper ---

type prProcessExtractor struct{}

func init() { framework.Register(&prProcessExtractor{}) }

func (*prProcessExtractor) Name() string        { return "pr_process" }
func (*prProcessExtractor) Family() string       { return "process" }
func (*prProcessExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*prProcessExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{
		".github/CODEOWNERS",
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/workflows/*",
	})
}

func (*prProcessExtractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	candidate := DetectPRProcess(ctx, ec.ProductID, &ec.RepoID, ec.RepoPath)
	payload, _ := json.Marshal(candidate)
	return []*framework.Blob{{
		Section: "pr_process", Payload: payload, Confidence: 0.9,
	}}, nil
}

func (e *prProcessExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

// DetectPRProcess reads .github/CODEOWNERS, PR template, and workflow file names
// to produce a candidate summary of the team's PR review/check process.
func DetectPRProcess(ctx context.Context, productID uuid.UUID, repoID *uuid.UUID, repoPath string) onboarding.Candidate {
	value, evidence := ScanPRProcess(repoPath)
	if len(evidence) == 0 {
		return onboarding.NotDetected(productID, repoID, onboarding.QPRProcess,
			"detectPRProcess", "no .github/ artefacts found")
	}
	return onboarding.Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  onboarding.QPRProcess,
		Value:        value,
		Confidence:   onboarding.ConfidenceMedium,
		Evidence:     evidence,
		Extractor:    "detectPRProcess",
		UserStatus:   onboarding.StatusPending,
	}
}

// ParseCodeowners extracts unique owner handles (teams or users) from a
// CODEOWNERS file body. Comments and blank lines are skipped.
func ParseCodeowners(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "@") && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// ScanPRProcess is the pure-filesystem layer (no git required) so it can be
// unit-tested against a temp dir.
func ScanPRProcess(repoPath string) (string, []onboarding.Evidence) {
	var parts []string
	var evidence []onboarding.Evidence

	// CODEOWNERS
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if data, err := os.ReadFile(filepath.Join(repoPath, rel)); err == nil {
			teams := ParseCodeowners(string(data))
			parts = append(parts, fmt.Sprintf("CODEOWNERS file lists %d reviewer(s): %s", len(teams), strings.Join(teams, ", ")))
			evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: rel})
			break
		}
	}

	// PR template
	for _, rel := range []string{".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md", "docs/pull_request_template.md"} {
		if _, err := os.Stat(filepath.Join(repoPath, rel)); err == nil {
			parts = append(parts, "Pull-request template present - standard PR boilerplate is enforced")
			evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: rel})
			break
		}
	}

	// Workflow files
	workflowsDir := filepath.Join(repoPath, ".github", "workflows")
	entries, err := os.ReadDir(workflowsDir)
	if err == nil {
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext != ".yml" && ext != ".yaml" {
				continue
			}
			base := strings.TrimSuffix(e.Name(), ext)
			names = append(names, base)
			evidence = append(evidence, onboarding.Evidence{
				Kind: "workflow",
				Path: filepath.ToSlash(filepath.Join(".github/workflows", e.Name())),
				Name: base,
			})
		}
		if len(names) > 0 {
			parts = append(parts, fmt.Sprintf("CI workflows detected: %s", strings.Join(names, ", ")))
		}
	}

	if len(parts) == 0 {
		return "No .github/ CODEOWNERS, PR template, or workflow files detected.", nil
	}
	return strings.Join(parts, ". ") + ".", evidence
}
