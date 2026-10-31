// Package interactions — shared helpers for agents resuming a tool loop
// after a user answers.
package interactions

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// InjectAnswer appends two synthetic messages to an agent's serialized
// message history so the next tool-loop iteration sees the user's answer
// as if it had flowed naturally through the conversation:
//
//	1. A user-role message summarizing the answer (action chosen + free-text).
//	2. A tool-role message resolving the original request_user_input call,
//	   so the 1:1 tool_call → tool_result bookkeeping in the LLM protocol
//	   stays consistent.
//
// The history is opaque JSON (agents define their own message shapes) —
// we only require that it's a JSON array of objects. If the history was
// never persisted (empty or "[]"), an empty array is accepted.
func InjectAnswer(history []byte, promptID uuid.UUID, a AnsweredPayload) ([]byte, error) {
	var msgs []map[string]any
	if err := json.Unmarshal(history, &msgs); err != nil {
		return nil, fmt.Errorf("history unmarshal: %w", err)
	}
	userContent := fmt.Sprintf("[Answer to %q] %s — %s", a.Question, a.ActionChosen, a.AnswerText)
	msgs = append(msgs,
		map[string]any{
			"role":    "user",
			"content": userContent,
		},
		map[string]any{
			"role":         "tool",
			"tool_call_id": promptID.String(),
			"content":      "user answered",
		},
	)
	return json.Marshal(msgs)
}
