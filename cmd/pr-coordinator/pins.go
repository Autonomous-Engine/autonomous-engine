package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/snapshot"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pinnedBaseSHA is the work item's pinned base commit for repositoryID
// (M2-02), for pr.ready's base_sha. "" when the item is legacy-unpinned
// (logged once, counted) or the read fails (logged): the field is
// informational until M2-03.
func pinnedBaseSHA(ctx context.Context, db *pgxpool.Pool, workID, repositoryID uuid.UUID) string {
	if db == nil {
		return ""
	}
	pin, found, err := snapshot.NewStore(db).Get(ctx, workID, repositoryID)
	if err != nil {
		log.Printf("pr-coordinator: work_id=%s repo=%s: read base pin: %v", workID, repositoryID, err)
		return ""
	}
	if !found {
		snapshot.NoteUnpinned(agentID, workID, "pr.ready for repository "+repositoryID.String())
		return ""
	}
	return pin.BaseSHA
}

// The unpinned series reads 0 from startup rather than being absent.
func init() { snapshot.RegisterUnpinned(agentID) }
