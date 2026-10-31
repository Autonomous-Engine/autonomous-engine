package verifier

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
)

// pushingAdapter is a verifier model that first tries `git push`, then
// answers with an approval.
type pushingAdapter struct {
	mu    sync.Mutex
	calls int
	push  string
}

func (a *pushingAdapter) Complete(context.Context, adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == 1 {
		return &adapter.CompletionResponse{
			ToolCalls:    []adapter.ToolCall{{ID: "push-1", Name: "run_command", Arguments: `{"command":` + strconvQuote(a.push) + `}`}},
			FinishReason: "tool_calls",
		}, nil
	}
	return &adapter.CompletionResponse{Content: approveAC1, FinishReason: "stop"}, nil
}
func (a *pushingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, nil
}
func (a *pushingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *pushingAdapter) HealthCheck(context.Context) error { return nil }
func (a *pushingAdapter) Name() string                      { return "fake/pusher" }

// TestRunInstallsCommandGuardBeforeAnyTool: Run is handed an executor
// with no guard (as a caller that forgot it would), and the real tool loop
// executes the model's first call, `git push` to a reachable remote. Run
// must have installed CommandGuard before that call: the push is REJECTED
// and the remote receives nothing.
func TestRunInstallsCommandGuardBeforeAnyTool(t *testing.T) {
	gitAvailable(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH; run_command runs through sh")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, tree, "init", "-q")
	gitOut(t, tree, "add", "-A")
	gitOut(t, tree, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "-m", "base")
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitOut(t, tree, "init", "-q", "--bare", remote)
	gitOut(t, tree, "remote", "add", "origin", filepath.ToSlash(remote))

	in := Build(Sources{
		WorkID: uuid.New(),
		Requirements: Requirements{Criteria: []plan.Criterion{
			{ID: "AC-1", Text: "a", Source: plan.Source{Kind: plan.SourceDerived, Reason: "r"}, Risk: plan.RiskLow},
		}},
		RepoTree: tree,
	})
	model := &pushingAdapter{push: "git push origin HEAD:refs/heads/evil"}
	var mu sync.Mutex
	var results []string
	ex := tools.NewExecutor(tree) // deliberately no SetCommandGuard
	v := Run(context.Background(), Runner{
		Adapter: model, Executor: ex, MaxIterations: 5,
		OnTool: func(callID, toolName, arguments, result string, isError bool, _ tools.ResultMeta) {
			mu.Lock()
			defer mu.Unlock()
			if toolName == "run_command" {
				if !isError {
					result = "OK: " + result
				}
				results = append(results, result)
			}
		},
	}, in)

	mu.Lock()
	defer mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("run_command results = %q, want the one push attempt (verdict %+v)", results, v)
	}
	if !strings.HasPrefix(results[0], "REJECTED") || !strings.Contains(results[0], "verifier may not run git commands") {
		t.Fatalf("git push was not denied by CommandGuard: %s", results[0])
	}
	if refs := strings.TrimSpace(gitOut(t, remote, "for-each-ref")); refs != "" {
		t.Fatalf("the remote received refs: %s", refs)
	}
	if v.Overall != OverallApprove {
		t.Fatalf("verdict = %+v, want the scripted approval after the rejected push", v)
	}
}
