package outbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func mustDecode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload is not a JSON object: %v (%s)", err, raw)
	}
	return m
}

func TestNewEvent_InjectsEventID(t *testing.T) {
	agg := uuid.New()
	ev, err := NewEvent("work_item", agg, "implementation.tasks", agg.String(), map[string]any{
		"work_id": agg.String(),
		"title":   "do the thing",
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.ID == uuid.Nil {
		t.Fatal("NewEvent did not allocate an event id")
	}
	if ev.AggregateType != "work_item" || ev.AggregateID != agg {
		t.Errorf("aggregate = %q/%v, want work_item/%v", ev.AggregateType, ev.AggregateID, agg)
	}
	if ev.Topic != "implementation.tasks" || ev.Key != agg.String() {
		t.Errorf("topic/key = %q/%q", ev.Topic, ev.Key)
	}
	if got := ev.Headers[HeaderEventID]; got != ev.ID.String() {
		t.Errorf("header %s = %q, want %q", HeaderEventID, got, ev.ID)
	}
	payload := mustDecode(t, ev.Payload)
	if got, _ := payload[FieldEventID].(string); got != ev.ID.String() {
		t.Errorf("payload %s = %v, want %q", FieldEventID, payload[FieldEventID], ev.ID)
	}
	if payload["title"] != "do the thing" {
		t.Errorf("payload lost its own fields: %s", ev.Payload)
	}
}

func TestNewEvent_AdoptsExistingEventID(t *testing.T) {
	existing := uuid.New()
	ev, err := NewEvent("work_item", uuid.New(), "implementation.tasks", "", map[string]any{
		"event_id": existing.String(),
		"work_id":  "w1",
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.ID != existing {
		t.Errorf("event id = %v, want the caller's %v", ev.ID, existing)
	}
	if ev.Headers[HeaderEventID] != existing.String() {
		t.Errorf("header = %q, want %q", ev.Headers[HeaderEventID], existing)
	}
	if got, _ := mustDecode(t, ev.Payload)[FieldEventID].(string); got != existing.String() {
		t.Errorf("payload event_id rewritten to %q, want %q", got, existing)
	}
}

func TestNewEvent_NonObjectPayloadStillCarriesHeader(t *testing.T) {
	ev, err := NewEvent("work_item", uuid.New(), "t", "", []int{1, 2, 3})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.Headers[HeaderEventID] != ev.ID.String() {
		t.Errorf("header = %q, want %q", ev.Headers[HeaderEventID], ev.ID)
	}
	if string(ev.Payload) != "[1,2,3]" {
		t.Errorf("payload = %s, want it untouched", ev.Payload)
	}
}

func TestNewEvent_PreEncodedPayload(t *testing.T) {
	ev, err := NewEvent("work_item", uuid.New(), "t", "", json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	p := mustDecode(t, ev.Payload)
	if p["a"] != float64(1) {
		t.Errorf("payload = %s, want a=1 preserved", ev.Payload)
	}
	if got, _ := p[FieldEventID].(string); got != ev.ID.String() {
		t.Errorf("pre-encoded payload did not get an event_id: %s", ev.Payload)
	}
}

func TestNewEvent_RejectsBadInput(t *testing.T) {
	agg := uuid.New()
	cases := map[string]struct {
		aggType string
		aggID   uuid.UUID
		topic   string
		payload any
		want    string
	}{
		"empty aggregate type": {"", agg, "t", map[string]any{}, "aggregate_type"},
		"nil aggregate id":     {"work_item", uuid.Nil, "t", map[string]any{}, "aggregate_id"},
		"empty topic":          {"work_item", agg, "", map[string]any{}, "topic"},
		"nil payload":          {"work_item", agg, "t", nil, "payload"},
		"unmarshalable":        {"work_item", agg, "t", make(chan int), "payload"},
		"invalid json bytes":   {"work_item", agg, "t", json.RawMessage(`{"a":`), "payload"},
		"non-uuid event_id":    {"work_item", agg, "t", map[string]any{"event_id": "not-a-uuid"}, "event_id"},
		"non-string event_id":  {"work_item", agg, "t", map[string]any{"event_id": 7}, "event_id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewEvent(tc.aggType, tc.aggID, tc.topic, "", tc.payload)
			if err == nil {
				t.Fatalf("NewEvent accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestNewEvent_HeadersAreIndependentPerEvent(t *testing.T) {
	a, err := NewEvent("work_item", uuid.New(), "t", "", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEvent("work_item", uuid.New(), "t", "", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two events share an id")
	}
	a.Headers["extra"] = "x"
	if _, ok := b.Headers["extra"]; ok {
		t.Error("events share a headers map")
	}
}
