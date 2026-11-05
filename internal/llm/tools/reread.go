package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// R12 X2: a live run (attempt 12) made 43 read_file calls in 25 turns, most of
// them of the same two HTML windows it still had in its recent results.
// With LoopOptions.DedupReads, a read of lines the model still has — a
// result that stays in the transcript it sees next, or a pinned read in
// "Current file contents" — and that are unchanged since, is answered with
// one line instead of the content. It still counts as a read.

// rereadMarker is in every short answer; the ledger and the pins know a
// re-read by it.
const rereadMarker = ": unchanged since your last read of lines "

// isRereadResult reports a short "unchanged" answer to a read.
func isRereadResult(content string) bool {
	return len(content) < 400 && strings.Contains(content, rereadMarker)
}

func rereadResult(path string, start, end int, pinned bool) string {
	where := ""
	if pinned {
		where = " (pinned)"
	}
	return fmt.Sprintf("%s%s%d-%d%s; edit directly or request a different range", path, rereadMarker, start, end, where)
}

// readRecord is the content of lines start..end of path a read returned
// (line breaks normalised to \n), and the tool-call turn it came in.
type readRecord struct {
	path       string
	start, end int
	body       string
	turn       int
}

// readMemo is the loop's record of what each read returned.
type readMemo struct {
	records []readRecord
	// floor: records from this turn or earlier are no longer in the
	// transcript (a checkpoint replaced it).
	floor int
}

// forgetTranscript is called when a checkpoint drops the transcript up to
// and including turn.
func (m *readMemo) forgetTranscript(turn int) { m.floor = turn }

// record keeps what each read call of the turn returned.
func (m *readMemo) record(e *Executor, calls []adapter.ToolCall, results []adapter.ToolResult, turn int) {
	for k, c := range calls {
		if !isReadTool(c.Name) || results[k].IsError || isRereadResult(results[k].Content) || resultLooksFailed(c.Name, results[k].Content) {
			continue
		}
		var args map[string]any
		_ = json.Unmarshal([]byte(c.Arguments), &args)
		p, _ := args["path"].(string)
		if c.Name == "read_file" {
			p = e.normalizePath(p)
		}
		if p = pinPath(p); p == "" {
			continue
		}
		content := results[k].Content
		start, end := readSpan(args, content)
		body := content
		if c.Name == "codex_read_file" {
			body = jsonField(content, "content")
		}
		lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
		if n := end - start + 1; n > 0 && n <= len(lines) {
			lines = lines[:n]
		} else {
			continue
		}
		m.records = append(m.records, readRecord{path: p, start: start, end: end, body: strings.Join(lines, "\n"), turn: turn})
	}
}

// answer returns the short answers, by call index, for the turn's reads of
// lines the model still has unchanged: a read from a turn that stays in
// the transcript after this one (the last keepRecent-1 turns, or an
// earlier call of this turn), or a pinned read. Reads after a write in the
// same turn are not answered (the write may change what they see). A pin
// an answer relies on is made the newest, so newer pins cannot evict it
// from the next request (R12 review).
func (m *readMemo) answer(ctx context.Context, e *Executor, calls []adapter.ToolCall, turn, keepRecent int, ledger *evidenceLedger) map[int]adapter.ToolResult {
	out := map[int]adapter.ToolResult{}
	var pins []pinnedRange
	if ledger != nil {
		pins = ledger.rendered
	}
	var thisTurn []readRecord
	for i, c := range calls {
		if isWriteTool(c.Name) || IsFileEditTool(c.Name) {
			break
		}
		if !isReadTool(c.Name) {
			continue
		}
		p, start, end, total, ok := e.readPlan(ctx, c)
		if !ok {
			continue
		}
		unchanged := func(path string, a, b int, body string) bool {
			cur, ok := e.currentLines(path, a, b)
			return ok && cur == body
		}
		hit, pinned := (*readRecord)(nil), false
		for k := len(m.records) - 1; k >= 0 && hit == nil; k-- {
			r := &m.records[k]
			if r.path == p && r.turn > m.floor && r.turn >= turn-keepRecent+1 && r.start <= start && end <= r.end && unchanged(p, r.start, r.end, r.body) {
				hit = r
			}
		}
		for k := range thisTurn {
			if r := &thisTurn[k]; hit == nil && r.path == p && r.start <= start && end <= r.end {
				hit = r
			}
		}
		for _, pr := range pins {
			if hit == nil && pr.path == p && pr.start <= start && end <= pr.end && unchanged(p, pr.start, pr.end, pr.body) {
				hit, pinned = &readRecord{path: p, start: pr.start, end: pr.end}, true
			}
		}
		if hit == nil {
			// A read that has left the transcript, of lines the model
			// recorded a finding from (unchanged since), is answered with
			// that finding: findings are the memory, not the transcript.
			if n, f, r := m.recordedFinding(e, p, start, end, unchanged); f != nil {
				res := adapter.ToolResult{ToolCallID: c.ID, Content: rereadFindingResult(p, r.start, r.end, n, *f)}
				e.markObserved(p)
				e.markRead(p, start, end, total)
				e.notifyReused(c, res)
				out[i] = res
				continue
			}
			if cur, ok := e.currentLines(p, start, end); ok {
				thisTurn = append(thisTurn, readRecord{path: p, start: start, end: end, body: cur, turn: turn})
			}
			continue
		}
		if pinned && ledger != nil {
			ledger.touchPin(p)
		}
		res := adapter.ToolResult{ToolCallID: c.ID, Content: rereadResult(p, hit.start, hit.end, pinned)}
		// Still a read: the read-before-edit gate and the read coverage
		// count it.
		e.markObserved(p)
		e.markRead(p, start, end, total)
		e.notifyReused(c, res)
		out[i] = res
	}
	return out
}

