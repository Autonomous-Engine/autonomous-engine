package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// qaBaseSHA is qa.ready's base_sha (M2-02): the pin of repositoryID when the
// event names a repository, otherwise the work item's single (or common)
// pin; "" for a legacy-unpinned item (logged once, counted), for pins that
// differ across repositories, or when the read fails (logged). It is
// informational until M2-03.
func qaBaseSHA(ctx context.Context, db *pgxpool.Pool, workID uuid.UUID, repositoryID string) string {
	if db == nil {
		return ""
	}
	pins, err := snapshot.NewStore(db).GetAll(ctx, workID)
	if err != nil {
		log.Printf("status-orchestrator: work_id=%s: read base pins for qa.ready: %v", workID, err)
		return ""
	}
	if len(pins) == 0 {
		snapshot.NoteUnpinned(agentID, workID, "qa.ready")
		return ""
	}
	var repo *uuid.UUID
	if id, err := uuid.Parse(repositoryID); err == nil {
		repo = &id
	}
	return snapshot.SHAFor(pins, repo)
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
