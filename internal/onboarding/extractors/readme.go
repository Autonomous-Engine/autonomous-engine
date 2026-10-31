package extractors

import (
	"bufio"
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

type readmeExtractor struct{}

func init() { framework.Register(&readmeExtractor{}) }

func (*readmeExtractor) Name() string        { return "readme" }
func (*readmeExtractor) Family() string       { return "documentation" }
func (*readmeExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*readmeExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{
		"README*",
		"docs/README.md",
	})
}

func (*readmeExtractor) ExtractFull(_ context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	candidate := ReadmeCandidate(ec.ProductID, &ec.RepoID, ec.RepoPath)
	payload, _ := json.Marshal(candidate)
	return []*framework.Blob{{
		Section: "readme_summary", Payload: payload, Confidence: 0.9,
	}}, nil
}

func (e *readmeExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

const readmeMaxLines = 500

// SliceReadme finds a README file in the repo root and returns up to the first
// readmeMaxLines lines plus the file path. Returns an error when no README is
// present — the caller (summarize.go) decides whether to fall back to
// not_detected or a deterministic summary.
func SliceReadme(repoPath string) (string, string, error) {
	candidates := []string{"README.md", "README.MD", "README.rst", "README", "readme.md"}
	for _, rel := range candidates {
		p := filepath.Join(repoPath, rel)
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		defer f.Close() //nolint:errcheck

		var sb strings.Builder
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lines := 0
		for scanner.Scan() {
			sb.WriteString(scanner.Text())
			sb.WriteByte('\n')
			lines++
			if lines >= readmeMaxLines {
				break
			}
		}
		return sb.String(), rel, nil
	}
	return "", "", fmt.Errorf("no README found in %s", repoPath)
}

// ReadmeCandidate wraps SliceReadme and returns a QDomainKnowledge Candidate
// populated with the raw README slice as Value. Confidence is intentionally
// ConfidenceLow — a README is marketing overview, not ground truth. The
// caller (summarize.go in T10) may replace Value with an LLM-generated summary
// and/or upgrade confidence. When no README is found we emit NotDetected so
// the question is never silently skipped.
func ReadmeCandidate(productID uuid.UUID, repoID *uuid.UUID, repoPath string) onboarding.Candidate {
	slice, path, err := SliceReadme(repoPath)
	if err != nil {
		return onboarding.NotDetected(productID, repoID, onboarding.QDomainKnowledge,
			"sliceReadme", err.Error())
	}
	return onboarding.Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  onboarding.QDomainKnowledge,
		Value:        slice,
		Confidence:   onboarding.ConfidenceLow,
		Evidence:     []onboarding.Evidence{{Kind: "file", Path: path}},
		Extractor:    "sliceReadme",
		UserStatus:   onboarding.StatusPending,
	}
}
