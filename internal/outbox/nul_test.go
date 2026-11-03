package outbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Postgres jsonb refuses the \u0000 escape (SQLSTATE 22P05). A payload
// carrying a NUL byte must still be storable (F35).
func TestSanitizeJSONNULKeepsPayloadValid(t *testing.T) {
	raw, err := json.Marshal(map[string]string{"reason": "a\x00b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `\u0000`) {
		t.Fatalf("precondition: encoding/json should escape NUL as \\u0000, got %s", raw)
	}
	out := SanitizeJSONNUL(raw)
	if strings.Contains(string(out), `\u0000`) {
		t.Fatalf("still contains \\u0000: %s", out)
	}
	var back map[string]string
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if back["reason"] != "a�b" {
		t.Fatalf("reason = %q, want a\\ufffdb", back["reason"])
	}
}

func TestNewEventPayloadHasNoNUL(t *testing.T) {
	ev, err := NewEvent(AggregateTypeWorkItem, uuid.New(), "t", "k", map[string]string{"x": "\x00"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ev.Payload), `\u0000`) {
		t.Fatalf("event payload carries \\u0000: %s", ev.Payload)
	}
}
