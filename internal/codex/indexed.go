package codex

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
)

// Triggers recorded on codex.indexed (IndexedEvent.Trigger): what wrote the
// version.
const (
	TriggerFullIndex          = "full_index_product"
	TriggerIncrementalIndex   = "incremental_index"
	TriggerOnboardingApproval = "onboarding_approval"
	TriggerInteractionAnswer  = "interaction_answer"
	TriggerInteractionRevoked = "interaction_revoked"
	// TriggerZoneChange: a code_zones write (internal/zones, migration 063);
	// the version's sections are section:zones.
	TriggerZoneChange = "zone_change"
)

// aggregateTypeProduct orders a product's codex.indexed events (the same
// aggregate the services use for a product's codex.updated requests).
const aggregateTypeProduct = "product"

// IndexedEvent is the bus.TopicCodexIndexed payload: one new codex version
// for ProductID on Branch. SectionsChanged holds typed entries (see
// sections.go). EventID is the outbox event id, filled in on the wire; a
// consumer deduplicates on it (or the event_id header).
type IndexedEvent struct {
	ProductID       uuid.UUID `json:"product_id"`
	Branch          string    `json:"branch"`
	ProductVersion  int       `json:"product_version"`
	VersionNumber   int       `json:"version_number"`
	SectionsChanged []string  `json:"sections_changed"`
	Trigger         string    `json:"trigger"`
	EventID         string    `json:"event_id,omitempty"`
}

// Modules is the event's changed module names (ChangedModules).
func (e IndexedEvent) Modules() []string { return ChangedModules(e.SectionsChanged) }

// DecodeIndexedEvent decodes and validates a codex.indexed payload: it
// needs a product, a branch and a positive product_version.
func DecodeIndexedEvent(raw []byte) (IndexedEvent, error) {
	var ev IndexedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return IndexedEvent{}, fmt.Errorf("decode codex.indexed: %w", err)
	}
	switch {
	case ev.ProductID == uuid.Nil:
		return IndexedEvent{}, errors.New("decode codex.indexed: no product_id")
	case ev.Branch == "":
		return IndexedEvent{}, errors.New("decode codex.indexed: no branch")
	case ev.ProductVersion <= 0:
		return IndexedEvent{}, errors.New("decode codex.indexed: no product_version")
	}
	return ev, nil
}

// indexedOutboxEvent builds the outbox event for ev: topic codex.indexed,
// aggregate the product, key the product id.
func indexedOutboxEvent(ev IndexedEvent) (outbox.Event, error) {
	if ev.SectionsChanged == nil {
		ev.SectionsChanged = []string{}
	}
	return outbox.NewEvent(aggregateTypeProduct, ev.ProductID, bus.TopicCodexIndexed, ev.ProductID.String(), ev)
}
