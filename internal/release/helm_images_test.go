package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	matrixService = regexp.MustCompile(`(?m)^\s+- service: ([a-z-]+)\s*$`)
	chartImage    = regexp.MustCompile(`include "autonomous-engine\.image" \(dict "root" \. "service" "([a-z-]+)"\)`)
	imageLine     = regexp.MustCompile(`(?m)^\s+image:\s*(.+)$`)
)

// TestHelmChartUsesThePublishedImages keeps deploy/helm in step with
// .github/workflows/publish-images.yml: every engine container in the chart
// uses the per-service image helper, every service it names is one the
// workflow publishes, and the default tag is the release tag the workflow
// pushes for the chart's appVersion.
func TestHelmChartUsesThePublishedImages(t *testing.T) {
	root := moduleRoot(t)
	workflow := readTracked(t, ".github/workflows/publish-images.yml")
	if workflow == "" {
		t.Fatal("publish-images.yml not found")
	}
	if !strings.Contains(workflow, "IMAGE_ORG: autonomous-engine") ||
		!strings.Contains(workflow, "${{ env.REGISTRY }}/${{ env.IMAGE_ORG }}/ae-${{ matrix.service }}:${{ steps.vars.outputs.version }}") {
		t.Fatal("publish-images.yml no longer publishes ghcr.io/autonomous-engine/ae-<service>:<tag>; update the chart's image values and this test")
	}
	published := map[string]bool{}
	for _, m := range matrixService.FindAllStringSubmatch(workflow, -1) {
		published[m[1]] = true
	}

	templates, err := filepath.Glob(filepath.Join(root, "deploy", "helm", "templates", "*.yaml"))
	if err != nil || len(templates) == 0 {
		t.Fatalf("no chart templates: %v", err)
	}
	deployed := 0
	for _, f := range templates {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range chartImage.FindAllStringSubmatch(src, -1) {
			deployed++
			if !published[m[1]] {
				t.Errorf("%s: service %q has no image in publish-images.yml", filepath.Base(f), m[1])
			}
		}
		for _, m := range imageLine.FindAllStringSubmatch(src, -1) {
			v := strings.TrimSpace(m[1])
			// Third-party images (Kafka, Postgres, Qdrant, Redis) come from values.
			if strings.HasPrefix(v, "{{ .Values.") {
				continue
			}
			if !chartImage.MatchString(v) {
				t.Errorf("%s: engine image %q does not use the autonomous-engine.image helper", filepath.Base(f), v)
			}
		}
	}
	if deployed == 0 {
		t.Fatal("chart deploys no engine images")
	}

	values := readTracked(t, "deploy/helm/values.yaml")
	for _, want := range []string{"registry: ghcr.io", "repositoryPrefix: autonomous-engine/ae-", `tag: ""`} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml: missing %q", want)
		}
	}
	chart := readTracked(t, "deploy/helm/Chart.yaml")
	if !regexp.MustCompile(`(?m)^appVersion: "?\d+\.\d+\.\d+"?$`).MatchString(chart) {
		t.Errorf("Chart.yaml appVersion must be a release version (the default image tag is v<appVersion>)")
	}
}