// recordedFinding is the newest finding of e citing lines that cover
// start..end of p, with its 1-based number and the read record (any turn)
// that returned those lines and still matches the file; nil when there is
// none. A finding cites lines the loop read, so a record normally exists.
func (m *readMemo) recordedFinding(e *Executor, p string, start, end int, unchanged func(string, int, int, string) bool) (int, *CitedFinding, *readRecord) {
	fs := e.Findings()
	for n := len(fs) - 1; n >= 0; n-- {
		f := fs[n]
		if pinPath(f.Path) != p || f.StartLine > start || end > f.EndLine {
			continue
		}
		for k := len(m.records) - 1; k >= 0; k-- {
			r := &m.records[k]
			if r.path == p && r.start <= start && end <= r.end && unchanged(p, r.start, r.end, r.body) {
				return n + 1, &fs[n], r
			}
		}
	}
	return 0, nil, nil
}

// rereadFindingResult is the short answer to a re-read of lines a finding
// covers; it carries rereadMarker (a re-read to the ledger) and stays
// under isRereadResult's bound.
func rereadFindingResult(path string, start, end, n int, f CitedFinding) string {
	return fmt.Sprintf("%s%s%d-%d; you recorded finding #%d from them: %s (%s:%d-%d). Cite it, or request a different range",
		path, rereadMarker, start, end, n, truncateRunes(f.Claim, 160), f.Path, f.StartLine, f.EndLine)
}

// assistantProseMax bounds the prose of an assistant turn kept in the
// transcript. Live (enmime, 2026-11-03 15:54) a model wrote 24k characters
// around one tool call; the one-turn window kept the turn whole, so the
// next call carried 6k tokens of the model's own words and the work meter
// counted them. The tool calls are what the history needs; the prose is
// kept as its head (the plan) and tail (the next step) with a note.
const assistantProseMax = 2000

// trimAssistantProse keeps content under assistantProseMax: the first 1500
// and last 500 characters with a note saying how much is gone.
func trimAssistantProse(content string) string {
	if len(content) <= assistantProseMax {
		return content
	}
	head, tail := assistantProseMax*3/4, assistantProseMax/4
	// Cut on rune boundaries.
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	for tail > 0 && !utf8.RuneStart(content[len(content)-tail]) {
		tail--
	}
	return fmt.Sprintf("%s\n[… %d chars trimmed from the transcript; the tool calls stand …]\n%s",
		content[:head], len(content)-head-tail, content[len(content)-tail:])
}

// readPlan is the path (pinPath form) and the lines start..end of total a
// read call would return now, without reading them for the model; ok is
// false for a read this cannot tell (an error, another repository's clone).
func (e *Executor) readPlan(ctx context.Context, c adapter.ToolCall) (path string, start, end, total int, ok bool) {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(c.Arguments), &args); err != nil {
		return "", 0, 0, 0, false
	}
	raw, _ := args["path"].(string)
	var rel string
	switch c.Name {
	case "read_file":
		rel = e.normalizePath(raw)
		if !strings.HasPrefix(filepath.Join(e.repoPath, filepath.Clean(rel)), e.repoPath) {
			return "", 0, 0, 0, false
		}
	case "codex_read_file":
		r, err := validateRepoRelPath(raw)
		if err != nil || !e.codexReadsAvailable() {
			return "", 0, 0, 0, false
		}
		repo, _ := args["repo"].(string)
		root, _, err := e.codexReadRoot(ctx, repo)
		if err != nil || filepath.Clean(root) != filepath.Clean(e.repoPath) {
			return "", 0, 0, 0, false
		}
		rel = r
	default:
		return "", 0, 0, 0, false
	}
	data, err := os.ReadFile(filepath.Join(e.repoPath, filepath.FromSlash(filepath.ToSlash(rel))))
	if err != nil {
		return "", 0, 0, 0, false
	}
	lines := fileLinesOf(strings.ReplaceAll(string(data), "\r\n", "\n"))
	total = len(lines)
	if window, _, w := e.defaultWindow(rel, args, lines); w {
		args = window
	}
	if c.Name == "codex_read_file" {
		start, end = 1, 0
		if v, ok := args["start_line"].(float64); ok && int(v) > 0 {
			start = int(v)
		}
		if v, ok := args["end_line"].(float64); ok && int(v) > 0 {
			end = int(v)
		}
		if end == 0 || end < start {
			end = start + e.readDefault() - 1
		}
		end = min(end, start+e.readMax()-1, total)
	} else {
		start, end, _ = readWindow(args, total, e.readDefault(), e.readMax())
	}
	if total == 0 || start > end {
		return "", 0, 0, 0, false
	}
	return pinPath(rel), start, end, total, true
}
