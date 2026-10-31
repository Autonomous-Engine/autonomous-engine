package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// checkpointHeader opens every checkpoint summary message.
const checkpointHeader = "## Conversation checkpoint"

// Section titles of a checkpoint summary, in order. A later checkpoint
// reads them back from the summary it removes, so they must stay stable.
var checkpointSectionTitles = []string{"Files read", "Files edited", "Commands run", "Searches", "Decisions"}

// checkpointSectionCap bounds each section (newest entries kept) so a long
// run of checkpoints cannot grow the summary without bound.
var checkpointSectionCap = map[string]int{
	"Files read": 20, "Files edited": 20, "Commands run": 12, "Searches": 15, "Decisions": 5,
}

// checkpointSections accumulates the summary's lines per section, in
// first-seen order, without duplicates.
type checkpointSections struct {
	lines map[string][]string
	seen  map[string]map[string]bool
	// readRanges keeps one "Files read" line per path with every range.
	readRanges map[string][]string
	readOrder  []string
}

func newCheckpointSections() *checkpointSections {
	s := &checkpointSections{lines: map[string][]string{}, seen: map[string]map[string]bool{}, readRanges: map[string][]string{}}
	for _, t := range checkpointSectionTitles {
		s.seen[t] = map[string]bool{}
	}
	return s
}

func (s *checkpointSections) add(section, line string) {
	line = strings.TrimSpace(line)
	if line == "" || s.seen[section][line] {
		return
	}
	s.seen[section][line] = true
	s.lines[section] = append(s.lines[section], line)
}

func (s *checkpointSections) addRead(path, rng string) {
	if path == "" {
		return
	}
	if _, ok := s.readRanges[path]; !ok {
		s.readOrder = append(s.readOrder, path)
	}
	for _, r := range s.readRanges[path] {
		if r == rng {
			return
		}
	}
	s.readRanges[path] = append(s.readRanges[path], rng)
}

