package verifier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxIterations caps the verifier's tool loop.
const DefaultMaxIterations = 20

// Runner is how a verification is executed.
type Runner struct {
	// Adapter is the verifier's model; nil escalates without a call.
	Adapter adapter.ModelAdapter
	// Executor runs the tools; it must be rooted at Input.RepoTree with
	// its diff base and head set. Nil escalates without a call.
	Executor *tools.Executor
	// SystemPrompt is the resolved verify_implementation template;
	// "" uses prompts.VerifyImplementationSystemPrompt.
	SystemPrompt  string
	MaxIterations int
	// Info is recorded on the verdict.
	Info ModelInfo
	// DB and WorkItemID let the tool loop log inference rows (optional).
	// The caller tags ctx with the verifier role (llm.WithInferenceTags).
	DB         *pgxpool.Pool
	WorkItemID *uuid.UUID
	// OnTool, when set, observes every tool call (dashboard streaming).
	OnTool tools.ToolCallback
	// loop replaces tools.RunToolLoop in tests.
	loop func(ctx context.Context, a adapter.ModelAdapter, req adapter.CompletionRequest, ex *tools.Executor, max int) (*adapter.CompletionResponse, error)
}

// Run verifies in and always returns a verdict: a model answer that passed
// strict parsing (on the first try or after one re-prompt naming the parse
// error), or an Escalation saying why there is none. It never returns an
// approval it did not parse.
//
// R18: an invalid evidence reference is a problem of its criterion only.
// The re-prompt names just those citations; one still invalid after it
// makes that criterion not_verifiable ("evidence ref invalid: …", listed in
// InvalidEvidence) and the other results and defects stand. When the
// re-prompt's answer does not parse, the first answer stands that way.
//
// Non-code criteria (plan.Criterion.NonCode, R3) are not given to the
// model: no change can satisfy them. Each is appended to the verdict as
// not_verifiable with its gap line (plan.NonCodeGap) and listed in Gaps, so
// the gate never approves it and a human verifies it — fail-closed by
// surfacing (a fixable not_met next to it gets its fix round first, R16).
func Run(ctx context.Context, r Runner, in Input) Verdict {
	var judged, nonCode []plan.Criterion
	for _, c := range in.Criteria {
		if c.NonCode {
			nonCode = append(nonCode, c)
		} else {
			judged = append(judged, c)
		}
	}
	cmds := in.PlanCommands()
	if len(nonCode) == 0 {
		return run(ctx, r, in).WithPlanChecks(cmds)
	}
	in.Criteria = judged
	v := run(ctx, r, in)
	for _, c := range nonCode {
		gap := plan.NonCodeGap(c)
		v.PerCriterion = append(v.PerCriterion, CriterionResult{ID: c.ID, Status: StatusNotVerifiable, Evidence: []Ref{}, Note: gap})
		v.Gaps = append(v.Gaps, gap)
	}
	return v.WithPlanChecks(cmds)
}

