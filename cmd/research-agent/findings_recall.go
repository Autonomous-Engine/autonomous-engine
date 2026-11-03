package main

import (
	"context"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The work item's findings are the memory a task starts from (shared with
// the implementation agent: internal/llm/tools/known_findings.go). A task
// never sees its own rows (a re-delivered task would otherwise start from
// itself).

const knownFindingsMax = tools.KnownFindingsMax

// loadKnownFindings returns the work item's findings recorded by tasks
// other than excludeTaskID, newest first.
func loadKnownFindings(ctx context.Context, pool *pgxpool.Pool, workID uuid.UUID, excludeTaskID string) ([]tools.CitedFinding, error) {
	return tools.LoadKnownFindings(ctx, pool, workID, excludeTaskID)
}

// knownFindingsSection renders the findings for the task message.
func knownFindingsSection(fs []tools.CitedFinding) string {
	return tools.KnownFindingsSection(fs)
}
