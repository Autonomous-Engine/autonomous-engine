package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// recordingBus captures publishes for assertion.
type recordingBus struct {
	mu   sync.Mutex
	pubs []bus.Message
}

func (r *recordingBus) Publish(_ context.Context, topic, key string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pubs = append(r.pubs, bus.Message{Topic: topic, Key: key, Value: payload})
	return nil
}

func (r *recordingBus) PublishMessage(_ context.Context, m *bus.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pubs = append(r.pubs, *m)
	return nil
}

func (*recordingBus) Subscribe(string, string, bus.Handler) error           { return nil }
func (*recordingBus) SubscribeMultiple([]string, string, bus.Handler) error { return nil }
func (*recordingBus) Close() error                                          { return nil }

func TestRequestUserInput_PublishesPromptWithExpectedFields(t *testing.T) {
	rb := &recordingBus{}
	wiID := uuid.New()
	prodID := uuid.New()
	e := &Executor{
		kb: rb, workItemID: &wiID, productID: &prodID, askingAgent: "implementation-agent",
	}

	args := map[string]any{
		"headline":    "Codex lookup failed",
		"question":    "Which function did you mean?",
		"prompt_type": "clarification",
		"actions": []any{
			map[string]any{"id": "use_sendmail", "label": "Use sendMail", "semantic": "continue"},
		},
		"context_payload": map[string]any{"candidates": []any{"sendMail", "dispatchEmail"}},
	}
	res := e.requestUserInput(context.Background(), "call-1", args)

	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content)
	}
	if len(rb.pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(rb.pubs))
	}
	if rb.pubs[0].Topic != bus.TopicUserPromptRequested {
		t.Errorf("topic = %q", rb.pubs[0].Topic)
	}
	if rb.pubs[0].Key != wiID.String() {
		t.Errorf("partition key = %q, want %s", rb.pubs[0].Key, wiID)
	}
	var payload interactions.Prompt
	if err := json.Unmarshal(rb.pubs[0].Value, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload.Trigger != interactions.TriggerAgentRequest {
		t.Errorf("trigger = %q, want agent_request", payload.Trigger)
	}
	if payload.PromptType != interactions.PromptTypeClarification {
		t.Errorf("prompt_type = %q", payload.PromptType)
	}
	if payload.AskingAgent != "implementation-agent" {
		t.Errorf("asking_agent = %q", payload.AskingAgent)
	}
	if len(payload.Actions) != 1 || payload.Actions[0].ID != "use_sendmail" {
		t.Errorf("actions round-trip failed: %+v", payload.Actions)
	}
}

func TestRequestUserInput_RejectsApprovalPromptType(t *testing.T) {
	rb := &recordingBus{}
	wiID := uuid.New()
	prodID := uuid.New()
	e := &Executor{kb: rb, workItemID: &wiID, productID: &prodID, askingAgent: "x"}

	res := e.requestUserInput(context.Background(), "c1", map[string]any{
		"headline":    "h",
		"question":    "q",
		"prompt_type": "approval",
	})
	if !res.IsError {
		t.Error("approval prompt_type should error — approvals don't come from this tool")
	}
	if len(rb.pubs) != 0 {
		t.Errorf("no publish on rejected prompt_type, got %d", len(rb.pubs))
	}
}

func TestRequestUserInput_RequiresAskingAgent(t *testing.T) {
	rb := &recordingBus{}
	wiID := uuid.New()
	prodID := uuid.New()
	e := &Executor{kb: rb, workItemID: &wiID, productID: &prodID /* askingAgent empty */}

	res := e.requestUserInput(context.Background(), "c1", map[string]any{
		"headline": "h", "question": "q", "prompt_type": "clarification",
	})
	if !res.IsError {
		t.Error("missing askingAgent should error")
	}
}

func TestRequestUserInput_ToolDefinition_CorrectName(t *testing.T) {
	if RequestUserInput().Name != "request_user_input" {
		t.Errorf("tool name = %q, want request_user_input", RequestUserInput().Name)
	}
	if adapter.Tool(RequestUserInput()).Description == "" {
		t.Error("description must not be empty")
	}
}

type recordingResumption struct{ persisted *interactions.ResumptionState }

func (r *recordingResumption) PersistResumption(_ context.Context, rs *interactions.ResumptionState) error {
	r.persisted = rs
	return nil
}

func TestRequestUserInput_PersistsResumptionWhenConfigured(t *testing.T) {
	rb := &recordingBus{}
	rr := &recordingResumption{}
	wiID := uuid.New()
	prodID := uuid.New()
	e := &Executor{
		kb: rb, workItemID: &wiID, productID: &prodID,
		askingAgent: "implementation-agent",
		resumption:  rr,
		resumptionSnapshot: []byte(`[{"role":"user","content":"hello"}]`),
	}
	res := e.requestUserInput(context.Background(), "call-1", map[string]any{
		"headline": "h", "question": "q", "prompt_type": "clarification",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if rr.persisted == nil {
		t.Fatal("PersistResumption not called")
	}
	if rr.persisted.WorkItemID != wiID {
		t.Errorf("persisted.WorkItemID = %v", rr.persisted.WorkItemID)
	}
	if string(rr.persisted.MessageHistory) != `[{"role":"user","content":"hello"}]` {
		t.Errorf("persisted.MessageHistory mismatch: %s", rr.persisted.MessageHistory)
	}
}

func TestRequestUserInput_NoResumptionWhenNotConfigured(t *testing.T) {
	// Nil resumption is legal — tool succeeds without persisting.
	rb := &recordingBus{}
	wiID := uuid.New()
	prodID := uuid.New()
	e := &Executor{kb: rb, workItemID: &wiID, productID: &prodID, askingAgent: "x" /* no resumption */}
	res := e.requestUserInput(context.Background(), "c1", map[string]any{
		"headline": "h", "question": "q", "prompt_type": "clarification",
	})
	if res.IsError {
		t.Errorf("nil resumption should be fine: %s", res.Content)
	}
}
