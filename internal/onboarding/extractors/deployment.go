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

type deploymentExtractor struct{}

func init() { framework.Register(&deploymentExtractor{}) }

func (*deploymentExtractor) Name() string        { return "deployment" }
func (*deploymentExtractor) Family() string       { return "process" }
func (*deploymentExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*deploymentExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{
		"Dockerfile*",
		"docker-compose*.yml",
		"k8s/*",
		"deploy/*",
		".github/workflows/*",
	})
}

func (*deploymentExtractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	candidate := DetectDeploymentSignals(ctx, ec.ProductID, &ec.RepoID, ec.RepoPath)
	payload, _ := json.Marshal(candidate)
	return []*framework.Blob{{
		Section: "deployment", Payload: payload, Confidence: 0.9,
	}}, nil
}

func (e *deploymentExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

// DetectDeploymentSignals walks the repo root (depth 2) looking for well-known
// deployment artefacts and classifies the delivery model.
func DetectDeploymentSignals(ctx context.Context, productID uuid.UUID, repoID *uuid.UUID, repoPath string) onboarding.Candidate {
	detected := collectDeploymentFiles(repoPath)
	label, evidence := ClassifyDeploymentSignals(detected)
	confidence := onboarding.ConfidenceMedium
	if len(detected) == 0 {
		confidence = onboarding.ConfidenceNotDetected
	}
	return onboarding.Candidate{
		ProductID:    productID,
		RepositoryID: repoID,
		QuestionKey:  onboarding.QDeploymentProcess,
		Value:        label,
		Confidence:   confidence,
		Evidence:     evidence,
		Extractor:    "detectDeploymentSignals",
		UserStatus:   onboarding.StatusPending,
	}
}

func collectDeploymentFiles(repoPath string) []string {
	var hits []string
	// Root-level single-file checks.
	for _, rel := range []string{
		"docker-compose.yml", "docker-compose.yaml", "Dockerfile",
		"fly.toml", "vercel.json", "netlify.toml", "app.yaml", "Procfile", "railway.json",
	} {
		if _, err := os.Stat(filepath.Join(repoPath, rel)); err == nil {
			hits = append(hits, rel)
		}
	}
	// Directory checks (anything inside counts as a hit).
	for _, dir := range []string{".github/workflows", "helm", "k8s", "kubernetes", "terraform", "infra", "deploy"} {
		p := filepath.Join(repoPath, dir)
		entries, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			hits = append(hits, filepath.ToSlash(filepath.Join(dir, e.Name())))
		}
	}
	return hits
}

// ClassifyDeploymentSignals is the pure-function core — tests drive this
// directly without needing a real filesystem.
func ClassifyDeploymentSignals(files []string) (string, []onboarding.Evidence) {
	hasActions := false
	hasDocker := false
	hasCompose := false
	hasK8s := false
	hasTerraform := false
	hasPaaS := false

	var evidence []onboarding.Evidence
	for _, f := range files {
		lower := strings.ToLower(f)
		switch {
		case strings.HasPrefix(lower, ".github/workflows/"):
			hasActions = true
		case strings.HasPrefix(lower, "helm/"), strings.HasPrefix(lower, "k8s/"), strings.HasPrefix(lower, "kubernetes/"):
			hasK8s = true
		case strings.HasPrefix(lower, "terraform/"), strings.HasSuffix(lower, ".tf"):
			hasTerraform = true
		case lower == "dockerfile":
			hasDocker = true
		case strings.HasPrefix(lower, "docker-compose"):
			hasCompose = true
		case lower == "vercel.json", lower == "netlify.toml", lower == "fly.toml", lower == "app.yaml", lower == "procfile", lower == "railway.json":
			hasPaaS = true
		}
		if len(evidence) < 10 {
			evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: f})
		}
	}

	switch {
	case hasK8s && hasActions:
		return "kubernetes — Helm/K8s manifests deployed via GitHub Actions workflows.", evidence
	case hasK8s:
		return "kubernetes — deployment via Helm or raw manifests.", evidence
	case hasTerraform:
		return "terraform — infrastructure defined as code; deploys likely triggered by CI.", evidence
	case hasActions && hasDocker:
		return fmt.Sprintf("github-actions + docker — %d workflow file(s) build and publish container images.", countPrefixed(files, ".github/workflows/")), evidence
	case hasActions:
		return "github-actions — CI/CD via workflow files in .github/workflows/.", evidence
	case hasCompose:
		return "docker-compose — local or single-host orchestration.", evidence
	case hasPaaS:
		return "platform-as-a-service — deploys through a managed platform (Vercel/Netlify/Fly/Heroku/Railway).", evidence
	default:
		return "unknown — no deployment artefacts detected.", evidence
	}
}

func countPrefixed(files []string, prefix string) int {
	n := 0
	for _, f := range files {
		if strings.HasPrefix(strings.ToLower(f), prefix) {
			n++
		}
	}
	return n
}
