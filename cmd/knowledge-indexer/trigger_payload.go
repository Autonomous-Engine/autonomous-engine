package main

import (
	"encoding/json"
	"errors"
)

// indexTrigger is the part of a codex.updated / external-change.detected
// message the indexer acts on. Every field is optional.
type indexTrigger struct {
	Trigger      string
	Since        string // defaults to "HEAD~1"
	ProductID    string
	RepositoryID string
}

// notification reports whether the message only announces a change: it
// names no trigger and no repository or product to index (onboarding's
// approve-codex notification, a dead-letter replay of a raw GitHub push
// body). Such a message starts no index; every producer that sets a
// trigger or names a repository or product still does.
func (t indexTrigger) notification() bool {
	return t.Trigger == "" && t.RepositoryID == "" && t.ProductID == ""
}

// errTriggerNotObject is returned for a payload that is valid JSON but not an
// object (an array, a string, a number).
var errTriggerNotObject = errors.New("payload is not a JSON object")

// decodeIndexTrigger extracts the trigger fields from a message payload.
//
// The topics it serves carry several producers' shapes: the onboarding
// approval adds an integer version_number, the webhook push event a boolean
// is_agent, the outbox an event_id, and a dead-letter replay forwards the raw
// GitHub webhook body with nested objects. Decoding into map[string]string
// rejected all of those, so the message was skipped. Here the payload is
// decoded as an object of raw values and only the four known keys are read;
// a known key holding a non-string value is treated as absent rather than
// failing the whole message. JSON null decodes to the zero trigger, as the
// string map did.
func decodeIndexTrigger(raw []byte) (indexTrigger, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return indexTrigger{}, errTriggerNotObject
		}
		return indexTrigger{}, err
	}
	str := func(key string) string {
		v, ok := fields[key]
		if !ok {
			return ""
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return ""
		}
		return s
	}
	t := indexTrigger{
		Trigger:      str("trigger"),
		Since:        str("since"),
		ProductID:    str("product_id"),
		RepositoryID: str("repository_id"),
	}
	if t.Since == "" {
		t.Since = "HEAD~1"
	}
	return t, nil
}
