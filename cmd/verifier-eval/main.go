// Command verifier-eval measures the independent verifier (internal/verifier)
// against seeded defects, next to the author-only review it replaces as the
// first gate (M3-07). It is a developer tool, not a service.
//
// Each fixture under internal/verifier/testdata/seeded is materialized as a
// real git repository (base commit, head commit with one seeded defect).
// With a provider configured, both reviewers run on every fixture:
//
//   - verifier: internal/verifier.Run with the fixture's criteria and task
//     spec, the diff base...head and a scratch repository at head — no author
//     prose;
//   - author-only: review-agent's LLM review prompt (prompts.ReviewSystemPrompt)
//     with the fixture's author summary as the PR description, the diff via
//     git_diff, and review-agent's keyword decision rule.
//
// and the Markdown table reports which caught the defect. Without a provider
// it validates the corpus (dry run) and says so.
//
// Provider (env): VERIFIER_EVAL_PROVIDER (anthropic|openai|ollama|vllm|
// lmstudio|custom), VERIFIER_EVAL_MODEL, VERIFIER_EVAL_ENDPOINT,
// VERIFIER_EVAL_API_KEY. ANTHROPIC_API_KEY selects anthropic when no
// provider is named; VERIFIER_EVAL_MODEL is always required.
//
//	go run ./cmd/verifier-eval [-fixtures dir] [-out report.md] [-dry-run]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/google/uuid"
)

func main() {
	fixtures := flag.String("fixtures", filepath.FromSlash(verifier.SeededDir), "seeded fixture directory")
	out := flag.String("out", "", "write the Markdown report here (default: stdout only)")
	dry := flag.Bool("dry-run", false, "validate the corpus without calling a model")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-fixture timeout for each reviewer")
	flag.Parse()

	fx, err := verifier.LoadFixtures(*fixtures)
	if err != nil {
		log.Fatalf("verifier-eval: load fixtures: %v", err)
	}
	var report string
	switch {
	case *dry:
		// -dry-run never looks at the provider env.
		report, err = dryRun(fx, "-dry-run")
	default:
		cfg, haveProvider, perr := providerFromEnv()
		switch {
		case perr != nil:
			log.Fatalf("verifier-eval: %v", perr)
		case !haveProvider:
			report, err = dryRun(fx, "no provider configured (VERIFIER_EVAL_PROVIDER / ANTHROPIC_API_KEY unset)")
		default:
			report, err = evaluate(fx, cfg, *timeout)
		}
	}
	fmt.Print(report)
	if *out != "" {
		if werr := os.WriteFile(*out, []byte(report), 0o644); werr != nil {
			log.Fatalf("verifier-eval: write %s: %v", *out, werr)
		}
	}
	if err != nil {
		log.Fatalf("verifier-eval: %v", err)
	}
}

type providerConfig struct {
	provider, model, endpoint, apiKey string
}

// providerFromEnv reads the provider env. ok is false when none is named;
// a provider without a model is an error, never a guess.
func providerFromEnv() (providerConfig, bool, error) {
	c := providerConfig{
		provider: os.Getenv("VERIFIER_EVAL_PROVIDER"),
		model:    os.Getenv("VERIFIER_EVAL_MODEL"),
		endpoint: os.Getenv("VERIFIER_EVAL_ENDPOINT"),
		apiKey:   os.Getenv("VERIFIER_EVAL_API_KEY"),
	}
	if c.provider == "" && os.Getenv("ANTHROPIC_API_KEY") != "" {
		c.provider, c.apiKey = "anthropic", os.Getenv("ANTHROPIC_API_KEY")
	}
	if c.provider != "" && c.model == "" {
		return c, false, fmt.Errorf("VERIFIER_EVAL_MODEL is required with provider %q", c.provider)
	}
	return c, c.provider != "", nil
}

// ── dry run ──────────────────────────────────────────────────────────────

func dryRun(fx []verifier.Fixture, reason string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Verifier seeded-defect evaluation — DRY RUN\n\nNo model was called: %s.\nEach fixture is validated, materialized as a git repository, and checked: the seeded file is in the diff, the author summary reaches the author-only prompt but not the verifier prompt, and the head's own tests (the author's evidence) pass.\n\n", reason)
	b.WriteString("| Fixture | Defect class | Valid | Base → head | Changed files | Seeded file in diff | Verifier prompt (bytes) | Author prose excluded | Author's tests at head |\n|---|---|---|---|---|---|---|---|---|\n")
	failed := 0
	ctx := context.Background()
	for _, f := range fx {
		row := dryRunFixture(ctx, f)
		if !row.ok {
			failed++
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d | %s | %s |\n", f.Name, f.DefectClass, row.valid, row.shas, strings.Join(row.files, ", "), yes(row.inDiff), row.promptBytes, yes(row.excluded), row.tests)
	}
	fmt.Fprintf(&b, "\n%d fixtures, %d failed validation.\n", len(fx), failed)
	if failed > 0 {
		return b.String(), fmt.Errorf("%d fixture(s) failed validation", failed)
	}
	if len(fx) < 8 {
		return b.String(), fmt.Errorf("%d fixtures, want at least 8", len(fx))
	}
	return b.String(), nil
}

