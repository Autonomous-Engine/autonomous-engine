package git_strategy

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() {
	framework.Register(New())
}

func (*Extractor) Name() string         { return "git_strategy" }
func (*Extractor) Family() string       { return "git" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/workflows/*",
		".github/CODEOWNERS",
		"CONTRIBUTING.md",
		"docs/branch*.md",
		".gitlab/*",
	})
}

type Output struct {
	Pattern           string   `json:"pattern"`
	MainBranch        string   `json:"main_branch"`
	LongLivedBranches []string `json:"long_lived_branches"`
	MergeStrategy     string   `json:"merge_strategy"`
	PRRequired        bool     `json:"pr_required"`
}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	branches := listBranches(ctx, ec.RepoPath)
	main := detectMain(ctx, ec.RepoPath, branches)
	longLived := detectLongLived(branches, main)
	pattern := classifyPattern(branches, longLived)
	mergeStrategy := detectMergeStrategy(ctx, ec.RepoPath, main)
	prRequired := detectPRRequired(ctx, ec.RepoPath)

	out := Output{
		Pattern: pattern, MainBranch: main, LongLivedBranches: longLived,
		MergeStrategy: mergeStrategy, PRRequired: prRequired,
	}
	payload, _ := json.Marshal(out)
	return []*framework.Blob{{
		Section:    "git_strategy",
		Payload:    payload,
		Confidence: 0.95,
		Evidence:   []framework.Evidence{{File: ".git", Note: "git history analysis"}},
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

func listBranches(ctx context.Context, repo string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "branch", "-a", "--format=%(refname:short)").Output()
	if err != nil {
		return nil
	}
	var branches []string
	for _, b := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		branches = append(branches, b)
	}
	return branches
}

func detectMain(ctx context.Context, repo string, branches []string) string {
	if out, err := exec.CommandContext(ctx, "git", "-C", repo, "symbolic-ref", "refs/remotes/origin/HEAD").Output(); err == nil {
		v := strings.TrimSpace(string(out))
		if idx := strings.LastIndex(v, "/"); idx >= 0 {
			return v[idx+1:]
		}
	}
	for _, name := range []string{"main", "master", "trunk"} {
		for _, b := range branches {
			if b == name {
				return name
			}
		}
	}
	if len(branches) > 0 {
		return branches[0]
	}
	return "main"
}

func detectLongLived(branches []string, main string) []string {
	long := []string{}
	for _, b := range branches {
		if b == main {
			continue
		}
		if b == "develop" || b == "dev" || strings.HasPrefix(b, "release/") || strings.HasPrefix(b, "hotfix/") {
			long = append(long, b)
		}
	}
	return long
}

func classifyPattern(branches, longLived []string) string {
	hasDevelop := false
	for _, b := range longLived {
		if b == "develop" || b == "dev" {
			hasDevelop = true
		}
	}
	if hasDevelop {
		return "gitflow"
	}
	if len(branches) <= 2 {
		return "trunk_based"
	}
	return "github_flow"
}

func detectMergeStrategy(ctx context.Context, repo, main string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "log", "-n", "50", "--format=%P", main).Output()
	if err != nil {
		return "mixed"
	}
	merges, linear := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parents := strings.Fields(line)
		switch len(parents) {
		case 2:
			merges++
		default:
			linear++
		}
	}
	switch {
	case merges == 0 && linear > 0:
		return "squash"
	case merges > 0 && linear > merges*3:
		return "rebase"
	case merges > 0:
		return "merge"
	}
	return "mixed"
}

func detectPRRequired(ctx context.Context, repo string) bool {
	for _, p := range []string{".github/CODEOWNERS", ".github/PULL_REQUEST_TEMPLATE.md", ".gitlab/merge_request_templates"} {
		if err := exec.CommandContext(ctx, "git", "-C", repo, "ls-files", "--error-unmatch", p).Run(); err == nil {
			return true
		}
	}
	return false
}