func run(ctx context.Context, r Runner, in Input) Verdict {
	ids := in.CriterionIDs()
	if in.RepoTree == "" {
		reason := in.Unavailable
		if reason == "" {
			reason = "no repository tree at the head commit"
		}
		return Escalation(ids, "not verifiable: "+reason, ModelInfo{})
	}
	if r.Adapter == nil || r.Executor == nil {
		return Escalation(ids, "not verifiable: no verifier model or tool executor", r.Info)
	}
	if len(in.Tools) == 0 {
		in.Tools = ToolNames()
	}
	sys := r.SystemPrompt
	if strings.TrimSpace(sys) == "" {
		sys = prompts.VerifyImplementationSystemPrompt
	}
	maxIters := r.MaxIterations
	if maxIters <= 0 {
		maxIters = DefaultMaxIterations
	}

	// Record every call id of this run: a "met" may cite them.
	var mu sync.Mutex
	callIDs := map[string]bool{}
	// The verifier never writes through git (CommandGuard).
	r.Executor.SetCommandGuard(CommandGuard)
	r.Executor.SetCallback(func(callID, toolName, arguments, result string, isError bool, meta tools.ResultMeta) {
		if !isError && callID != "" {
			mu.Lock()
			callIDs[callID] = true
			mu.Unlock()
		}
		if r.OnTool != nil {
			r.OnTool(callID, toolName, arguments, result, isError, meta)
		}
	})
	snapshot := func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(callIDs))
		for k, v := range callIDs {
			out[k] = v
		}
		return out
	}

	loop := r.loop
	if loop == nil {
		loop = func(ctx context.Context, a adapter.ModelAdapter, req adapter.CompletionRequest, ex *tools.Executor, max int) (*adapter.CompletionResponse, error) {
			return tools.RunToolLoop(ctx, a, req, ex, max, r.DB, r.WorkItemID)
		}
	}

	req := adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: sys},
			{Role: adapter.RoleUser, Content: RenderPrompt(in)},
		},
		Tools:       Tools(),
		ToolChoice:  "auto",
		MaxTokens:   8192,
		Temperature: 0.1,
	}
	resp, err := loop(ctx, r.Adapter, req, r.Executor, maxIters)
	if err != nil {
		if errors.Is(err, tools.ErrToolCallingNotSupported) {
			return Escalation(ids, "not verifiable: the verifier model does not support tool calling", r.Info)
		}
		return Escalation(ids, fmt.Sprintf("not verifiable: verifier tool loop failed: %v", err), r.Info)
	}
	content := ""
	if resp != nil {
		content = resp.Content
	}
	v, probs, perr := parseVerdict(content, ids, Grounding{CallIDs: snapshot(), PathExists: TreePathExists(in.RepoTree)})
	if perr == nil && len(probs) == 0 {
		v.ModelInfo = r.Info
		return v
	}

	// One re-prompt. A structural problem is named as before; an answer
	// whose only problem is invalid evidence gets the offending citations
	// alone (R18).
	reprompt := ""
	if perr != nil {
		reprompt = "Your answer was rejected: " + perr.Error() +
			"\nReply again with ONLY the JSON object described under Output, correcting that problem. Do not call more tools unless you must."
	} else {
		reprompt = invalidRefsReprompt(probs)
	}
	req.Messages = append(req.Messages,
		adapter.ChatMessage{Role: adapter.RoleAssistant, Content: content},
		adapter.ChatMessage{Role: adapter.RoleUser, Content: reprompt},
	)
	// firstParsed is the first answer with its invalid citations voiding
	// their criteria: what stands when the re-prompt does not parse.
	firstParsed := func() (Verdict, bool) {
		if perr != nil {
			return Verdict{}, false
		}
		v1 := voidInvalidRefs(v, probs)
		v1.ModelInfo = r.Info
		return v1, true
	}
	resp2, err2 := loop(ctx, r.Adapter, req, r.Executor, 3)
	if err2 != nil {
		if v1, ok := firstParsed(); ok {
			return v1
		}
		return Escalation(ids, fmt.Sprintf("not verifiable: verdict unparseable (%v) and the re-prompt failed: %v", perr, err2), r.Info)
	}
	content2 := ""
	if resp2 != nil {
		content2 = resp2.Content
	}
	v2, probs2, perr2 := parseVerdict(content2, ids, Grounding{CallIDs: snapshot(), PathExists: TreePathExists(in.RepoTree)})
	if perr2 != nil {
		if v1, ok := firstParsed(); ok {
			return v1
		}
		return Escalation(ids, fmt.Sprintf("not verifiable: verdict unparseable after one re-prompt: %v", perr2), r.Info)
	}
	// An invalid citation left after the re-prompt voids its criterion,
	// never the verdict.
	v2 = voidInvalidRefs(v2, probs2)
	v2.ModelInfo = r.Info
	return v2
}
