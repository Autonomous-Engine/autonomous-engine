package main

import (
	"context"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/google/uuid"
)

// recordAttemptSnapshot links this attempt to the configuration snapshot
// its resolution captured (M4-05): implementation_runs.config_snapshot_id,
// work_items.config_snapshot_id of the parent (outcomes roll up by the
// configuration that wrote the code), and — through the returned context —
// the role/agent type/snapshot/model tags on every inference_logs row the
// tool loops write. The evidence record gets it through
// evidence.Store.WithConfigSnapshot at the finalize seam. Every write is
// best-effort: a missing link is logged, never fatal.
func recordAttemptSnapshot(ctx context.Context, registry *agentregistry.Registry, runs *controller.Store, run *controller.Run, workID uuid.UUID, res *agentregistry.Resolution) context.Context {
	ctx = llm.WithInferenceTags(ctx, llm.TagsForResolution(res, workID))
	if res == nil || res.SnapshotID == "" {
		return ctx
	}
	if run != nil && runs != nil {
		if err := runs.SetConfigSnapshot(ctx, run.ID, res.SnapshotID); err != nil {
			log.Printf("implementation-agent: record config snapshot on run %s: %v", run.ID, err)
		}
	}
	if err := registry.RecordAttemptSnapshot(ctx, workID, res.SnapshotID); err != nil {
		log.Printf("implementation-agent: record config snapshot on work_id=%s: %v", workID, err)
	}
	return ctx
}
