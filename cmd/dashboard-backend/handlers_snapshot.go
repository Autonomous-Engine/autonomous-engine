package main

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Repository snapshot pins on the work item detail (M2-02): the base commit
// each repository was pinned to at intake (copied to subtasks by the
// planner), and the base_sha_events audit trail (resolved, drift_detected,
// repinned). Both are empty arrays for a legacy-unpinned item.

// basePinView is one work_item_base_shas row with its repository's name.
type basePinView struct {
	RepositoryID   string    `json:"repository_id"`
	RepositoryName string    `json:"repository_name"`
	BaseRef        string    `json:"base_ref"`
	BaseSHA        string    `json:"base_sha"`
	ResolvedAt     time.Time `json:"resolved_at"`
}

// baseEventView is one base_sha_events row with its repository's name.
type baseEventView struct {
	ID             string    `json:"id"`
	RepositoryID   string    `json:"repository_id"`
	RepositoryName string    `json:"repository_name"`
	Kind           string    `json:"kind"`
	OldSHA         string    `json:"old_sha"`
	NewSHA         string    `json:"new_sha"`
	Detail         string    `json:"detail"`
	CreatedAt      time.Time `json:"created_at"`
}

type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const basePinsSQL = `
SELECT p.repository_id::text, COALESCE(r.name, ''), p.base_ref, p.base_sha, p.resolved_at
  FROM work_item_base_shas p
  LEFT JOIN repositories r ON r.id = p.repository_id
 WHERE p.work_item_id = $1
 ORDER BY r.name, p.repository_id`

const baseEventsSQL = `
SELECT e.id::text, e.repository_id::text, COALESCE(r.name, ''), e.kind, e.old_sha, e.new_sha, e.detail, e.created_at
  FROM base_sha_events e
  LEFT JOIN repositories r ON r.id = e.repository_id
 WHERE e.work_item_id = $1
 ORDER BY e.created_at, e.id`

// loadBaseSnapshot returns the item's pins and events. A query error is
// logged and yields empty arrays: the detail page must still render.
func loadBaseSnapshot(ctx context.Context, db rowQuerier, id uuid.UUID) ([]basePinView, []baseEventView) {
	pins := []basePinView{}
	events := []baseEventView{}
	if db == nil {
		return pins, events
	}
	if rows, err := db.Query(ctx, basePinsSQL, id); err != nil {
		log.Printf("handlers: get base pins for %s: %v", id, err)
	} else {
		for rows.Next() {
			var p basePinView
			if err := rows.Scan(&p.RepositoryID, &p.RepositoryName, &p.BaseRef, &p.BaseSHA, &p.ResolvedAt); err != nil {
				log.Printf("handlers: scan base pin for %s: %v", id, err)
				continue
			}
			pins = append(pins, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("handlers: read base pins for %s: %v", id, err)
		}
	}
	if rows, err := db.Query(ctx, baseEventsSQL, id); err != nil {
		log.Printf("handlers: get base sha events for %s: %v", id, err)
	} else {
		for rows.Next() {
			var e baseEventView
			if err := rows.Scan(&e.ID, &e.RepositoryID, &e.RepositoryName, &e.Kind, &e.OldSHA, &e.NewSHA, &e.Detail, &e.CreatedAt); err != nil {
				log.Printf("handlers: scan base sha event for %s: %v", id, err)
				continue
			}
			events = append(events, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			log.Printf("handlers: read base sha events for %s: %v", id, err)
		}
	}
	return pins, events
}
