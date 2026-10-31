// Package outbox implements the transactional outbox (roadmap 1.1, ticket
// M1-01): producers write the state change and the event it causes in one
// PostgreSQL transaction, and a dispatcher (M1-02) publishes the event to
// Kafka afterwards.
//
// Delivery is at-least-once. A row is committed with the state change and
// marked published only after the bus acknowledges it, so a crash between the
// two can duplicate an event; consumers deduplicate on the event id (M1-05).
// Every event therefore carries a stable id in the Kafka headers under
// HeaderEventID and, for JSON-object payloads, as the top-level FieldEventID
// field.
package outbox

import (
	"encoding/json"
	"errors"
	"fmt"

	"time"

	"github.com/google/uuid"
)

const (
	// HeaderEventID is the Kafka message header carrying the event id.
	HeaderEventID = "event_id"
	// FieldEventID is the top-level payload field carrying the event id.
	FieldEventID = "event_id"
)

// ErrNotFound is returned when no outbox row matches an id.
var ErrNotFound = errors.New("outbox event not found")

// ErrClaimLost is returned by Store.MarkFailedBy when the row is no longer
// claimed by the caller: another dispatcher took it over after the claim TTL
// expired, so the outcome the caller is trying to record is no longer its to
// record. Callers should log it and move on, never retry it.
var ErrClaimLost = errors.New("outbox event claim lost")

// Event mirrors one outbox_events row (migration 047). The zero value is not
// useful; construct events with NewEvent.
type Event struct {
	// ID is the event id: the row's primary key, the Kafka header value, and
	// the payload's event_id. Consumers deduplicate on it.
	ID uuid.UUID
	// AggregateType names the kind of thing the event is about
	// ("work_item", "repository", …); AggregateID identifies the instance.
	// Ordering is promised per AggregateID, not per type.
	AggregateType string
	AggregateID   uuid.UUID
	// Topic and Key are the Kafka destination. An empty Key means the
	// producer does not care about partition affinity.
	Topic string
	Key   string
	// Payload is the encoded Kafka value; Headers become message headers.
	Payload json.RawMessage
	Headers map[string]string

	// Dispatch bookkeeping, owned by the Store. These are zero on a freshly
	// constructed Event and populated when one is read back from the table.
	CreatedAt     time.Time
	Attempts      int
	NextAttemptAt time.Time
	ClaimedAt     *time.Time
	ClaimedBy     string
	PublishedAt   *time.Time
	DeadAt        *time.Time
	LastError     string
}

// NewEvent encodes payload and returns an Event ready to Enqueue.
//
// The event id is normally freshly generated. If payload is a JSON object that
// already carries a top-level event_id, that id is adopted instead, so a
// caller that wants a deterministic, retry-stable id (and therefore a
// duplicate INSERT rather than a second event) can supply one. The id is
// always mirrored into the headers, and into the payload when the payload is a
// JSON object; non-object payloads are left byte-for-byte alone and carry the
// id in the header only.
func NewEvent(aggregateType string, aggregateID uuid.UUID, topic, key string, payload any) (Event, error) {
	if aggregateType == "" {
		return Event{}, errors.New("aggregate_type is required")
	}
	if aggregateID == uuid.Nil {
		return Event{}, errors.New("aggregate_id is required")
	}
	if topic == "" {
		return Event{}, errors.New("topic is required")
	}
	if payload == nil {
		return Event{}, errors.New("payload is required")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("encode payload: %w", err)
	}
	if !json.Valid(raw) {
		// Reached when payload is a json.RawMessage / json.Marshaler holding
		// malformed bytes: json.Marshal passes those through unchecked in
		// some Go versions, and an invalid value would fail the JSONB cast at
		// INSERT time instead of here.
		return Event{}, errors.New("payload is not valid JSON")
	}

	id, raw, err := applyEventID(raw)
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:            id,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Topic:         topic,
		Key:           key,
		Payload:       raw,
		Headers:       map[string]string{HeaderEventID: id.String()},
	}, nil
}

// applyEventID resolves the event id for an encoded payload and returns the
// payload with the id present when it is a JSON object.
func applyEventID(raw json.RawMessage) (uuid.UUID, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		// Not a JSON object (array, string, number, null): nothing to inject.
		return uuid.New(), raw, nil
	}
	if existing, ok := fields[FieldEventID]; ok {
		var s string
		if err := json.Unmarshal(existing, &s); err != nil {
			return uuid.Nil, nil, fmt.Errorf("payload event_id must be a UUID string, got %s", existing)
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("payload event_id %q is not a UUID: %w", s, err)
		}
		return id, raw, nil
	}
	id := uuid.New()
	encoded, err := json.Marshal(id.String())
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("encode event_id: %w", err)
	}
	fields[FieldEventID] = encoded
	merged, err := json.Marshal(fields)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("inject event_id into payload: %w", err)
	}
	return id, merged, nil
}
