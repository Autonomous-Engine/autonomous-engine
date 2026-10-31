package bus

import (
	"testing"

	"github.com/google/uuid"
)

func TestEventID_HeaderFirst(t *testing.T) {
	h := uuid.New()
	p := uuid.New()
	m := &Message{
		Headers: map[string]string{HeaderEventID: h.String()},
		Value:   []byte(`{"event_id":"` + p.String() + `","work_id":"x"}`),
	}
	if got := m.EventID(); got != h {
		t.Fatalf("EventID = %s, want header id %s", got, h)
	}
}

func TestEventID_PayloadFallback(t *testing.T) {
	p := uuid.New()
	m := &Message{Value: []byte(` {"work_id":"x","event_id":"` + p.String() + `"}`)}
	if got := m.EventID(); got != p {
		t.Fatalf("EventID = %s, want payload id %s", got, p)
	}
}

func TestEventID_InvalidHeaderFallsBackToPayload(t *testing.T) {
	p := uuid.New()
	m := &Message{
		Headers: map[string]string{HeaderEventID: "not-a-uuid"},
		Value:   []byte(`{"event_id":"` + p.String() + `"}`),
	}
	if got := m.EventID(); got != p {
		t.Fatalf("EventID = %s, want payload id %s", got, p)
	}
}

func TestEventID_Absent(t *testing.T) {
	cases := map[string]*Message{
		"nil message":     nil,
		"empty":           {},
		"no field":        {Value: []byte(`{"work_id":"x"}`)},
		"non-object":      {Value: []byte(`["a","b"]`)},
		"not json":        {Value: []byte(`hello`)},
		"non-string id":   {Value: []byte(`{"event_id":42}`)},
		"non-uuid string": {Value: []byte(`{"event_id":"abc"}`)},
		"empty header":    {Headers: map[string]string{HeaderEventID: ""}},
	}
	for name, m := range cases {
		if got := m.EventID(); got != uuid.Nil {
			t.Errorf("%s: EventID = %s, want uuid.Nil", name, got)
		}
	}
}

func TestResolveEventID_NormalisesPayloadIDIntoHeader(t *testing.T) {
	p := uuid.New()
	m := &Message{Topic: "t", Value: []byte(`{"event_id":"` + p.String() + `"}`)}
	if got := resolveEventID("g", m); got != p {
		t.Fatalf("resolveEventID = %s, want %s", got, p)
	}
	if m.Headers[HeaderEventID] != p.String() {
		t.Fatalf("header not set: %v", m.Headers)
	}
}

func TestResolveEventID_CountsMissing(t *testing.T) {
	missingMu.Lock()
	before := missingCount["m105-test-topic"]
	missingMu.Unlock()
	m := &Message{Topic: "m105-test-topic", Value: []byte(`{"x":1}`)}
	if got := resolveEventID("g", m); got != uuid.Nil {
		t.Fatalf("resolveEventID = %s, want Nil", got)
	}
	missingMu.Lock()
	after := missingCount["m105-test-topic"]
	missingMu.Unlock()
	if after != before+1 {
		t.Fatalf("missing count for topic = %d, want %d", after, before+1)
	}
	if _, ok := m.Headers[HeaderEventID]; ok {
		t.Fatalf("a missing id must not be written into the headers")
	}
}
