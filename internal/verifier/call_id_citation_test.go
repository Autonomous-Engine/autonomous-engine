package verifier

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// citingAdapter runs one command, then cites the call id it can read in the
// tool result's TEXT. A local model never sees the transport's tool_call_id
// field (Ollama's chat template renders a tool result as its content alone),
// so an id that is not in the content cannot be cited.
type citingAdapter struct {
	cited string
}

var callIDLine = regexp.MustCompile(`tool call id: (\S+)`)

func (a *citingAdapter) Complete(_ context.Context, req adapter.CompletionRequest) (*adapter.CompletionResponse, error) {
	for _, m := range req.Messages {
		if m.Role != adapter.RoleTool {
			continue
		}
		if sub := callIDLine.FindStringSubmatch(m.Content); sub != nil {
			a.cited = sub[1]
		}
	}
	if a.cited == "" {
		if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == adapter.RoleTool {
			// The result named no id: the model can only describe what it ran.
			return &adapter.CompletionResponse{Content: `{"schema_version":1,"per_criterion":[{"id":"AC-1","status":"met","evidence":["run_command: go version output shows go1.25"],"note":""}],"defects":[],"overall":"approve"}`, FinishReason: "stop"}, nil
		}
		return &adapter.CompletionResponse{ToolCalls: []adapter.ToolCall{{ID: "call_7f3a", Name: "run_command", Arguments: `{"command":"go version"}`}}, FinishReason: "tool_calls"}, nil
	}
	return &adapter.CompletionResponse{Content: `{"schema_version":1,"per_criterion":[{"id":"AC-1","status":"met","evidence":["` + a.cited + `"],"note":""}],"defects":[],"overall":"approve"}`, FinishReason: "stop"}, nil
}
func (a *citingAdapter) Stream(context.Context, adapter.CompletionRequest) (<-chan adapter.StreamChunk, error) {
	return nil, errors.New("not used")
}
func (a *citingAdapter) Capabilities() adapter.ModelCapabilities {
	return adapter.ModelCapabilities{MaxContextTokens: 100000, SupportsFunctions: true}
}
func (a *citingAdapter) HealthCheck(context.Context) error { return nil }
func (a *citingAdapter) Name() string                      { return "citing/test" }

// Live (ciblame item eebeeb1e), the verifier built the branch, ran the tests
// and the CLI, judged all 12 criteria met, and cited the three command-backed
// ones as "run_command: <command> output shows …" — prose, because the call
// id is not in anything the model reads. Those three became not_verifiable
// and the verdict escalated to a person. A tool result the verifier sees
// ends with its call id, so the id can be cited.
func TestToolResultsNameTheirCallIDSoItCanBeCited(t *testing.T) {
	in := runInput(t)
	model := &citingAdapter{}
	v := Run(context.Background(), Runner{Adapter: model, Executor: tools.NewExecutor(in.RepoTree)}, in)
	if v.Error != "" || v.Overall != OverallApprove {
		t.Fatalf("verdict = %+v", v)
	}
	if model.cited != "call_7f3a" {
		t.Fatalf("the model cited %q, want the call id read from the tool result's text", model.cited)
	}
	if len(v.InvalidEvidence) != 0 || v.PerCriterion[0].Status != StatusMet {
		t.Fatalf("per_criterion = %+v invalid = %v, want AC-1 met on the cited call id", v.PerCriterion, v.InvalidEvidence)
	}
	if !strings.Contains(RenderPrompt(in), "tool call id:") {
		t.Fatalf("the output contract does not tell the model where the id is printed")
	}
}
