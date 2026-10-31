package main

import (
	"context"
	"log"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
)

// Codex pins on the dashboard (codex coherence Part 1): the codex version and
// modules a work item was planned against (work_item_codex_pins, migration
// 060), and what the codex re-plan did with it (work_item_codex_events,
// migration 062): re-plans with the modules that changed, escalations,
// "codex changed since planning" notes and "continue as is" answers.
//
// The work item list (GET /api/work-items) carries codex_pin and, for an
// item whose latest codex event left it on its plan (noted or escalated),
// codex_changed_since_planning; the detail carries codex_pin and
// codex_events.

// codexPinView is one work_item_codex_pins row.
type codexPinView struct {
	ProductVersion int       `json:"product_version"`
	Branch         string    `json:"branch"`
	Modules        []string  `json:"modules"`
	PinnedAt       time.Time `json:"pinned_at"`
}

// codexEventView is one work_item_codex_events row.
type codexEventView struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Automatic   bool       `json:"automatic"`
	Branch      string     `json:"branch"`
	FromVersion int        `json:"from_version"`
	ToVersion   int        `json:"to_version"`
	Modules     []string   `json:"modules"`
	Sections    []string   `json:"sections"`
	ItemState   string     `json:"item_state"`
	Reason      string     `json:"reason"`
	PromptID    *uuid.UUID `json:"prompt_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// codexNoteView flags an item whose codex changed since planning while it
// stayed on its plan.
type codexNoteView struct {
	Kind      string    `json:"kind"`
	ToVersion int       `json:"to_version"`
	Modules   []string  `json:"modules"`
	Sections  []string  `json:"sections"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// workItemListEntry is a listed work item with its codex pin.
type workItemListEntry struct {
	*models.WorkItem
	CodexPin                  *codexPinView  `json:"codex_pin,omitempty"`
	CodexChangedSincePlanning *codexNoteView `json:"codex_changed_since_planning,omitempty"`
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

const codexPinsForItemsSQL = `
SELECT work_item_id, product_version, branch, modules, created_at
  FROM work_item_codex_pins
 WHERE work_item_id = ANY($1)`

// codexNotesForItemsSQL is each item's latest codex event when it is a note
// or an escalation (the item stayed on its plan).
const codexNotesForItemsSQL = `
SELECT work_item_id, kind, to_version, modules, sections, reason, created_at
  FROM (SELECT DISTINCT ON (work_item_id) work_item_id, kind, to_version, modules, sections, reason, created_at
          FROM work_item_codex_events
         WHERE work_item_id = ANY($1)
         ORDER BY work_item_id, created_at DESC, id DESC) latest
 WHERE kind IN ('noted', 'escalated')`

// withCodexPins wraps items with their pins and notes, in two queries. A
// query error is logged and leaves the fields out: the list must render.
func withCodexPins(ctx context.Context, db rowQuerier, items []*models.WorkItem) []workItemListEntry {
	out := make([]workItemListEntry, len(items))
	ids := make([]uuid.UUID, len(items))
	for i, it := range items {
		out[i].WorkItem = it
		ids[i] = it.ID
	}
	if db == nil || len(items) == 0 {
		return out
	}
	pins := map[uuid.UUID]*codexPinView{}
	if rows, err := db.Query(ctx, codexPinsForItemsSQL, ids); err != nil {
		log.Printf("handlers: list codex pins: %v", err)
	} else {
		for rows.Next() {
			var id uuid.UUID
			p := &codexPinView{}
			if err := rows.Scan(&id, &p.ProductVersion, &p.Branch, &p.Modules, &p.PinnedAt); err != nil {
				log.Printf("handlers: scan codex pin: %v", err)
				continue
			}
			p.Modules = nonNil(p.Modules)
			pins[id] = p
		}
		rows.Close()
	}
	notes := map[uuid.UUID]*codexNoteView{}
	if rows, err := db.Query(ctx, codexNotesForItemsSQL, ids); err != nil {
		log.Printf("handlers: list codex notes: %v", err)
	} else {
		for rows.Next() {
			var id uuid.UUID
			n := &codexNoteView{}
			if err := rows.Scan(&id, &n.Kind, &n.ToVersion, &n.Modules, &n.Sections, &n.Reason, &n.At); err != nil {
				log.Printf("handlers: scan codex note: %v", err)
				continue
			}
			n.Modules, n.Sections = nonNil(n.Modules), nonNil(n.Sections)
			notes[id] = n
		}
		rows.Close()
	}
	for i := range out {
		out[i].CodexPin = pins[out[i].ID]
		out[i].CodexChangedSincePlanning = notes[out[i].ID]
	}
	return out
}

const codexEventsForItemSQL = `
SELECT id::text, kind, automatic, branch, from_version, to_version, modules, sections, item_state, reason, prompt_id, created_at
  FROM work_item_codex_events
 WHERE work_item_id = $1
 ORDER BY created_at, id`

// loadCodexPin returns the item's pin (nil when it has none) and its codex
// events (never nil). A query error is logged: the detail must render.
func loadCodexPin(ctx context.Context, db rowQuerier, id uuid.UUID) (*codexPinView, []codexEventView) {
	events := []codexEventView{}
	if db == nil {
		return nil, events
	}
	entries := withCodexPins(ctx, db, []*models.WorkItem{{ID: id}})
	pin := entries[0].CodexPin
	rows, err := db.Query(ctx, codexEventsForItemSQL, id)
	if err != nil {
		log.Printf("handlers: get codex events for %s: %v", id, err)
		return pin, events
	}
	defer rows.Close()
	for rows.Next() {
		var e codexEventView
		if err := rows.Scan(&e.ID, &e.Kind, &e.Automatic, &e.Branch, &e.FromVersion, &e.ToVersion, &e.Modules, &e.Sections, &e.ItemState, &e.Reason, &e.PromptID, &e.CreatedAt); err != nil {
			log.Printf("handlers: scan codex event for %s: %v", id, err)
			continue
		}
		e.Modules, e.Sections = nonNil(e.Modules), nonNil(e.Sections)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("handlers: read codex events for %s: %v", id, err)
	}
	return pin, events
}
