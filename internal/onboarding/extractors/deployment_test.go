package extractors

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/google/uuid"
)

func TestClassifyDeploymentSignals_GitHubActions(t *testing.T) {
	files := []string{".github/workflows/deploy.yml", "Dockerfile"}
	label, _ := ClassifyDeploymentSignals(files)
	if !strings.Contains(strings.ToLower(label), "github-actions") {
		t.Fatalf("expected github-actions, got %q", label)
	}
}

func TestClassifyDeploymentSignals_Kubernetes(t *testing.T) {
	files := []string{"helm/chart.yaml", "k8s/deployment.yaml"}
	label, _ := ClassifyDeploymentSignals(files)
	if !strings.Contains(strings.ToLower(label), "kubernetes") {
		t.Fatalf("expected kubernetes, got %q", label)
	}
}

func TestClassifyDeploymentSignals_DockerCompose(t *testing.T) {
	files := []string{"docker-compose.yml"}
	label, _ := ClassifyDeploymentSignals(files)
	if !strings.Contains(strings.ToLower(label), "docker-compose") {
		t.Fatalf("expected docker-compose, got %q", label)
	}
}

func TestClassifyDeploymentSignals_Terraform(t *testing.T) {
	files := []string{"terraform/main.tf"}
	label, _ := ClassifyDeploymentSignals(files)
	if !strings.Contains(strings.ToLower(label), "terraform") {
		t.Fatalf("expected terraform, got %q", label)
	}
}

func TestClassifyDeploymentSignals_PlatformAsAService(t *testing.T) {
	files := []string{"vercel.json"}
	label, _ := ClassifyDeploymentSignals(files)
	if !strings.Contains(strings.ToLower(label), "platform-as-a-service") {
		t.Fatalf("expected platform-as-a-service, got %q", label)
	}
}

func TestClassifyDeploymentSignals_Unknown(t *testing.T) {
	label, _ := ClassifyDeploymentSignals(nil)
	if !strings.Contains(strings.ToLower(label), "unknown") {
		t.Fatalf("expected unknown, got %q", label)
	}
}

func TestDetectDeploymentSignals_FixtureRepo(t *testing.T) {
	tmp := t.TempDir()
	// Simulate a repo with a Dockerfile and GitHub Actions workflow.
	mustWrite(t, filepath.Join(tmp, "Dockerfile"), "FROM alpine\n")
	mustMkdir(t, filepath.Join(tmp, ".github", "workflows"))
	mustWrite(t, filepath.Join(tmp, ".github", "workflows", "ci.yml"), "name: ci\n")

	productID := uuid.New()
	repoID := uuid.New()
	cand := DetectDeploymentSignals(context.Background(), productID, &repoID, tmp)

	if cand.QuestionKey != onboarding.QDeploymentProcess {
		t.Fatalf("question key mismatch: %q", cand.QuestionKey)
	}
	if cand.Confidence != onboarding.ConfidenceMedium {
		t.Fatalf("expected medium confidence, got %q", cand.Confidence)
	}
	if !strings.Contains(strings.ToLower(cand.Value), "github-actions") {
		t.Fatalf("expected github-actions label, got %q", cand.Value)
	}
	if len(cand.Evidence) == 0 {
		t.Fatalf("expected evidence, got none")
	}
	if cand.Extractor != "detectDeploymentSignals" {
		t.Fatalf("extractor mismatch: %q", cand.Extractor)
	}
}

func TestDetectDeploymentSignals_EmptyRepo(t *testing.T) {
	tmp := t.TempDir()
	cand := DetectDeploymentSignals(context.Background(), uuid.New(), nil, tmp)
	if cand.Confidence != onboarding.ConfidenceNotDetected {
		t.Fatalf("expected not_detected confidence, got %q", cand.Confidence)
	}
	if !strings.Contains(strings.ToLower(cand.Value), "unknown") {
		t.Fatalf("expected unknown label, got %q", cand.Value)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestDeploymentImplementsExtractor(t *testing.T) {
	var _ framework.Extractor = (*deploymentExtractor)(nil)
}