// buildCheckpointSummary summarises the messages a checkpoint removes (never
// the pinned task or the kept tail) as five sections built from the tool
// calls' arguments and their results: files read (path and line ranges),
// files edited (path and one line), commands run (command, exit code, last
// error line), searches (query and top hits) and decisions (the model's own
// prose). A previous checkpoint summary inside the removed span is folded
// in, so nothing summarised earlier is lost. recallOffered adds the
// recall_context hint only when that tool is offered.
func buildCheckpointSummary(removed []adapter.ChatMessage, snapshotID uuid.UUID, recallOffered bool) string {
	s := newCheckpointSections()
	results := 0
	var groupCalls map[string]adapter.ToolCall
	for _, m := range removed {
		switch m.Role {
		case adapter.RoleAssistant:
			groupCalls = make(map[string]adapter.ToolCall, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				groupCalls[tc.ID] = tc
			}
			if d := decisionLine(m.Content); d != "" {
				s.add("Decisions", d)
			}
		case adapter.RoleTool:
			results++
			call, ok := groupCalls[m.ToolCallID]
			if !ok {
				call = adapter.ToolCall{ID: m.ToolCallID, Name: m.Name}
			}
			if call.Name == "" {
				call.Name = m.Name
			}
			s.addResult(call, m.Content)
		case adapter.RoleUser:
			if isCheckpointSummary(m) {
				s.merge(m.Content)
			}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (ID: %s)\n", checkpointHeader, snapshotID)
	fmt.Fprintf(&sb, "Earlier turns (%d tool results) were removed to save context. The task above is unchanged; this is what was done so far.\n", results)
	for _, title := range checkpointSectionTitles {
		lines := s.lines[title]
		if title == "Files read" {
			lines = nil
			for _, p := range s.readOrder {
				lines = append(lines, p+" "+strings.Join(s.readRanges[p], ", "))
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "\n### %s\n", title)
		if c := checkpointSectionCap[title]; len(lines) > c {
			fmt.Fprintf(&sb, "- (%d earlier entries omitted)\n", len(lines)-c)
			lines = lines[len(lines)-c:]
		}
		for _, ln := range lines {
			sb.WriteString("- ")
			sb.WriteString(ln)
			sb.WriteString("\n")
		}
	}
	if recallOffered {
		fmt.Fprintf(&sb, "\nFull earlier results: recall_context(%q).\n", snapshotID.String())
	}
	return sb.String()
}

// merge folds the sections of an earlier checkpoint summary in.
func (s *checkpointSections) merge(summary string) {
	section := ""
	for _, ln := range strings.Split(strings.ReplaceAll(summary, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(ln, "### ") {
			section = strings.TrimSpace(strings.TrimPrefix(ln, "### "))
			continue
		}
		if section == "" || !strings.HasPrefix(ln, "- ") {
			continue
		}
		body := strings.TrimPrefix(ln, "- ")
		if strings.HasPrefix(body, "(") && strings.HasSuffix(body, "entries omitted)") {
			continue
		}
		if section == "Files read" {
			path, rng, _ := strings.Cut(body, " ")
			for _, r := range strings.Split(rng, ", ") {
				s.addRead(path, strings.TrimSpace(r))
			}
			continue
		}
		if _, known := s.seen[section]; known {
			s.add(section, body)
		}
	}
}

// addResult files one tool call and its result under its section.
func (s *checkpointSections) addResult(call adapter.ToolCall, content string) {
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	str := func(k string) string { v, _ := args[k].(string); return strings.TrimSpace(v) }
	failed := resultLooksFailed(call.Name, content)
	switch call.Name {
	case "read_file", "codex_read_file":
		path := str("path")
		if failed {
			if path != "" {
				s.add("Searches", fmt.Sprintf("%s %s → %s", call.Name, path, "error: "+errorClass(content)))
			}
			return
		}
		if path == "" {
			path = jsonField(content, "path")
		}
		s.addRead(path, readRange(args, content))
	case "edit_file", "write_file", "insert_after", "append_to_file":
		path := str("path")
		if path == "" {
			return
		}
		var what string
		switch {
		case failed:
			what = "rejected: " + errorClass(content)
		case call.Name == "write_file":
			what = fmt.Sprintf("wrote %d lines", len(strings.Split(str("content"), "\n")))
		case call.Name == "insert_after" || call.Name == "append_to_file":
			what = "insert: " + truncateRunes(firstNonEmptyLine(str("text")), 80)
		default:
			what = "edit: " + truncateRunes(firstNonEmptyLine(str("new_string")), 80)
		}
		s.add("Files edited", path+" — "+what)
	case "run_command":
		s.add("Commands run", commandLine(str("command"), content))
	default:
		q := firstNonEmpty(str("pattern"), str("name_or_pattern"), str("description"), str("query"),
			str("symbol"), str("module_name"), str("category_or_keyword"), str("path"), str("contract_name"))
		s.add("Searches", fmt.Sprintf("%s %q → %s", call.Name, truncateRunes(q, 60), summarizeToolResult(call.Name, content, failed)))
	}
}

var exitStatusRE = regexp.MustCompile(`\[exit code: (?:exit status )?(-?\d+)`)

// commandLine is "`cmd` → exit N: last error line".
func commandLine(command, content string) string {
	code := 0
	if m := exitStatusRE.FindStringSubmatch(content); m != nil {
		code, _ = strconv.Atoi(m[1])
	} else if strings.Contains(content, "[exit code:") {
		code = -1
	} else if strings.HasPrefix(content, "REJECTED") {
		return fmt.Sprintf("`%s` → rejected: %s", truncateRunes(command, 80), errorClass(content))
	}
	line := fmt.Sprintf("`%s` → exit %d", truncateRunes(command, 80), code)
	if code == -1 {
		line = fmt.Sprintf("`%s` → did not finish", truncateRunes(command, 80))
	}
	if code != 0 {
		if e := lastErrorLine(content); e != "" {
			line += ": " + truncateRunes(e, 120)
		}
	}
	return line
}

var (
	errorLineRE = regexp.MustCompile(`(?i)(error|panic|exception|assert|✗|×)`)
	failLineRE  = regexp.MustCompile(`(?i)fail`)
)

// lastErrorLine is the last line of a command's output that names an error
// (an assertion or exception beats a "N failed" total), else its last
// non-empty line (the exit-code marker and the filter note excluded).
func lastErrorLine(content string) string {
	var lines []string
	for _, ln := range nonEmptyLines(content) {
		if !strings.HasPrefix(ln, "[exit code:") && !strings.HasPrefix(ln, "(filtered ") {
			lines = append(lines, ln)
		}
	}
	for _, re := range []*regexp.Regexp{errorLineRE, failLineRE} {
		for i := len(lines) - 1; i >= 0; i-- {
			if re.MatchString(lines[i]) {
				return lines[i]
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// readRange is the line range a read covered: from its arguments, else the
// result's own start/end (codex_read_file), else the whole result.
func readRange(args map[string]any, content string) string {
	start, end := readSpan(args, content)
	return fmt.Sprintf("L%d-%d", start, end)
}

// resultLooksFailed infers a tool error from the result text (tool messages
// do not carry the error flag).
func resultLooksFailed(tool, content string) bool {
	c := strings.TrimSpace(content)
	switch {
	case strings.HasPrefix(c, "REJECTED"), strings.HasPrefix(c, "Error"),
		strings.HasPrefix(c, tool+": "), strings.HasPrefix(c, "Tool \""):
		return true
	}
	return false
}

// decisionLine is the model's prose from an assistant turn, trimmed; empty
// for an empty turn or one that is raw tool-call JSON.
func decisionLine(content string) string {
	c := strings.TrimSpace(content)
	if c == "" || strings.HasPrefix(c, "{") || strings.HasPrefix(c, "[") || strings.HasPrefix(c, "```") {
		return ""
	}
	return truncateRunes(strings.Join(strings.Fields(c), " "), 200)
}

func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func jsonField(content, key string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &obj) != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal(obj[key], &v)
	return v
}

func jsonInt(content, key string) int {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &obj) != nil {
		return 0
	}
	var v int
	_ = json.Unmarshal(obj[key], &v)
	return v
}
