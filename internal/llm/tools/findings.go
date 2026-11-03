package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	// findingRangeMax bounds a citation: one research read window
	// (SetReadWindow 120); a claim about more than that is several claims.
	findingRangeMax = 120
	// findingExcerptMax bounds the excerpt kept with a finding.
	findingExcerptMax = 240
	// findingRecordedPrefix opens every successful record_finding result;
	// the loop recognises a productive turn by it.
	findingRecordedPrefix = "recorded finding #"
	// findingsHeader opens the ledger's findings section.
	findingsHeader = "## Findings so far"
	// findingsSectionMax bounds that section; the oldest findings fall off
	// (they are in the store) and a count line says how many.
	findingsSectionMax = 2500
)

// findingsSection renders the findings for the ledger message, newest last,
// de-duplicated by claim and citation, within findingsSectionMax.
func findingsSection(fs []CitedFinding) string {
	if len(fs) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var lines []string
	for _, f := range fs {
		line := fmt.Sprintf("- %s (%s:%d-%d)", f.Claim, f.Path, f.StartLine, f.EndLine)
		if seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	dropped := 0
	for {
		body := strings.Join(lines, "\n")
		head := findingsHeader + "\n"
		if dropped > 0 {
			head += fmt.Sprintf("(+%d earlier findings, kept in the store)\n", dropped)
		}
		if len(head)+len(body) <= findingsSectionMax || len(lines) <= 1 {
			return head + body
		}
		lines = lines[1:]
		dropped++
	}
}

// RecordFinding is the tool definition offered to loops with
// LoopOptions.Findings.
func RecordFinding() adapter.Tool {
	return adapter.Tool{
		Name: "record_finding",
		Description: "Record one thing you learned with the lines that show it, after each read and before the next: " +
			"reads leave your context after one turn; findings stay. Lines you have read only; at most 120 per claim.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"claim":{"type":"string","description":"One sentence"},"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer","description":"Default start_line"},"repo":{"type":"string"}},"required":["claim","path","start_line"]}`),
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

// markSeen records that a search (codex_grep, codex_similar_code) showed
// the model line of rel: a citation of that line is grounded, as a read
// is. Live, code_trace ended stuck on three refusals of lines a grep had
// shown it.
func (e *Executor) markSeen(rel string, line int) {
	rel = pinPath(rel)
	if rel == "" || line <= 0 {
		return
	}
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	if e.seenCov == nil {
		e.seenCov = map[string]map[int]bool{}
	}
	k := filepath.Clean(rel)
	if e.seenCov[k] == nil {
		e.seenCov[k] = map[int]bool{}
	}
	e.seenCov[k][line] = true
}

// hasRead reports whether lines start..end of rel overlap a range the loop
// has read (markRead), or were every one shown by a search (markSeen), so a
// finding can only cite what the model was shown.
func (e *Executor) hasRead(rel string, start, end int) bool {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	k := filepath.Clean(rel)
	if c := e.readCov[k]; c != nil {
		for _, r := range c.ranges {
			if start <= r[1] && end >= r[0] {
				return true
			}
		}
	}
	seen := e.seenCov[k]
	if seen == nil {
		return false
	}
	for l := start; l <= end; l++ {
		if !seen[l] {
			return false
		}
	}
	return true
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

	// The citation is checked against the tree the loop read (readCov is
	// kept for e.repoPath); a repo that resolves elsewhere is refused
	// rather than excerpted from a tree the model never saw (review #3).
	repo := strings.TrimSpace(stringMapField(args, "repo"))
	if e.codexReadsAvailable() {
		root, _, rerr := e.codexReadRoot(ctx, repo)
		if rerr != nil {
			return refuse(fmt.Sprintf("repo %q: %v", repo, rerr))
		}
		if filepath.Clean(root) != filepath.Clean(e.repoPath) {
			return refuse(fmt.Sprintf("repo %q is not the tree this task reads; cite what you read here", repo))
		}
	}
	full := filepath.Join(e.repoPath, filepath.FromSlash(rel))
	raw, err := os.ReadFile(full)
	if err != nil {
		return refuse(fmt.Sprintf("cannot read %q: %v", rel, err))
	}
	// Lines are counted as codex_read_file counts them (review #4).
	lines := fileLinesOf(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if int(end) > len(lines) {
		return refuse(fmt.Sprintf("%s has %d lines; cite lines that exist", rel, len(lines)))
	}
	if !e.hasRead(rel, int(start), int(end)) {
		return refuse(fmt.Sprintf("you have not read %s lines %d-%d in this task; read them first, then cite what they show", rel, int(start), int(end)))
	}
	cited := make([]string, 0, int(end-start)+1)
	for _, l := range lines[int(start)-1 : int(end)] {
		cited = append(cited, strings.TrimSuffix(l, "\n"))
	}
	f := CitedFinding{
		Claim:     claim,
		Path:      rel,
		StartLine: int(start),
		EndLine:   int(end),
		Excerpt:   truncateRunes(strings.Join(cited, "\n"), findingExcerptMax),
	}
	e.observedMu.Lock()
	for i, prev := range e.findings {
		if prev.Claim == f.Claim && prev.Path == f.Path && prev.StartLine == f.StartLine && prev.EndLine == f.EndLine {
			e.observedMu.Unlock()
			// The same claim again is not a new finding (pattern_match
			// recorded one claim four times live); nothing to store.
			return adapter.ToolResult{ToolCallID: callID, Content: fmt.Sprintf("already recorded as #%d: %s (%s:%d-%d); record something new or write your answer", i+1, f.Claim, f.Path, f.StartLine, f.EndLine)}
		}
	}
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

// closingAnswerMessage is the one user message of the closing call.
const closingAnswerMessage = "You have reached this task's limit. Write your answer now from your findings (listed in the ledger): " +
	"what the acceptance criteria need, each point with its file:line citation; say what is missing. No more tool calls."

// closingAnswer ends a loop that hit its iteration cap or the caller's
// budget with an answer written from the findings it recorded: one call,
// no tools, FinishReason "closed_at_limit". Live, pattern_match explored
// for 25 iterations, recorded 13 findings and returned nothing.
func closingAnswer(ctx context.Context, llmAdapter adapter.ModelAdapter, req adapter.CompletionRequest, why string) (*adapter.CompletionResponse, error) {
	req.Tools = nil
	req.ToolChoice = ""
	req.ToolResults = nil
	req.Messages = append(append([]adapter.ChatMessage(nil), req.Messages...), adapter.ChatMessage{Role: adapter.RoleUser, Content: closingAnswerMessage})
	log.Printf("tool-loop: %s — closing with an answer from the recorded findings", why)
	resp, err := llmAdapter.Complete(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("closing answer after %s: %w", why, err)
	}
	resp.FinishReason = "closed_at_limit"
	resp.ToolCalls = nil
	return resp, nil
}

// isFindingRecorded reports a successful record_finding result.
func isFindingRecorded(r adapter.ToolResult) bool {
	return !r.IsError && strings.HasPrefix(r.Content, findingRecordedPrefix)
}

// The read gate. The live run on qwen3.6:35b made 25 reads and no
// record_finding call: a strategy line is not enough. A read that follows a
// read nothing was recorded from carries readGateNote; the next one is not
// served (readGateRefusal) until something is recorded; after that reads
// are served again, so a model that will not record is slowed, not starved.
const (
	readGateNote = "[unrecorded read] You recorded nothing from your previous read. This result leaves your context after this turn: " +
		"record_finding what it shows (one claim + lines) before the next read, or the next read is not served."
	readGateRefusal = "record_finding: not served — you have read twice without recording anything. " +
		"Call record_finding with what your last read showed (one claim, path, lines); then read on."
)

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
