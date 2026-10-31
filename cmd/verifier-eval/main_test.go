package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/verifier"
)

func corpus(t *testing.T) []verifier.Fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	fx, err := verifier.LoadFixtures(filepath.Join("..", "..", filepath.FromSlash(verifier.SeededDir)))
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestDryRunValidatesCorpus(t *testing.T) {
	fx := corpus(t)
	report, err := dryRun(fx, "test")
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	if strings.Count(report, "| yes |") < len(fx) || !strings.Contains(report, "0 failed validation") {
		t.Fatalf("report:\n%s", report)
	}
}

func TestKeywordDecisionMirrorsReviewAgent(t *testing.T) {
	cases := map[string]string{
		"DECISION: APPROVE":         "approve",
		"LGTM":                      "approve",
		"DECISION: REQUEST_CHANGES": "request_changes",
		"looks good to me but there is a blocking issue": "request_changes",
		"hmm": "request_changes (default)",
	}
	for in, want := range cases {
		if got := keywordDecision(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

func TestProviderFromEnvNeverFatals(t *testing.T) {
	t.Setenv("VERIFIER_EVAL_PROVIDER", "anthropic")
	t.Setenv("VERIFIER_EVAL_MODEL", "")
	if _, _, err := providerFromEnv(); err == nil {
		t.Fatal("a provider without a model must be an error")
	}
	t.Setenv("VERIFIER_EVAL_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, ok, err := providerFromEnv(); ok || err != nil {
		t.Fatalf("no provider: ok=%v err=%v", ok, err)
	}
}

// Env-gated: runs both reviewers against the configured provider.
func TestEvaluateAgainstProvider(t *testing.T) {
	// Opt-in only: a key in the environment must never make a plain
	// `go test ./...` spend money.
	if os.Getenv("VERIFIER_EVAL") != "1" {
		t.Skip("set VERIFIER_EVAL=1 (and VERIFIER_EVAL_PROVIDER / VERIFIER_EVAL_MODEL / key) to run the provider evaluation")
	}
	cfg, ok, err := providerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("VERIFIER_EVAL=1 but no provider configured")
	}
	report, err := evaluate(corpus(t), cfg, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + report)
}
