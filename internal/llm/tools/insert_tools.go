package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// Insertion tools (R11 S2). In a live run (attempt 11) the model added a
// test by re-typing the whole suite around an anchor (`});`, then the
// suite's `describe(…)`) as edit_file's new_string; the duplicate guards rightly
// refused all four calls and the loop ended stuck. insert_after and
// append_to_file add lines without re-typing any: same read-before-edit
// gate, read coverage (the anchor must be in lines the model read), syntax
// check and duplicate check as edit_file.

// InsertAfter returns the insert_after tool definition.
func InsertAfter() adapter.Tool {
	return adapter.Tool{
		Name: "insert_after",
		Description: "Insert new lines after one line of an existing file; nothing existing is re-typed. " +
			"Give anchor_line (0 = top) or anchor_text (text of one unique line), inside lines you read. " +
			"Use it to add a test, function or import; edit_file changes lines.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"anchor_line":{"type":"integer"},"anchor_text":{"type":"string"},"text":{"type":"string","description":"Only the new lines"}},"required":["path","text"]}`),
	}
}

// AppendToFile returns the append_to_file tool definition.
func AppendToFile() adapter.Tool {
	return adapter.Tool{
		Name:        "append_to_file",
		Description: "Append new lines to the end of an existing file (read its last lines first); nothing existing is re-typed.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string","description":"Only the new lines"}},"required":["path","text"]}`),
	}
}

// IsFileEditTool reports whether a tool changes a file's content: edit_file,
// write_file, insert_after and append_to_file.
func IsFileEditTool(name string) bool {
	switch name {
	case "edit_file", "write_file", "insert_after", "append_to_file":
		return true
	}
	return false
}

// readCovers reports whether line (1-based) of path is inside the lines
// the model read; a file without a coverage record (written by the model,
// or never read in part) counts as known. cov describes the coverage.
func (e *Executor) readCovers(path string, line int) (ok bool, cov string) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	c := e.readCov[filepath.Clean(path)]
	if c == nil || c.full() {
		return true, ""
	}
	for _, r := range c.ranges {
		if line >= r[0] && line <= r[1] {
			return true, ""
		}
	}
	return false, c.String()
}

func (e *Executor) insertAfter(callID string, args map[string]interface{}) adapter.ToolResult {
	return e.insertLines(callID, "insert_after", args)
}

func (e *Executor) appendToFile(callID string, args map[string]interface{}) adapter.ToolResult {
	return e.insertLines(callID, "append_to_file", args)
}

// insertLines inserts args.text after the anchor line (insert_after) or
// the last line (append_to_file).
func (e *Executor) insertLines(callID, tool string, args map[string]interface{}) adapter.ToolResult {
	fail := func(msg string) adapter.ToolResult {
		return adapter.ToolResult{ToolCallID: callID, Content: msg, IsError: true}
	}
	path, _ := args["path"].(string)
	path = e.normalizePath(path)
	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return fail("text is required: the new lines to insert")
	}
	if !e.hasObserved(path) {
		return fail("REJECTED: read_file must be called on this path before " + tool + ". Call read_file(\"" + path + "\") first.")
	}
	fullPath := filepath.Join(e.repoPath, filepath.Clean(path))
	if !strings.HasPrefix(fullPath, e.repoPath) {
		return fail("Path traversal not allowed")
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fail(fmt.Sprintf("%s: %s does not exist; create it with write_file", tool, path))
		}
		return fail("Error reading file: " + err.Error())
	}
	content := string(data)
	lines := strings.SplitAfter(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)

	anchor := total
	if tool == "insert_after" {
		var msg string
		if anchor, msg = resolveAnchor(lines, args); msg != "" {
			return fail(msg)
		}
	}
	// The anchor must be in lines the model read (the top of the file for
	// anchor 0): it cannot place lines next to text it never saw.
	if probe := max(anchor, 1); total > 0 {
		if ok, cov := e.readCovers(path, probe); !ok {
			return fail(fmt.Sprintf("REJECTED: line %d of %s is outside the lines you read (%s); read_file with start_line=%d first.", probe, path, cov, max(probe-20, 1)))
		}
	}

	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	// The file's line ending at the anchor (no mixed endings).
	if ref := max(anchor, 1) - 1; ref < total && strings.HasSuffix(lines[ref], "\r\n") {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	// Idempotent: the same lines already right after the anchor, or (for
	// append_to_file) already ending the file. A single short line (`}`,
	// a blank) may legitimately repeat, so it is always inserted.
	trimmed := strings.TrimSpace(text)
	if strings.Contains(trimmed, "\n") || len(trimmed) >= 8 {
		rest := strings.Join(lines[anchor:], "")
		norm := func(s string) string { return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n") }
		switch {
		case tool == "append_to_file" && strings.HasSuffix(norm(content), norm(text)):
			return adapter.ToolResult{ToolCallID: callID, Content: insertAlreadyApplied(path, total-strings.Count(norm(text), "\n"))}
		case tool == "insert_after" && strings.HasPrefix(rest, text):
			return adapter.ToolResult{ToolCallID: callID, Content: insertAlreadyApplied(path, anchor+1)}
		}
	}
	head := strings.Join(lines[:anchor], "")
	if head != "" && !strings.HasSuffix(head, "\n") {
		if strings.HasSuffix(text, "\r\n") {
			head += "\r"
		}
		head += "\n"
	}
	newContent := head + text + strings.Join(lines[anchor:], "")

	if corrupt := detectSuspiciousDuplicates(content, newContent); corrupt != "" {
		return fail("REJECTED: the insertion introduces suspicious duplicates that did not exist in the original file. " + corrupt)
	}
	if rej := e.syntaxRejection(path, data, true, []byte(newContent)); rej != "" {
		return fail(rej)
	}
	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return fail("Error writing file: " + err.Error())
	}
	if rejection := e.validateAfterWrite(context.Background(), callID, path, data); rejection != nil {
		return *rejection
	}
	added := strings.Count(text, "\n")
	e.shiftReadRanges(path, anchor, added)
	where := fmt.Sprintf("after line %d", anchor)
	if tool == "append_to_file" {
		where = "at the end"
	}
	noun := "lines"
	if added == 1 {
		noun = "line"
	}
	return adapter.ToolResult{ToolCallID: callID, Content: fmt.Sprintf("Inserted %d %s into %s %s: now at lines %d-%d", added, noun, path, where, anchor+1, anchor+added)}
}

// insertAlreadyApplied is an insertion's "already applied" result (the
// ledger reads "already applied at line N").
func insertAlreadyApplied(path string, line int) string {
	return fmt.Sprintf("Insertion into %s already applied at line %d — no change (these lines are already there).", path, line)
}

// shiftReadRanges follows an insertion of delta lines after line anchor:
// read ranges after it move down, a range spanning it grows, and the
// inserted lines count as read (the model wrote them).
func (e *Executor) shiftReadRanges(path string, anchor, delta int) {
	e.observedMu.Lock()
	defer e.observedMu.Unlock()
	c := e.readCov[filepath.Clean(path)]
	if c == nil || delta == 0 {
		return
	}
	wasFull := c.full()
	c.total += delta
	if wasFull {
		c.ranges = [][2]int{{1, c.total}}
		return
	}
	var rs [][2]int
	for _, r := range c.ranges {
		switch {
		case r[0] > anchor:
			r[0], r[1] = r[0]+delta, r[1]+delta
		case r[1] > anchor:
			r[1] += delta
		}
		rs = append(rs, r)
	}
	rs = append(rs, [2]int{anchor + 1, anchor + delta})
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	merged := rs[:1]
	for _, r := range rs[1:] {
		last := &merged[len(merged)-1]
		if r[0] <= last[1]+1 {
			last[1] = max(last[1], r[1])
		} else {
			merged = append(merged, r)
		}
	}
	c.ranges = merged
}

// resolveAnchor is insert_after's anchor line (0 = before line 1): the
// anchor_line given, or the one line whose text contains anchor_text
// (with anchor_line, the match nearest it). msg is the error.
func resolveAnchor(lines []string, args map[string]interface{}) (int, string) {
	total := len(lines)
	line, hasLine := -1, false
	if v, ok := args["anchor_line"].(float64); ok {
		line, hasLine = int(v), true
	}
	at, _ := args["anchor_text"].(string)
	at = strings.TrimSpace(at)
	if at == "" {
		switch {
		case !hasLine:
			return 0, "insert_after needs anchor_line (0 = top of the file) or anchor_text"
		case line < 0 || line > total:
			return 0, fmt.Sprintf("anchor_line %d is outside the file (lines 1-%d; 0 = top)", line, total)
		}
		return line, ""
	}
	var matches []int
	for i, l := range lines {
		if strings.Contains(l, at) {
			matches = append(matches, i+1)
		}
	}
	switch {
	case len(matches) == 0:
		return 0, fmt.Sprintf("anchor_text %q is not in the file; pass anchor_line or copy one line's text", truncateRunes(at, 80))
	case len(matches) == 1:
		return matches[0], ""
	case hasLine:
		best := matches[0]
		for _, m := range matches {
			if abs(m-line) < abs(best-line) {
				best = m
			}
		}
		return best, ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "anchor_text %q is on %d lines; pass anchor_line=<one of them>:", truncateRunes(at, 80), len(matches))
	for k, m := range matches {
		if k >= 5 {
			break
		}
		fmt.Fprintf(&b, "\n  %d: %s", m, truncateRunes(strings.TrimRight(lines[m-1], "\r\n"), 120))
	}
	return 0, b.String()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// insertSteering is what a rejected re-typing edit_file is told to do
// instead (R11 S2): the line the new content goes after, as an
// insert_after call, and the lines the edit really changes, to quote alone
// as old_string. It applies the edit at each occurrence of old_string (the
// last of equally small changes, which is where appending usually goes)
// and diffs the result against the file: the common leading lines are
// what the new content follows; the original lines between the common
// head and tail are the ones it changes. "" when old_string is absent.
func insertSteering(path, content, oldString, newString string) string {
	occ := occurrences(content, oldString)
	if len(occ) == 0 {
		return ""
	}
	if len(occ) > 50 {
		occ = occ[len(occ)-50:]
	}
	orig := splitKeep(content)
	bestN, bestA, bestB, bestChanged, bestIdx := 0, 0, 0, -1, 0
	for _, idx := range occ {
		edited := splitKeep(content[:idx] + newString + content[idx+len(oldString):])
		p := 0
		for p < len(orig) && p < len(edited) && sameLine(orig[p], edited[p]) {
			p++
		}
		s := 0
		for s < min(len(orig), len(edited))-p && sameLine(orig[len(orig)-1-s], edited[len(edited)-1-s]) {
			s++
		}
		a, b := p+1, len(orig)-s // original lines the edit changes (empty when b < a)
		changed := max(b-a+1, 0)
		if bestChanged < 0 || changed <= bestChanged {
			bestN, bestA, bestB, bestChanged, bestIdx = p, a, b, changed, idx
		}
	}
	if bestChanged == 0 {
		// A pure insertion: old_string's own lines are the ones to quote
		// if something there must change.
		bestA = lineAt(content, bestIdx)
		bestB = bestA + strings.Count(strings.TrimSuffix(oldString, "\n"), "\n")
	}
	span := fmt.Sprintf("lines %d-%d", bestA, bestB)
	if bestA == bestB {
		span = fmt.Sprintf("line %d", bestA)
	}
	return fmt.Sprintf("Instead: to add new content after line %d use insert_after(path=\"%s\", anchor_line=%d, text=…) with only the new lines; to change %s, quote only those lines as old_string.", bestN, filepath.ToSlash(path), bestN, span)
}

// matchLineEndings gives newString the line ending of the line old_string
// starts on (offset idx in content) when that line ends in "\r\n": a model
// writes bare "\n", and a CRLF file must not get mixed endings (R11 review).
func matchLineEndings(content string, idx int, newString string) string {
	end := strings.IndexByte(content[idx:], '\n')
	if end <= 0 || content[idx+end-1] != '\r' || !strings.Contains(newString, "\n") {
		return newString
	}
	return strings.ReplaceAll(strings.ReplaceAll(newString, "\r\n", "\n"), "\n", "\r\n")
}

// splitKeep splits s into lines (without their newlines); a final newline
// adds no empty line.
func splitKeep(s string) []string {
	out := strings.Split(s, "\n")
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}

func sameLine(a, b string) bool { return strings.TrimRight(a, "\r") == strings.TrimRight(b, "\r") }
