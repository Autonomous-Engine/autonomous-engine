package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The work item's findings are the memory later stages start from: what
// research recorded (research_findings), newest first, instead of
// re-reading the same files. Research recalls them per task (never its own
// rows); implementation recalls them all — live, the implementation agent
// read ten files before its first edit while research had already cited
// the lines that matter.

const (
	// KnownFindingsLimit is how many findings a task is handed.
	KnownFindingsLimit = 40
	// KnownFindingsMax bounds the section; it is re-sent on every call.
	KnownFindingsMax = 3000
	// knownFindingsExcerpt bounds the quoted excerpt per finding.
	knownFindingsExcerpt = 120
	// KnownFindingsHeader opens the section.
	KnownFindingsHeader = "## Known findings (recorded by earlier research on this work item; cite them as they are, or verify with one read)"
)

// LoadKnownFindings returns the work item's findings recorded by tasks
// other than excludeTaskID ("" excludes none), newest first. A finding
// recorded again by a later round (same claim and citation) is returned
// once, so duplicates neither pad the section nor use up the limit.
func LoadKnownFindings(ctx context.Context, pool *pgxpool.Pool, workID uuid.UUID, excludeTaskID string) ([]CitedFinding, error) {
	if pool == nil {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `SELECT claim, path, start_line, end_line, excerpt FROM (
			SELECT DISTINCT ON (claim, path, start_line, end_line) claim, path, start_line, end_line, excerpt, created_at
			FROM research_findings WHERE work_item_id = $1 AND task_id <> $2
			ORDER BY claim, path, start_line, end_line, created_at DESC
		) once ORDER BY created_at DESC LIMIT $3`, workID, excludeTaskID, KnownFindingsLimit)
	if err != nil {
		return nil, fmt.Errorf("known findings: %w", err)
	}
	defer rows.Close()
	var out []CitedFinding
	for rows.Next() {
		var f CitedFinding
		if err := rows.Scan(&f.Claim, &f.Path, &f.StartLine, &f.EndLine, &f.Excerpt); err != nil {
			return nil, fmt.Errorf("known findings: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// KnownFindingsSection renders the findings for a task message (newest
// first, as loaded), within KnownFindingsMax. Empty when there are none.
func KnownFindingsSection(fs []CitedFinding) string {
	if len(fs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n" + KnownFindingsHeader)
	for _, f := range fs {
		excerpt := strings.Join(strings.Fields(f.Excerpt), " ")
		if r := []rune(excerpt); len(r) > knownFindingsExcerpt {
			excerpt = string(r[:knownFindingsExcerpt])
		}
		line := fmt.Sprintf("\n- %s (%s:%d-%d) — %q", f.Claim, f.Path, f.StartLine, f.EndLine, excerpt)
		if b.Len()+len(line) > KnownFindingsMax {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
