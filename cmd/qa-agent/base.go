package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
)

// M2-03: the QA report records the pinned base the work item was built on
// and the base-drift status QA received. QA's checkout is unchanged: it stays
// at the verified head SHA (head_sha.go); the base is recorded, not tested.

// baseDrift is the drift a producer may forward on qa.ready (the shape of
// pr.ready.base_drift). status-orchestrator does not forward it today, so QA
// also reads the work item's drift_detected events.
type baseDrift struct {
	Old   string `json:"old"`
	New   string `json:"new"`
	Clean bool   `json:"clean"`
}

// Base drift statuses recorded in the QA report.
const (
	// DriftNone: pinned, and no drift was received or recorded.
	DriftNone = "none"
	// DriftDetected: drift_detected events exist (or drift was received);
	// see the events for old/new and whether it merged cleanly.
	DriftDetected = "detected"
	// DriftUnpinned: the item predates pinning; there is no base to drift.
	DriftUnpinned = "unpinned"
	// DriftUnknown: the audit trail could not be read.
	DriftUnknown = "unknown"
)

// qaDriftEvent is one drift_detected row as the report shows it.
type qaDriftEvent struct {
	RepositoryID string `json:"repository_id"`
	Old          string `json:"old"`
	New          string `json:"new"`
	Detail       string `json:"detail,omitempty"`
}

// qaBaseRecord is the report's base_drift object.
type qaBaseRecord struct {
	Status   string         `json:"status"`
	Received *baseDrift     `json:"received,omitempty"`
	Events   []qaDriftEvent `json:"events,omitempty"`
}

// baseReader is the slice of *snapshot.Store QA reads.
type baseReader interface {
	GetAll(ctx context.Context, workID uuid.UUID) ([]snapshot.Pin, error)
	Events(ctx context.Context, workID uuid.UUID) ([]snapshot.Event, error)
}

// qaBase returns the report's base_sha and base_drift: the base_sha qa.ready
// carried (else the item's single/common pin), and the drift status from what
// qa.ready carried plus the item's drift_detected events.
func qaBase(ctx context.Context, store baseReader, workID uuid.UUID, msg qaReadyMessage) (string, qaBaseRecord) {
	rec := qaBaseRecord{Status: DriftNone, Received: msg.BaseDrift}
	baseSHA := msg.BaseSHA
	if store == nil {
		if baseSHA == "" {
			rec.Status = DriftUnpinned
		}
		return baseSHA, rec
	}
	pins, err := store.GetAll(ctx, workID)
	if err != nil {
		log.Printf("qa-agent: work_id=%s: read pins: %v", workID, err)
		rec.Status = DriftUnknown
		return baseSHA, rec
	}
	if baseSHA == "" {
		baseSHA = snapshot.SHAFor(pins, nil)
	}
	if len(pins) == 0 && baseSHA == "" {
		snapshot.NoteUnpinned(agentID, workID, "QA report base")
		rec.Status = DriftUnpinned
		return "", rec
	}
	events, err := store.Events(ctx, workID)
	if err != nil {
		log.Printf("qa-agent: work_id=%s: read base events: %v", workID, err)
		rec.Status = DriftUnknown
		return baseSHA, rec
	}
	for _, ev := range events {
		if ev.Kind != snapshot.EventDriftDetected {
			continue
		}
		rec.Events = append(rec.Events, qaDriftEvent{RepositoryID: ev.RepositoryID.String(), Old: ev.OldSHA, New: ev.NewSHA, Detail: ev.Detail})
	}
	if len(rec.Events) > 0 || msg.BaseDrift != nil {
		rec.Status = DriftDetected
	}
	return baseSHA, rec
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