type dryRow struct {
	ok          bool
	valid       string
	shas        string
	files       []string
	inDiff      bool
	promptBytes int
	excluded    bool
	tests       string
}

func dryRunFixture(ctx context.Context, f verifier.Fixture) dryRow {
	r := dryRow{valid: "yes", tests: "not run"}
	if err := f.Validate(); err != nil {
		r.valid = "NO: " + oneLine(err.Error())
		return r
	}
	tmp, err := os.MkdirTemp("", "verifier-eval-")
	if err != nil {
		r.valid = "NO: " + err.Error()
		return r
	}
	defer os.RemoveAll(tmp)
	repo, err := f.Materialize(ctx, filepath.Join(tmp, "repo"))
	if err != nil {
		r.valid = "NO: " + oneLine(err.Error())
		return r
	}
	r.shas = short(repo.BaseSHA) + " → " + short(repo.HeadSHA)
	files, patch, truncated, err := verifier.DiffOf(ctx, repo.Path, repo.BaseSHA, repo.HeadSHA)
	if err != nil {
		r.valid = "NO: " + oneLine(err.Error())
		return r
	}
	r.files = files
	for _, p := range files {
		r.inDiff = r.inDiff || verifier.SameFile(p, f.Expect.File)
	}
	in := verifierInput(f, repo, files, patch, truncated, repo.Path)
	vp := verifier.RenderPrompt(in)
	r.promptBytes = len(vp)
	r.excluded = !strings.Contains(vp, f.AuthorSummary) && strings.Contains(baselineUserMessage(f, repo, files), f.AuthorSummary)
	if _, err := exec.LookPath("go"); err == nil {
		cmd := exec.CommandContext(ctx, "go", "test", "./...")
		cmd.Dir = repo.Path
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			r.tests = "fail: " + oneLine(lastLine(string(out)))
		} else {
			r.tests = "pass"
		}
	}
	r.ok = r.inDiff && r.excluded
	return r
}

// ── provider run ─────────────────────────────────────────────────────────

func evaluate(fx []verifier.Fixture, cfg providerConfig, timeout time.Duration) (string, error) {
	a, err := adapter.NewFromConfig(cfg.provider, cfg.endpoint, cfg.apiKey, cfg.model)
	if err != nil {
		return "", fmt.Errorf("build adapter: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Verifier seeded-defect evaluation\n\nModel: %s/%s (both reviewers). Run %s.\n\n", cfg.provider, cfg.model, time.Now().UTC().Format(time.RFC3339))
	b.WriteString("| Fixture | Defect class | Verifier gate | Verifier detected | Author-only decision | Author-only detected |\n|---|---|---|---|---|---|\n")
	vHits, bHits := 0, 0
	for _, f := range fx {
		vGate, vDet, bDec, bDet := runFixture(f, a, cfg, timeout)
		if vDet {
			vHits++
		}
		if bDet {
			bHits++
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", f.Name, f.DefectClass, vGate, yes(vDet), bDec, yes(bDet))
	}
	n := len(fx)
	fmt.Fprintf(&b, "| **Detection rate** | | | **%d/%d (%.0f%%)** | | **%d/%d (%.0f%%)** |\n", vHits, n, pct(vHits, n), bHits, n, pct(bHits, n))
	b.WriteString("\nDetected (verifier): the gate requests changes and an expected criterion is not_met or a critical/major defect names the seeded file. Detected (author-only): review-agent's keyword rule yields request_changes and the review names the seeded file.\n")
	return b.String(), nil
}

func runFixture(f verifier.Fixture, a adapter.ModelAdapter, cfg providerConfig, timeout time.Duration) (vGate string, vDet bool, bDec string, bDet bool) {
	tmp, err := os.MkdirTemp("", "verifier-eval-")
	if err != nil {
		return "error: " + err.Error(), false, "-", false
	}
	defer os.RemoveAll(tmp)
	ctx := context.Background()
	repo, err := f.Materialize(ctx, filepath.Join(tmp, "repo"))
	if err != nil {
		return "error: " + oneLine(err.Error()), false, "-", false
	}

	// Verifier.
	vctx, cancel := context.WithTimeout(ctx, timeout)
	scratch, err := verifier.NewScratch(vctx, repo.Path, repo.HeadSHA)
	if err != nil {
		cancel()
		return "error: " + oneLine(err.Error()), false, "-", false
	}
	files, patch, truncated, _ := verifier.DiffOf(vctx, scratch.Path, repo.BaseSHA, scratch.Head)
	ex := tools.NewExecutor(scratch.Path)
	ex.SetDiffBase(repo.BaseSHA)
	ex.SetDiffHead(scratch.Head)
	v := verifier.Run(vctx, verifier.Runner{Adapter: a, Executor: ex,
		Info: verifier.ModelInfo{Provider: cfg.provider, ModelID: cfg.model, Source: "verifier-eval"}},
		verifierInput(f, repo, files, patch, truncated, scratch.Path))
	_ = scratch.Close(ctx)
	cancel()
	d := verifier.Gate(v)
	vGate = string(d.Action)
	if v.Error != "" {
		vGate += " (" + oneLine(v.Error) + ")"
	}
	vDet = f.Detected(v)

	// Author-only review.
	bctx, bcancel := context.WithTimeout(ctx, timeout)
	defer bcancel()
	bex := tools.NewExecutor(repo.Path)
	bex.SetDiffBase(repo.BaseSHA)
	bex.SetDiffHead(repo.HeadSHA)
	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: prompts.ReviewSystemPrompt},
			{Role: adapter.RoleUser, Content: baselineUserMessage(f, repo, files)},
		},
		Tools:       []adapter.Tool{tools.ReadFile(), tools.SearchCode(), tools.ListFiles(), tools.GitDiff()},
		ToolChoice:  "auto",
		MaxTokens:   8192,
		Temperature: 0.2,
	}
	resp, err := tools.RunToolLoop(bctx, a, req, bex, 15, nil, nil)
	if err != nil || resp == nil {
		return vGate, vDet, "error: " + errString(err), false
	}
	bDec = keywordDecision(resp.Content)
	bDet = bDec == "request_changes" && mentionsFile(resp.Content, f.Expect.File)
	return vGate, vDet, bDec, bDet
}

