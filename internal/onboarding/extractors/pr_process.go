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
// unit-tested against a temp dir. It answers what a pull request must pass
// (each workflow by name, with its jobs and triggers), who reviews
// (CODEOWNERS) and what the template asks for — not a count of files (F26).
func ScanPRProcess(repoPath string) (string, []onboarding.Evidence) {
	var parts []string
	var evidence []onboarding.Evidence

	// Workflow files: name, triggers and jobs read from the YAML.
	workflowsDir := filepath.Join(repoPath, ".github", "workflows")
	entries, err := os.ReadDir(workflowsDir)
	if err == nil {
		var checks []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext != ".yml" && ext != ".yaml" {
				continue
			}
			base := strings.TrimSuffix(e.Name(), ext)
			rel := filepath.ToSlash(filepath.Join(".github/workflows", e.Name()))
			data, _ := os.ReadFile(filepath.Join(workflowsDir, e.Name()))
			wf := parseWorkflow(string(data))
			name := wf.Name
			if name == "" {
				name = base
			}
			desc := fmt.Sprintf("`%s` (%s", name, rel)
			if len(wf.Jobs) > 0 {
				desc += "; jobs " + strings.Join(wf.Jobs, ", ")
			}
			if len(wf.Triggers) > 0 {
				desc += "; on " + strings.Join(wf.Triggers, ", ")
			}
			checks = append(checks, desc+")")
			evidence = append(evidence, onboarding.Evidence{Kind: "workflow", Path: rel, Name: name, Detail: strings.Join(wf.Jobs, ", ")})
		}
		if len(checks) > 0 {
			parts = append(parts, "A pull request must pass the GitHub Actions workflows "+strings.Join(checks, "; ")+".")
		}
	}

	// CODEOWNERS: who reviews what.
	var owners bool
	for _, rel := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if data, err := os.ReadFile(filepath.Join(repoPath, rel)); err == nil {
			owners = true
			if rules := codeownerRules(string(data)); len(rules) > 0 {
				parts = append(parts, "Reviews are requested from the code owners in "+rel+": "+strings.Join(rules, "; ")+".")
			} else {
				parts = append(parts, rel+" exists but names no owners.")
			}
			evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: rel})
			break
		}
	}

	// PR template: the sections a PR description fills in.
	var template bool
	for _, rel := range []string{".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md", "docs/pull_request_template.md"} {
		if data, err := os.ReadFile(filepath.Join(repoPath, rel)); err == nil {
			template = true
			if heads := markdownHeadings(string(data)); len(heads) > 0 {
				parts = append(parts, "The pull-request template ("+rel+") asks for these sections: "+strings.Join(heads, ", ")+"; the PR description follows it.")
			} else {
				parts = append(parts, "A pull-request template ("+rel+") exists; the PR description follows it.")
			}
			evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: rel})
			break
		}
	}

	if len(parts) == 0 {
		return "No .github/ CODEOWNERS, PR template, or workflow files detected.", nil
	}
	var missing []string
	if !owners {
		missing = append(missing, "No CODEOWNERS file: say who reviews pull requests")
	}
	if !template {
		missing = append(missing, "no pull-request template: say what a PR description must contain")
	}
	if len(missing) > 0 {
		parts = append(parts, strings.Join(missing, "; ")+".")
	}
	return strings.Join(parts, " "), evidence
}

// workflowSummary is what a GitHub Actions workflow file says about itself:
// its name, the events that run it and its job ids.
type workflowSummary struct {
	Name     string
	Triggers []string
	Jobs     []string
}

// parseWorkflow reads the top-level `name:`, the `on:` events (inline list
// or block keys) and the `jobs:` ids (the two-space-indented keys under
// it) from a workflow file. It is a line reader for these three keys, not
// a YAML parser: anything it does not understand is left out, never
// invented.
func parseWorkflow(body string) workflowSummary {
	var wf workflowSummary
	section := ""
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			key, val, _ := strings.Cut(line, ":")
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			section = ""
			switch key {
			case "name":
				wf.Name = strings.Trim(val, `"'`)
			case "on", `"on"`, "'on'":
				if val != "" {
					wf.Triggers = append(wf.Triggers, inlineList(val)...)
				} else {
					section = "on"
				}
			case "jobs":
				section = "jobs"
			}
			continue
		}
		if indent != 2 || !strings.Contains(line, ":") {
			continue
		}
		key, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		key = strings.TrimSpace(strings.TrimPrefix(key, "- "))
		switch section {
		case "on":
			wf.Triggers = append(wf.Triggers, key)
		case "jobs":
			wf.Jobs = append(wf.Jobs, key)
		}
	}
	return wf
}

// inlineList reads `[a, b]` or a bare scalar.
func inlineList(val string) []string {
	val = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(val, "["), "]"))
	if val == "" {
		return nil
	}
	var out []string
	for _, v := range strings.Split(val, ",") {
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// codeownerRules renders each CODEOWNERS rule as "<pattern> → owners".
func codeownerRules(content string) []string {
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
		out = append(out, fmt.Sprintf("`%s` by %s", fields[0], strings.Join(fields[1:], " ")))
	}
	return out
}

// markdownHeadings lists the headings of a Markdown body, in order.
func markdownHeadings(body string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") {
			continue
		}
		if h := strings.TrimSpace(strings.TrimLeft(t, "#")); h != "" {
			out = append(out, h)
		}
	}
	return out
}
