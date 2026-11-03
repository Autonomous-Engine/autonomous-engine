package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The work item's findings are the memory a task starts from: what earlier
// tasks and rounds recorded (research_findings), newest first, instead of
// re-reading the same files. A task never sees its own rows (a re-delivered
// task would otherwise start from itself).

const (
	// knownFindingsLimit is how many findings a task is handed.
	knownFindingsLimit = 40
	// knownFindingsMax bounds the section; it is re-sent on every call.
	knownFindingsMax = 3000
	// knownFindingsExcerpt bounds the quoted excerpt per finding.
	knownFindingsExcerpt = 120
	knownFindingsHeader  = "## Known findings (recorded by earlier research on this work item; cite them as they are, or verify with one read)"
)

// loadKnownFindings returns the work item's findings recorded by tasks
// other than excludeTaskID, newest first.
func loadKnownFindings(ctx context.Context, pool *pgxpool.Pool, workID uuid.UUID, excludeTaskID string) ([]tools.CitedFinding, error) {
	rows, err := pool.Query(ctx, `SELECT claim, path, start_line, end_line, excerpt FROM research_findings
		WHERE work_item_id = $1 AND task_id <> $2 ORDER BY created_at DESC LIMIT $3`, workID, excludeTaskID, knownFindingsLimit)
	if err != nil {
		return nil, fmt.Errorf("known findings: %w", err)
	}
	defer rows.Close()
	var out []tools.CitedFinding
	for rows.Next() {
		var f tools.CitedFinding
		if err := rows.Scan(&f.Claim, &f.Path, &f.StartLine, &f.EndLine, &f.Excerpt); err != nil {
			return nil, fmt.Errorf("known findings: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// knownFindingsSection renders the findings for the task message (newest
// first, as loaded), within knownFindingsMax. Empty when there are none.
func knownFindingsSection(fs []tools.CitedFinding) string {
	if len(fs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n" + knownFindingsHeader)
	for _, f := range fs {
		excerpt := strings.Join(strings.Fields(f.Excerpt), " ")
		if r := []rune(excerpt); len(r) > knownFindingsExcerpt {
			excerpt = string(r[:knownFindingsExcerpt])
		}
		line := fmt.Sprintf("\n- %s (%s:%d-%d) — %q", f.Claim, f.Path, f.StartLine, f.EndLine, excerpt)
		if b.Len()+len(line) > knownFindingsMax {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
