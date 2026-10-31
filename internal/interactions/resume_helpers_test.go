package interactions_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/google/uuid"
)

func TestInjectAnswer_AppendsUserAndToolMessages(t *testing.T) {
	history := []byte(`[{"role":"system","content":"sys"},{"role":"user","content":"add fax"},{"role":"assistant","content":"ok, reading contact page"}]`)
	promptID := uuid.New()

	merged, err := interactions.InjectAnswer(history, promptID, interactions.AnsweredPayload{
		PromptID:     promptID,
		ActionChosen: "use_sendmail",
		AnswerText:   "sendMail at app/lib/mail.ts",
		Question:     "Which function did you mean?",
	})
	if err != nil {
		t.Fatalf("InjectAnswer: %v", err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(merged, &msgs); err != nil {
		t.Fatalf("merged JSON: %v", err)
	}
	// Original 3 + synthetic user message + synthetic tool result = 5.
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d", len(msgs))
	}
	if msgs[3]["role"] != "user" {
		t.Errorf("msg[3].role = %v, want user", msgs[3]["role"])
	}
	uc, _ := msgs[3]["content"].(string)
	if !strings.Contains(uc, "use_sendmail") || !strings.Contains(uc, "sendMail") {
		t.Errorf("user message missing action/text: %q", uc)
	}
	if msgs[4]["role"] != "tool" {
		t.Errorf("msg[4].role = %v, want tool", msgs[4]["role"])
	}
	if msgs[4]["tool_call_id"] != promptID.String() {
		t.Errorf("msg[4].tool_call_id = %v, want %s", msgs[4]["tool_call_id"], promptID)
	}
}

func TestInjectAnswer_EmptyHistory(t *testing.T) {
	// Empty history is a legit input — the agent may have been resumed
	// from a blank start. Must not error.
	merged, err := interactions.InjectAnswer([]byte(`[]`), uuid.New(), interactions.AnsweredPayload{
		AnswerText: "x", ActionChosen: "y", Question: "q",
	})
	if err != nil {
		t.Fatalf("InjectAnswer empty: %v", err)
	}
	var msgs []map[string]any
	_ = json.Unmarshal(merged, &msgs)
	if len(msgs) != 2 {
		t.Errorf("empty history + 2 injected = 2, got %d", len(msgs))
	}
}

func TestInjectAnswer_BadJSON(t *testing.T) {
	_, err := interactions.InjectAnswer([]byte("not json"), uuid.New(), interactions.AnsweredPayload{})
	if err == nil {
		t.Error("bad history should error")
	}
}
