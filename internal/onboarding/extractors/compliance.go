package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

// --- framework.Extractor wrapper ---

type complianceExtractor struct{}

func init() { framework.Register(&complianceExtractor{}) }

func (*complianceExtractor) Name() string        { return "compliance" }
func (*complianceExtractor) Family() string       { return "process" }
func (*complianceExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*complianceExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{
		"SECURITY.md",
		"COMPLIANCE*",
		"docs/security/*",
		"docs/compliance/*",
	})
}

func (*complianceExtractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	candidate := DetectComplianceFiles(ctx, ec.ProductID, &ec.RepoID, ec.RepoPath)
	payload, _ := json.Marshal(candidate)
	return []*framework.Blob{{
		Section: "compliance", Payload: payload, Confidence: 0.9,
	}}, nil
}

func (e *complianceExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

var complianceTokens = []string{
	"license", "security", "compliance", "gdpr", "hipaa", "soc2", "pci",
	"privacy", "code_of_conduct", "data-retention",
}

// DetectComplianceFiles scans the repo root and the top-level docs/ dir for
// file names containing any compliance-related token. File-name-only — we do
// not read the files (low signal, high noise).
func DetectComplianceFiles(ctx context.Context, productID uuid.UUID, repoID *uuid.UUID, repoPath string) onboarding.Candidate {
	hits := ScanComplianceFiles(repoPath)
	if len(hits) == 0 {
		return onboarding.NotDetected(productID, repoID, onboarding.QCriticalConstraints,
			"detectComplianceFiles", "no compliance-related files in repo root or docs/")
	}
	evidence := make([]onboarding.Evidence, 0, len(hits))
	for _, h := range hits {
		evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: h})
	}
	return onboarding.Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  onboarding.QCriticalConstraints,
		Value:        fmt.Sprintf("Compliance/regulatory artefacts detected: %s. Review each file to confirm which constraints apply.", strings.Join(hits, ", ")),
		Confidence:   onboarding.ConfidenceLow,
		Evidence:     evidence,
		Extractor:    "detectComplianceFiles",
		UserStatus:   onboarding.StatusPending,
	}
}

// ScanComplianceFiles is the pure layer tests drive. It looks at the repo root
// and top-level docs/ directory for file names that contain any compliance
// token. We intentionally do not read file contents — names alone give low but
// useful signal without the noise of keyword-matching prose.
func ScanComplianceFiles(repoPath string) []string {
	var hits []string
	seen := map[string]bool{}

	walk := func(dir string, depth int) {
		root := filepath.Join(repoPath, dir)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil {
				return nil
			}
			rel, _ := filepath.Rel(repoPath, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if strings.Count(rel, "/") >= depth {
					return fs.SkipDir
				}
				return nil
			}
			base := strings.ToLower(d.Name())
			for _, tok := range complianceTokens {
				if strings.Contains(base, tok) {
					if !seen[rel] {
						seen[rel] = true
						hits = append(hits, rel)
					}
					break
				}
			}
			return nil
		})
	}
	walk(".", 1)    // root
	walk("docs", 2) // docs/ one level deep
	return hits
}
