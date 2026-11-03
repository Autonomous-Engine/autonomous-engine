package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A research task's memory is its findings, not its transcript: a read is
// in the context for one turn, and what the model kept from it is a
// CitedFinding — a claim with a citation the executor verified against
// the read-only tree and the loop's own reads, and an excerpt the executor
// took itself. Findings are rendered in the evidence ledger, stored per
// work item (research_findings) and recalled by later tasks and rounds.
// Measured before this: 63 of 96 research tool calls were raw file reads
// re-read after compaction, and the repeated-read guard ended the tasks.

// CitedFinding is one claim with its verified citation.
type CitedFinding struct {
	Claim     string `json:"claim"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Excerpt   string `json:"excerpt"`
}

const (
	// findingClaimMax bounds a claim; a finding is a sentence, not a report.
	findingClaimMax = 400
	// findingRangeMax bounds a citation: a claim about more than 60 lines
	// is several claims.
	findingRangeMax = 60
	// findingExcerptMax bounds the excerpt kept with a finding.
	findingExcerptMax = 240
	// findingRecordedPrefix opens every successful record_finding result;
	// the loop recognises a productive turn by it.
	findingRecordedPrefix = "recorded finding #"
)

// RecordFinding is the tool definition offered to loops with
// LoopOptions.Findings.
func RecordFinding() adapter.Tool {
	return adapter.Tool{
		Name: "record_finding",
		Description: "Record one thing you learned, with the lines that show it. Call it after every read, before reading on: " +
			"the read leaves your context after one turn and only your findings stay. The lines must be ones you have read; " +
			"the excerpt is taken from the file for you. One claim per call, at most 60 lines.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"claim":{"type":"string","description":"One sentence stating what the cited lines show"},"path":{"type":"string","description":"Repo-relative path you read"},"start_line":{"type":"integer","description":"1-based first cited line"},"end_line":{"type":"integer","description":"1-based last cited line (default start_line)"},"repo":{"type":"string","description":"Repo name if the product has several"}},"required":["claim","path","start_line"]}`),
	}
}

// Findings returns the findings recorded in this executor, in order.
func (e *Executor) Findings() []CitedFinding {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	return append([]CitedFinding(nil), e.findings...)
}

// SetFindingStore sets where recorded findings are also written (the
// database, when the loop has one). A store error does not refuse the
// finding: it is logged in the result's note.
func (e *Executor) SetFindingStore(f func(context.Context, CitedFinding) error) { e.findingStore = f }

// SetFindingTask names the research task the findings belong to; the
// loop's database store writes it with every row.
func (e *Executor) SetFindingTask(taskID, researchType string) {
	e.findingTaskID, e.findingResearchType = taskID, researchType
}

// hasRead reports whether lines start..end of rel overlap a range the loop
// has read (markRead), so a finding can only cite what the model was shown.
func (e *Executor) hasRead(rel string, start, end int) bool {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	c := e.readCov[filepath.Clean(rel)]
	if c == nil {
		return false
	}
	for _, r := range c.ranges {
		if start <= r[1] && end >= r[0] {
			return true
		}
	}
	return false
}

// recordFinding executes record_finding.
func (e *Executor) recordFinding(ctx context.Context, callID string, args map[string]interface{}) adapter.ToolResult {
	refuse := func(msg string) adapter.ToolResult {
		return adapter.ToolResult{ToolCallID: callID, Content: "record_finding: " + msg, IsError: true}
	}
	claim := strings.TrimSpace(stringMapField(args, "claim"))
	if claim == "" {
		return refuse("claim is required: one sentence stating what the cited lines show")
	}
	if n := len([]rune(claim)); n > findingClaimMax {
		return refuse(fmt.Sprintf("claim is %d chars; keep it under %d (one claim per finding)", n, findingClaimMax))
	}
	rawPath := strings.TrimPrefix(path.Clean(strings.ReplaceAll(strings.TrimSpace(stringMapField(args, "path")), "\\", "/")), "./")
	rel, err := validateRepoRelPath(rawPath)
	if err != nil {
		return refuse(err.Error())
	}
	start, _ := args["start_line"].(float64)
	end, _ := args["end_line"].(float64)
	if start < 1 {
		return refuse("start_line must be a 1-based line number")
	}
	if end < start {
		end = start
	}
	if int(end-start)+1 > findingRangeMax {
		return refuse(fmt.Sprintf("cites %d lines; a finding cites at most %d — split it into one claim per range", int(end-start)+1, findingRangeMax))
	}

	root := e.repoPath
	if e.codexReadsAvailable() {
		if r, _, rerr := e.codexReadRoot(ctx, stringMapField(args, "repo")); rerr == nil {
			root = r
		}
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	raw, err := os.ReadFile(full)
	if err != nil {
		return refuse(fmt.Sprintf("cannot read %q: %v", rel, err))
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n"), "\n")
	if int(end) > len(lines) {
		return refuse(fmt.Sprintf("%s has %d lines; cite lines that exist", rel, len(lines)))
	}
	if !e.hasRead(rel, int(start), int(end)) {
		return refuse(fmt.Sprintf("you have not read %s lines %d-%d in this task; read them first, then cite what they show", rel, int(start), int(end)))
	}

	f := CitedFinding{
		Claim:     claim,
		Path:      rel,
		StartLine: int(start),
		EndLine:   int(end),
		Excerpt:   truncateRunes(strings.Join(lines[int(start)-1:int(end)], "\n"), findingExcerptMax),
	}
	e.observedMu.Lock()
	e.findings = append(e.findings, f)
	n := len(e.findings)
	e.observedMu.Unlock()
	content := fmt.Sprintf("%s%d: %s (%s:%d-%d)", findingRecordedPrefix, n, f.Claim, f.Path, f.StartLine, f.EndLine)
	if e.findingStore != nil {
		if serr := e.findingStore(ctx, f); serr != nil {
			content += "\n(not stored: " + serr.Error() + ")"
		}
	}
	return adapter.ToolResult{ToolCallID: callID, Content: content}
}

// isFindingRecorded reports a successful record_finding result.
func isFindingRecorded(r adapter.ToolResult) bool {
	return !r.IsError && strings.HasPrefix(r.Content, findingRecordedPrefix)
}

// insertFinding stores one finding for the work item.
func insertFinding(ctx context.Context, db *pgxpool.Pool, workItemID uuid.UUID, taskID, researchType string, productID *uuid.UUID, f CitedFinding) error {
	_, err := db.Exec(ctx, `INSERT INTO research_findings (work_item_id, task_id, research_type, product_id, claim, path, start_line, end_line, excerpt)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		workItemID, taskID, researchType, productID, f.Claim, f.Path, f.StartLine, f.EndLine, f.Excerpt)
	return err
}

// dbFindingStore is the loop's store when it has a database and a work item.
func dbFindingStore(db *pgxpool.Pool, workItemID uuid.UUID, e *Executor) func(context.Context, CitedFinding) error {
	return func(ctx context.Context, f CitedFinding) error {
		return insertFinding(ctx, db, workItemID, e.findingTaskID, e.findingResearchType, e.productID, f)
	}
}