// verifierInput is what review-agent would build for this change.
func verifierInput(f verifier.Fixture, repo verifier.FixtureRepo, files []string, patch string, truncated bool, tree string) verifier.Input {
	return verifier.Build(verifier.Sources{
		WorkID:       uuid.NewSHA1(uuid.NameSpaceURL, []byte(f.Name)),
		Requirements: verifier.Requirements{Criteria: f.Criteria, TaskSpecs: []verifier.TaskSpec{{TaskID: f.Name, Spec: f.TaskSpec}}},
		Diff:         verifier.Diff{BaseRef: repo.BaseSHA, BaseSHA: repo.BaseSHA, HeadSHA: repo.HeadSHA, Files: files, Patch: patch, Truncated: truncated},
		RepoTree:     tree,
	})
}

// baselineUserMessage mirrors review-agent's review user message: branch,
// base, head, the PR description (the author's summary) and the files.
func baselineUserMessage(f verifier.Fixture, repo verifier.FixtureRepo, files []string) string {
	return fmt.Sprintf(`Review the changes on branch: agent/%s
Base branch: main — base %s (pinned)
Verified head SHA under review: %s (the diff is pinned to this commit)

PR description:
%s

Files changed (git-derived, from verification evidence): %v

Work ID: %s
Repository path: %s

Use git_diff to examine the actual changes (it will automatically diff against the base branch). Only review the code that was actually changed in this PR — do not comment on pre-existing code outside the diff. Use read_file to understand context for changed lines. Produce a thorough review.`,
		f.Name, short(repo.BaseSHA), repo.HeadSHA, f.AuthorSummary, files, f.Name, repo.Path)
}

// keywordDecision is review-agent's decision rule for its LLM review
// (cmd/review-agent/main.go): any changes-needed signal wins, then an
// explicit approval, else the safe default request_changes.
func keywordDecision(text string) string {
	lc := strings.ToLower(text)
	for _, s := range []string{"request_changes", "changes requested", "needs changes", "blocking issue"} {
		if strings.Contains(lc, s) {
			return "request_changes"
		}
	}
	for _, s := range []string{"decision: approve", "decision:approve", "\napprove\n", "**approve**", "verdict: approve", "verdict:approve", "i approve", "lgtm", "looks good to me"} {
		if strings.Contains(lc, s) {
			return "approve"
		}
	}
	return "request_changes (default)"
}

func mentionsFile(text, file string) bool {
	return strings.Contains(text, file) || strings.Contains(text, path.Base(file))
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return 100 * float64(a) / float64(n)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func errString(err error) string {
	if err == nil {
		return "empty response"
	}
	return oneLine(err.Error())
}
