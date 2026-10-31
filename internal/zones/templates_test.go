package zones

import (
	"reflect"
	"strings"
	"testing"
)

// The embedded configs/codex-config.yaml is the default template:
// guarded → human_review, restricted → escalate, with the file's own
// patterns and descriptions. There is no safe zone (a zone needs a
// pattern; unmatched files are autonomous).
func TestDefaultTemplatesFromEmbeddedConfig(t *testing.T) {
	ts, err := DefaultTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("%d templates, want 2: %+v", len(ts), ts)
	}
	want := map[string]Policy{"guarded": PolicyHumanReview, "restricted": PolicyEscalate}
	for _, tpl := range ts {
		if want[tpl.Name] != tpl.Policy {
			t.Errorf("%s policy = %s, want %s", tpl.Name, tpl.Policy, want[tpl.Name])
		}
		if tpl.Source != SourceConfig {
			t.Errorf("%s source = %s", tpl.Name, tpl.Source)
		}
		if err := Validate(tpl.Name, tpl.Patterns, tpl.Policy); err != nil {
			t.Errorf("%s: %v", tpl.Name, err)
		}
		if strings.TrimSpace(tpl.Description) == "" || strings.Contains(tpl.Description, "\n") {
			t.Errorf("%s description = %q", tpl.Name, tpl.Description)
		}
	}
	if got := ts[0].Patterns; !reflect.DeepEqual(got, []string{
		"**/migrations/**", ".github/workflows/**", "**/Dockerfile", "**/Dockerfile.*",
		"**/docker-compose*.yml", "**/docker-compose*.yaml", "deploy/**", "infra/**", "**/auth/**", "**/security/**",
	}) {
		t.Errorf("guarded patterns = %v", got)
	}
	if got := ts[1].Patterns; !reflect.DeepEqual(got, []string{
		".env", ".env.*", "**/.env", "**/.env.*", "**/secrets/**", "**/*.pem", "**/*.key", "!**/*.example",
	}) {
		t.Errorf("restricted patterns = %v (inline comment must be stripped)", got)
	}
}

// The defaults are repository-agnostic: no pattern names this engine's own
// layout (cmd/, internal/, configs/, dashboard/, portal/ ...).
func TestDefaultTemplatesNameNoEnginePaths(t *testing.T) {
	ts, err := DefaultTemplates()
	if err != nil {
		t.Fatal(err)
	}
	engine := []string{"cmd", "internal", "configs", "dashboard", "portal", "benchmarks", "third_party", "scripts", "docker"}
	for _, tpl := range ts {
		for _, p := range tpl.Patterns {
			for _, seg := range strings.Split(strings.TrimPrefix(p, "!"), "/") {
				for _, e := range engine {
					if strings.EqualFold(seg, e) {
						t.Errorf("%s pattern %q names the engine's own path %q", tpl.Name, p, e)
					}
				}
			}
		}
	}
}

// What the defaults decide for typical files of any repository.
func TestDefaultTemplatesClassifyTypicalFiles(t *testing.T) {
	ts, err := DefaultTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var zs []Zone
	for _, tpl := range ts {
		zs = append(zs, Zone{Name: tpl.Name, Patterns: tpl.Patterns, Policy: tpl.Policy})
	}
	for file, want := range map[string]Policy{
		".env":                          PolicyEscalate,
		".env.production":               PolicyEscalate,
		"services/api/.env":             PolicyEscalate,
		"services/api/.env.local":       PolicyEscalate,
		"config/secrets/db.yaml":        PolicyEscalate,
		"certs/server.pem":              PolicyEscalate,
		"keys/deploy.key":               PolicyEscalate,
		".env.example":                  PolicyAutonomous,
		"services/api/.env.example":     PolicyAutonomous,
		"config/secrets/values.example": PolicyAutonomous,
		"db/migrations/001_init.sql":    PolicyHumanReview,
		"migrations/002.up.sql":         PolicyHumanReview,
		".github/workflows/ci.yml":      PolicyHumanReview,
		"Dockerfile":                    PolicyHumanReview,
		"web/Dockerfile.prod":           PolicyHumanReview,
		"docker-compose.yml":            PolicyHumanReview,
		"docker-compose.override.yaml":  PolicyHumanReview,
		"deploy/k8s/app.yaml":           PolicyHumanReview,
		"infra/main.tf":                 PolicyHumanReview,
		"src/auth/login.ts":             PolicyHumanReview,
		"pkg/security/hash.go":          PolicyHumanReview,
		"src/app/main.go":               PolicyAutonomous,
		"README.md":                     PolicyAutonomous,
		"src/keyboard.ts":               PolicyAutonomous,
		"docs/env.md":                   PolicyAutonomous,
	} {
		if got := Strictest(Match(zs, []string{file})); got != want {
			t.Errorf("%s → %s, want %s", file, got, want)
		}
	}
}

func TestParseZonesYAML(t *testing.T) {
	src := `# header
zones:
  - name: a
    description: >
      Folded line one
      and two.
    patterns:
      - "x/**"   # comment
      - 'y/*.go'
      - z
    requires_human_review: false

  - name: "b"
    description: plain text
    patterns: ["p/**", "q"]
    requires_human_review: true
  - name: c
    patterns:
      - c/**
    requires_human_review: true
    escalate_immediately: true

quiet_period:
  window: "02:00-06:00"
`
	ts, err := parseZonesYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []Template{
		{Name: "a", Description: "Folded line one and two.", Patterns: []string{"x/**", "y/*.go", "z"}, Policy: PolicyAutonomous, Source: SourceConfig},
		{Name: "b", Description: "plain text", Patterns: []string{"p/**", "q"}, Policy: PolicyHumanReview, Source: SourceConfig},
		{Name: "c", Patterns: []string{"c/**"}, Policy: PolicyEscalate, Source: SourceConfig},
	}
	if !reflect.DeepEqual(ts, want) {
		t.Fatalf("got  %+v\nwant %+v", ts, want)
	}
	for _, bad := range []string{
		"zones:\n  - name: a\n    patterns: []\n",
		"zones:\n  - description: x\n    patterns:\n      - a\n",
		"zones:\n  - name: a\n    patterns:\n      - a\n    surprise: 1\n",
		"nothing: here\n",
	} {
		if _, err := parseZonesYAML([]byte(bad)); err == nil {
			t.Errorf("want an error for %q", bad)
		}
	}
}
