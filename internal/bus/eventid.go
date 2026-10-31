package bus

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/google/uuid"
)

// M1-05: event ids on the consumer side.
//
// Every event a producer commits through the transactional outbox carries a
// stable id, in the Kafka header HeaderEventID and, for JSON-object payloads,
// as the top-level FieldEventID field (internal/outbox/event.go owns the
// producer side; the names are repeated here because internal/outbox imports
// this package). Consumers deduplicate on it with internal/idempotency.
//
// Messages without an id are still delivered: pre-outbox producers whose
// messages are still draining, telemetry topics that are published directly
// (console.stream, tool.executed, state.changed, …), and any producer not yet
// converted. They are counted in MetricEventsWithoutID and cannot be
// deduplicated.

const (
	// HeaderEventID is the Kafka message header carrying the event id.
	HeaderEventID = "event_id"
	// FieldEventID is the top-level JSON payload field carrying the event id.
	FieldEventID = "event_id"
)

// MetricEventsWithoutID counts delivered messages that carried no usable
// event id (none in the header, none in the payload, or one that is not a
// UUID). internal/metrics has no labels, so this is one process-wide series;
// the topic is in the log line written the first time each topic is seen
// without an id (and every logEveryMissing-th time after that).
var MetricEventsWithoutID = metrics.NewCounter("ae_bus_events_without_id_total",
	"Messages delivered to a handler without a usable event_id (header or payload); they cannot be deduplicated")

// logEveryMissing throttles the per-topic log line: telemetry topics never
// carry an id and would otherwise log once per message.
const logEveryMissing = 1000

var (
	missingMu    sync.Mutex
	missingCount = map[string]int64{}
)

// EventID returns the message's event id: the HeaderEventID header first,
// then the top-level FieldEventID of a JSON-object payload. It returns
// uuid.Nil when neither is present or the value is not a UUID.
//
// The result is not cached; the subscribe loop resolves it once per message
// and, when it came from the payload, copies it into the header so later calls
// are a map lookup.
func (m *Message) EventID() uuid.UUID {
	if m == nil {
		return uuid.Nil
	}
	if v, ok := m.Headers[HeaderEventID]; ok && v != "" {
		if id, err := uuid.Parse(v); err == nil {
			return id
		}
	}
	return payloadEventID(m.Value)
}

// payloadEventID extracts a top-level "event_id" string from a JSON object.
func payloadEventID(value []byte) uuid.UUID {
	if len(value) == 0 {
		return uuid.Nil
	}
	var probe struct {
		EventID *string `json:"event_id"`
	}
	if err := json.Unmarshal(value, &probe); err != nil || probe.EventID == nil {
		return uuid.Nil
	}
	id, err := uuid.Parse(*probe.EventID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// resolveEventID is called once per delivered message by the subscribe loop.
// It returns the id (uuid.Nil when absent), normalises it into the header so
// handlers read it with a map lookup, and counts and logs a missing id.
func resolveEventID(group string, msg *Message) uuid.UUID {
	id := msg.EventID()
	if id != uuid.Nil {
		if msg.Headers == nil {
			msg.Headers = map[string]string{}
		}
		msg.Headers[HeaderEventID] = id.String()
		return id
	}
	noteMissingEventID(group, msg.Topic)
	return uuid.Nil
}

// noteMissingEventID increments MetricEventsWithoutID and logs the topic the
// first time and then every logEveryMissing-th time it is seen without an id.
func noteMissingEventID(group, topic string) {
	MetricEventsWithoutID.Inc()
	missingMu.Lock()
	missingCount[topic]++
	n := missingCount[topic]
	missingMu.Unlock()
	if n == 1 || n%logEveryMissing == 0 {
		log.Printf("bus: message without event_id (group=%s topic=%s count=%d) — delivered without deduplication", group, topic, n)
	}
}
