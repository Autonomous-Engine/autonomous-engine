package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/google/uuid"
)

// LoopOptions are the opt-in behaviours of RunToolLoopWithOptions. The zero
// value is RunToolLoop's behaviour.
type LoopOptions struct {
	// EvidenceLedger compacts the transcript every iteration: tool results
	// older than the last KeepRecent iterations leave the transcript and
	// become one line each in a single user-role "Evidence ledger"
	// message right after the first user message (user, not system: the
	// adapters fold system messages into the leading system block, where a
	// changing ledger would defeat prefix/KV caching of system + tools +
	// PRD); older assistant turns are
	// trimmed to their first 300 chars. Microcompact/checkpoint remain the
	// backstop. Research sets it; the implementation agent sets it with
	// PinReads.
	EvidenceLedger bool
	// KeepRecent is how many of the latest tool-call iterations stay in
	// full. 0 means TOOL_LOOP_KEEP_RECENT, default 2.
	KeepRecent int
	// DeadToolGuard drops a tool from the offered list once it has
	// returned a "not ready / unavailable" class error twice in the loop,
	// and records that in the ledger. Research intents only.
	DeadToolGuard bool
	// RefreshTools, when set, is called before every LLM call and its
	// result replaces the offered tools (tools stripped by the loop stay
	// stripped). Research uses it so LSP tools appear once the language
	// server has warmed up.
	RefreshTools func() []adapter.Tool
	// PinReads (with EvidenceLedger) keeps the latest read of a file that
	// was edited during the loop, or that is in PinnedFiles, in full: once
	// the read leaves the transcript, the ledger message carries the read
	// range's CURRENT content (re-read from the executor's tree, so it
	// shows the agent's own edits) until a newer read of the same file
	// replaces it. The implementation agent sets it: an edit needs the
	// file's current text, which a one-line ledger entry does not hold.
	PinReads bool
	// PinnedFiles are repository paths pinned once read (the task's files
	// in scope).
	PinnedFiles []string
	// DedupReads (R12 X2, with EvidenceLedger) answers a read of lines the
	// model still has — in a result that stays in the transcript it sees
	// next, or in a pinned read — and that are unchanged since, with one
	// line instead of the content. The implementation agent sets it.
	DedupReads bool
	// StuckControl (R10 W3) fingerprints every file-tool call (tool,
	// canonical arguments, the target file's sha) over the whole loop. The
	// second identical failing call gets the first failure appended; the
	// third — or a fifth failed edit of one path, or a final INSUFFICIENT
	// answer — ends the loop with a *LoopStuck error. The implementation
	// agent sets it.
	//
	// R17: StuckControl also watches whole tool turns. When the last 9
	// turns are at most 3 distinct turns (tool + canonical arguments + file
	// sha) each repeated 3+ times, the loop adds one warning naming the
	// cycle; 3 more turns of it, or a second cycle later, end the loop with
	// LoopStuck{Kind: cycle}. The sliding-window detector ends the loop
	// (Kind sliding_window) when it fires again after its warning and no
	// turn since made a call the loop had not made before.
	StuckControl bool
	// ReadOnly (R17, with StuckControl) says the loop's tree does not
	// change — research reads a pinned snapshot — so an identical read
	// returns exactly what the model was given before. The first repeat of
	// a read keeps its content with a note, the second gets a one-line
	// answer (with DedupReads), and the third ends the loop with
	// LoopStuck{Kind: repeated_read}. Research sets it.
	ReadOnly bool
	// EvidenceNudge (R18 E1), when set, refuses an answer given before
	// any tool call has run: the first such answer gets this text as a
	// user message, a second ends the loop with *NoEvidence (the answer
	// kept as its Note). Research sets it.
	EvidenceNudge string
	// PriorStuck are an earlier pass's LoopStuck failures (R10 W4): the
	// first identical failure of one of their calls ends this loop.
	PriorStuck []LoopStuck
	// CheckpointStore stores checkpoint snapshots when the loop has no
	// database (tests and offline replays); with a database the snapshots
	// go to conversation_snapshots.
	CheckpointStore func(ctx context.Context, snapshotID, workItemID uuid.UUID, agentType string, seq int, messagesJSON []byte, summary string, turns int) error
}

// pinnedRead is the latest compacted read of one file.
type pinnedRead struct {
	path       string
	start, end int
	body       string
	seq        int
}

const (
	pinnedHeader   = "## Current file contents"
	pinnedMaxChars = 5000 // newest pins first; older ones fall back to their ledger line
	pinnedMaxLines = 400  // per pin, as read_file's default
)

const (
	ledgerHeader          = "## Evidence ledger"
	defaultLedgerKeep     = 2
	ledgerArgsMax         = 80
	ledgerSummaryMax      = 160
	ledgerAssistantKeep   = 300
	ledgerHeaderParagraph = ledgerHeader + "\n" +
		"Earlier tool calls and what they returned; the full results were removed to save context. " +
		"Do not repeat a call listed here: use its result.\n"
)

// keepRecentFromEnv is the KeepRecent default: TOOL_LOOP_KEEP_RECENT or 2.
func keepRecentFromEnv() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOOL_LOOP_KEEP_RECENT"))); err == nil && v >= 1 {
		return v
	}
	return defaultLedgerKeep
}

// EstimatePromptTokens is the chars/4 estimate of one request's input: every
// message's content and tool-call arguments plus the tool schemas as JSON.
func EstimatePromptTokens(req adapter.CompletionRequest) int {
	chars := 0
	for _, m := range req.Messages {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
	}
	if len(req.Tools) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			chars += len(b)
		}
	}
	return chars / 4
}

// evidenceLedger holds one line per distinct tool call whose result has
// left the transcript, in first-seen order.
type evidenceLedger struct {
	lines    []string
	byKey    map[string]int
	repeated map[int]bool
	// pending holds the line for each executed call until its result leaves
	// the transcript. It is keyed by the call AND its result (pendingKey),
	// never by the call ID alone: providers such as Ollama restart their
	// IDs (call_0, call_1 …) in every response, so an ID recurs across
	// iterations. Two entries can only share a key when the call and the
	// result are identical, in which case their lines are identical too.
	pending map[string]*ledgerPending

	// Pinned reads (LoopOptions.PinReads).
	pinReads bool
	scope    map[string]bool        // PinnedFiles, normalised
	edited   map[string]bool        // files edited in this loop
	lastRead map[string]*pinnedRead // latest compacted read per file
	readSeq  int
	// reader returns a file's current lines start..end; nil keeps the
	// content the read returned.
	reader func(path string, start, end int) (string, bool)
	// rendered are the pins the latest ledger message showed (R12 X2).
	rendered []pinnedRange
	// lastEdit is the first line of each file's latest applied edit, and
	// seedOf a file's Where-to-edit line: a pin trimmed to the budget
	// stays centred on the one or the other (R12 review).
	lastEdit map[string]int
	seedOf   func(path string) int
}

// pinnedRange is one pin a ledger message showed: lines start..end of path
// as they were then.
type pinnedRange struct {
	path       string
	start, end int
	body       string
}

// enablePins turns on pinned reads for scope.
func (l *evidenceLedger) enablePins(scope []string, reader func(path string, start, end int) (string, bool)) {
	l.pinReads = true
	l.reader = reader
	l.scope = map[string]bool{}
	l.edited = map[string]bool{}
	l.lastRead = map[string]*pinnedRead{}
	l.lastEdit = map[string]int{}
	for _, p := range scope {
		if n := pinPath(p); n != "" {
			l.scope[n] = true
		}
	}
}

// pinPath normalises a repository path for pin bookkeeping.
func pinPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	return strings.TrimPrefix(p, "./")
}

func isReadTool(name string) bool { return name == "read_file" || name == "codex_read_file" }

// recordRead keeps a compacted read as the file's latest read.
func (l *evidenceLedger) recordRead(call adapter.ToolCall, content string) {
	if !l.pinReads || !isReadTool(call.Name) || resultLooksFailed(call.Name, content) || isRereadResult(content) {
		return
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	p, _ := args["path"].(string)
	p = pinPath(p)
	if p == "" {
		return
	}
	start, end := readSpan(args, content)
	_, hasStart := args["start_line"]
	_, hasEnd := args["end_line"]
	total := jsonInt(content, "total_lines")
	if !hasStart && !hasEnd && !readHintRE.MatchString(content) && (call.Name == "read_file" || end >= total) {
		end = 0 // the whole file: a pin shows all of it as it is now, edits included
	}
	body := content
	if call.Name == "codex_read_file" {
		if b := jsonField(content, "content"); b != "" {
			body = b
		}
	}
	l.readSeq++
	l.lastRead[p] = &pinnedRead{path: p, start: start, end: end, body: body, seq: l.readSeq}
}

// pinnedSection renders the current content of every pinned read whose
// file is not read again in the kept transcript (inTail), newest first
// within pinnedMaxChars, in read order.
func (l *evidenceLedger) pinnedSection(inTail map[string]bool) string {
	if !l.pinReads {
		return ""
	}
	l.rendered = nil
	var pins []*pinnedRead
	for p, r := range l.lastRead {
		if (l.scope[p] || l.edited[p]) && !inTail[p] {
			pins = append(pins, r)
		}
	}
	if len(pins) == 0 {
		return ""
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].seq > pins[j].seq })
	var blocks []string
	budget := pinnedMaxChars
	omitted := 0
	for k, r := range pins {
		body := r.body
		if l.reader != nil {
			if cur, ok := l.reader(r.path, r.start, r.end); ok {
				body = cur
			} else {
				body = "(the file no longer exists)"
			}
		}
		body = strings.TrimRight(body, "\n")
		lines := strings.Split(body, "\n")
		if len(lines) > pinnedMaxLines {
			lines = lines[:pinnedMaxLines]
			body = strings.Join(lines, "\n") + fmt.Sprintf("\n[… more lines: read from line %d to see them]", r.start+pinnedMaxLines)
		}
		start := r.start
		label := fmt.Sprintf("### %s (lines %d-%d)", r.path, start, start+len(lines)-1)
		block := label + "\n" + body
		if len(block) > budget && k == 0 {
			// The newest pin is trimmed to the budget around the latest
			// edit or the Where-to-edit line, not dropped (R12 review: a
			// default window of a long-line page bigger than the budget
			// was never pinned and was read again).
			start, lines, block = l.trimPin(r, lines, budget)
		}
		if len(block) > budget {
			// Older pins past the budget fall back to their ledger line.
			omitted++
			continue
		}
		budget -= len(block)
		blocks = append(blocks, block)
		l.rendered = append(l.rendered, pinnedRange{path: r.path, start: start, end: start + len(lines) - 1, body: strings.Join(lines, "\n")})
	}
	if len(blocks) == 0 {
		return ""
	}
	for i, j := 0, len(blocks)-1; i < j; i, j = i+1, j-1 {
		blocks[i], blocks[j] = blocks[j], blocks[i]
	}
	note := ""
	if omitted > 0 {
		note = fmt.Sprintf(" %d older read(s) are only in the ledger: read again before editing them.", omitted)
	}
	return pinnedHeader + "\n" +
		"Your latest reads of files you edited or that are in scope, as they are on disk now; copy edit_file's old_string from here." + note + "\n\n" +
		strings.Join(blocks, "\n\n")
}

// trimPin narrows a pin of lines (from line r.start) to fit budget: from
// the file's latest edit line, else its Where-to-edit line, else the
// pin's middle, a line after and a line before in turn. It returns the
// first line kept, the lines kept and the block.
func (l *evidenceLedger) trimPin(r *pinnedRead, lines []string, budget int) (int, []string, string) {
	centre := r.start + len(lines)/2
	if n := l.lastEdit[r.path]; n > 0 {
		centre = n
	} else if l.seedOf != nil {
		if n := l.seedOf(r.path); n > 0 {
			centre = n
		}
	}
	c := min(max(centre-r.start, 0), len(lines)-1)
	block := func(a, b int) string {
		s, e := r.start+a, r.start+b
		return fmt.Sprintf("### %s (lines %d-%d)\n%s\n[pin trimmed to lines %d-%d]", r.path, s, e, strings.Join(lines[a:b+1], "\n"), s, e)
	}
	a, b := c, c
	for grew := true; grew; {
		grew = false
		if b+1 < len(lines) && len(block(a, b+1)) <= budget {
			b++
			grew = true
		}
		if a > 0 && len(block(a-1, b)) <= budget {
			a--
			grew = true
		}
	}
	return r.start + a, lines[a : b+1], block(a, b)
}

// touchPin makes path's pin the newest (R12 review): a short "unchanged"
// answer relies on it, so newer pins must not evict it next.
func (l *evidenceLedger) touchPin(path string) {
	if r := l.lastRead[path]; r != nil {
		l.readSeq++
		r.seq = l.readSeq
	}
}

// readSpan is the line range a read covered: its arguments, else the
// result's own start/end (codex_read_file), else the whole result.
func readSpan(args map[string]any, content string) (int, int) {
	num := func(k string) int { f, _ := args[k].(float64); return int(f) }
	start, end := num("start_line"), num("end_line")
	if js, je := jsonInt(content, "start_line"), jsonInt(content, "end_line"); js > 0 {
		start, end = js, je
	}
	if m := readHintRE.FindStringSubmatch(content); m != nil {
		start, _ = strconv.Atoi(m[1])
		end, _ = strconv.Atoi(m[2])
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = start + len(strings.Split(strings.TrimRight(content, "\n"), "\n")) - 1
	}
	return start, end
}

// readHintRE matches read_file's continuation hint "[lines A-B of N …".
var readHintRE = regexp.MustCompile(`\[lines (\d+)-(\d+) of \d+`)

type ledgerPending struct {
	line string
	n    int // executions not yet compacted
}

// pendingKey identifies one executed call by what was called and what
// came back.
func pendingKey(name, args, id, content string) string {
	return name + "\x00" + canonicalArgs(args) + "\x00" + id + "\x00" + content
}

func newEvidenceLedger() *evidenceLedger {
	return &evidenceLedger{
		byKey:    map[string]int{},
		repeated: map[int]bool{},
		pending:  map[string]*ledgerPending{},
	}
}

// note records the line for a call that just ran (the error flag is only
// known here). It enters the ledger when the call's result leaves the
// transcript.
func (l *evidenceLedger) note(call adapter.ToolCall, res adapter.ToolResult) {
	if l.pinReads && IsFileEditTool(call.Name) && !res.IsError {
		var args map[string]any
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		if p, _ := args["path"].(string); pinPath(p) != "" {
			l.edited[pinPath(p)] = true
			if m := appliedLinesRE.FindStringSubmatch(res.Content); m != nil {
				n, _ := strconv.Atoi(m[1])
				l.lastEdit[pinPath(p)] = n
			}
		}
	}
	k := pendingKey(call.Name, call.Arguments, call.ID, res.Content)
	if p, ok := l.pending[k]; ok {
		p.n++
		return
	}
	l.pending[k] = &ledgerPending{line: ledgerLine(call.Name, call.Arguments, res.Content, res.IsError), n: 1}
}

// absorb takes in a span a checkpoint removes (R10 W6): each tool result
// becomes its ledger line and each read is recorded for pinning, exactly as
// compactTranscript does for results that age out — so the newest read of
// every in-scope file survives in "Current file contents".
func (l *evidenceLedger) absorb(span []adapter.ChatMessage) {
	var groupCalls map[string]adapter.ToolCall
	for _, m := range span {
		switch m.Role {
		case adapter.RoleAssistant:
			groupCalls = make(map[string]adapter.ToolCall, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				groupCalls[tc.ID] = tc
			}
		case adapter.RoleTool:
			call, ok := groupCalls[m.ToolCallID]
			if !ok {
				call = adapter.ToolCall{ID: m.ToolCallID, Name: m.Name}
			}
			l.commit(call, m)
			l.recordRead(call, m.Content)
		}
	}
}

// commit moves a compacted tool message into the ledger. call is the tool
// call this message answers, taken from the assistant turn the message
// follows.
func (l *evidenceLedger) commit(call adapter.ToolCall, msg adapter.ChatMessage) {
	k := pendingKey(call.Name, call.Arguments, msg.ToolCallID, msg.Content)
	line := ""
	if p, ok := l.pending[k]; ok {
		line = p.line
		if p.n--; p.n <= 0 {
			delete(l.pending, k)
		}
	} else {
		line = ledgerLine(call.Name, call.Arguments, msg.Content, false)
	}
	key := call.Name + "\x00" + canonicalArgs(call.Arguments)
	if isReadTool(call.Name) && isRereadResult(msg.Content) {
		key += "\x00re-read" // its own line, not "(repeated)" on the read's
	}
	l.add(key, line)
}

func (l *evidenceLedger) add(key, line string) {
	if i, ok := l.byKey[key]; ok {
		if !l.repeated[i] {
			l.lines[i] += " (repeated)"
			l.repeated[i] = true
		}
		return
	}
	l.byKey[key] = len(l.lines)
	l.lines = append(l.lines, line)
}

// addNote records a loop event (a dropped tool) as its own ledger line.
func (l *evidenceLedger) addNote(line string) { l.add("note\x00"+line, line) }

func (l *evidenceLedger) message(pinned string) string {
	msg := ledgerHeaderParagraph + strings.Join(l.lines, "\n")
	if pinned != "" {
		msg += "\n\n" + pinned
	}
	return msg
}

func isLedgerMessage(m adapter.ChatMessage) bool {
	return m.Role == adapter.RoleUser && strings.HasPrefix(m.Content, ledgerHeader)
}

// compactTranscript applies the ledger pass to msgs: tool results of every
// tool-call iteration but the last keepRecent move into the ledger, the
// assistant turns before them lose their tool calls and are trimmed, and
// the ledger message is (re)placed right after the first user message.
// User and system messages are kept as they are.
func compactTranscript(msgs []adapter.ChatMessage, ledger *evidenceLedger, keepRecent int) []adapter.ChatMessage {
	if keepRecent < 1 {
		keepRecent = 1
	}
	base := make([]adapter.ChatMessage, 0, len(msgs)+1)
	for _, m := range msgs {
		if !isLedgerMessage(m) {
			base = append(base, m)
		}
	}
	firstUser := -1
	for i, m := range base {
		if m.Role == adapter.RoleUser {
			firstUser = i
			break
		}
	}
	if firstUser < 0 {
		return msgs
	}
	var groups []int // assistant messages carrying tool calls
	for i := firstUser + 1; i < len(base); i++ {
		if base[i].Role == adapter.RoleAssistant && len(base[i].ToolCalls) > 0 {
			groups = append(groups, i)
		}
	}
	cutoff := firstUser + 1
	if len(groups) > keepRecent {
		cutoff = groups[len(groups)-keepRecent]
	}

	out := make([]adapter.ChatMessage, 0, len(base)+1)
	out = append(out, base[:firstUser+1]...)
	ledgerAt := len(out)
	// groupCalls are the tool calls of the assistant turn the following
	// tool messages answer; IDs are unique within one turn only.
	var groupCalls map[string]adapter.ToolCall
	for i := firstUser + 1; i < cutoff; i++ {
		m := base[i]
		switch m.Role {
		case adapter.RoleTool:
			call, ok := groupCalls[m.ToolCallID]
			if !ok {
				call = adapter.ToolCall{ID: m.ToolCallID, Name: m.Name}
			}
			ledger.commit(call, m)
			ledger.recordRead(call, m.Content)
		case adapter.RoleAssistant:
			groupCalls = make(map[string]adapter.ToolCall, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				groupCalls[tc.ID] = tc
			}
			m.ToolCalls = nil
			m.Content = truncateRunes(strings.TrimSpace(m.Content), ledgerAssistantKeep)
			if m.Content != "" {
				out = append(out, m)
			}
		default:
			out = append(out, m)
		}
	}
	out = append(out, base[cutoff:]...)
	if len(ledger.lines) == 0 {
		return out
	}
	// Files read again in the kept transcript are not pinned twice. A
	// short "unchanged" answer (R12 X2) is not a read of the content: the
	// pin stays.
	inTail := map[string]bool{}
	var tailCalls map[string]adapter.ToolCall
	for _, m := range base[cutoff:] {
		switch m.Role {
		case adapter.RoleAssistant:
			tailCalls = make(map[string]adapter.ToolCall, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tailCalls[tc.ID] = tc
			}
		case adapter.RoleTool:
			tc, ok := tailCalls[m.ToolCallID]
			if !ok || !isReadTool(tc.Name) || isRereadResult(m.Content) {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Arguments), &args)
			if p, _ := args["path"].(string); p != "" {
				inTail[pinPath(p)] = true
			}
		}
	}
	withLedger := make([]adapter.ChatMessage, 0, len(out)+1)
	withLedger = append(withLedger, out[:ledgerAt]...)
	withLedger = append(withLedger, adapter.ChatMessage{Role: adapter.RoleUser, Content: ledger.message(ledger.pinnedSection(inTail))})
	withLedger = append(withLedger, out[ledgerAt:]...)
	return withLedger
}

// canonicalArgs re-marshals a JSON argument object so key order and spacing
// do not make two identical calls look distinct.
func canonicalArgs(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return strings.TrimSpace(args)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(args)
	}
	return string(b)
}

// ledgerLine renders "- tool(args) -> summary" with the args at most 80
// chars and the summary at most 160.
func ledgerLine(tool, args, content string, isError bool) string {
	if IsFileEditTool(tool) {
		return editLedgerLine(tool, args, content, isError)
	}
	a := canonicalArgs(args)
	a = strings.TrimSuffix(strings.TrimPrefix(a, "{"), "}")
	return fmt.Sprintf("- %s(%s) -> %s", tool, truncateRunes(a, ledgerArgsMax), summarizeToolResult(tool, content, isError))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// summarizeToolResult is the one-line, tool-aware summary of a result.
func summarizeToolResult(tool, content string, isError bool) string {
	return truncateRunes(summarize(tool, content, isError), ledgerSummaryMax)
}

func summarize(tool, content string, isError bool) string {
	content = strings.TrimSpace(content)
	if isError {
		return "error: " + errorClass(content)
	}
	if content == "" {
		return "0 results"
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal([]byte(content), &obj)
	switch tool {
	case "run_command":
		// "exit 1: × the failing test" — an old test log is one line.
		code := 0
		if m := exitStatusRE.FindStringSubmatch(content); m != nil {
			code, _ = strconv.Atoi(m[1])
		} else if strings.Contains(content, "[exit code:") {
			return "did not finish: " + lastErrorLine(content)
		}
		if code == 0 {
			return "exit 0"
		}
		return fmt.Sprintf("exit %d: %s", code, lastErrorLine(content))
	case "codex_find_module":
		var mods []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(obj["modules"], &mods)
		if len(mods) == 0 {
			return "0 results"
		}
		names := make([]string, 0, len(mods))
		for _, m := range mods {
			names = append(names, m.Name)
		}
		return fmt.Sprintf("%d modules: %s", len(mods), strings.Join(names, ", "))
	case "codex_grep", "search_code":
		var hits []struct {
			File string `json:"file"`
			Line int    `json:"line"`
		}
		if obj != nil {
			_ = json.Unmarshal(obj["hits"], &hits)
			if len(hits) == 0 {
				return "0 results"
			}
			first := make([]string, 0, 3)
			for i := 0; i < len(hits) && i < 3; i++ {
				first = append(first, fmt.Sprintf("%s:%d", hits[i].File, hits[i].Line))
			}
			total := len(hits)
			var t int
			if json.Unmarshal(obj["total_hits"], &t) == nil && t > total {
				total = t
			}
			return fmt.Sprintf("%d hits: %s", total, strings.Join(first, ", "))
		}
		lines := nonEmptyLines(content)
		return fmt.Sprintf("%d lines: %s", len(lines), firstN(lines, 3))
	case "codex_read_file", "read_file":
		if isRereadResult(content) {
			return "re-read, unchanged"
		}
		var path, body string
		var total, start, end int
		_ = json.Unmarshal(obj["path"], &path)
		_ = json.Unmarshal(obj["content"], &body)
		_ = json.Unmarshal(obj["total_lines"], &total)
		_ = json.Unmarshal(obj["start_line"], &start)
		_ = json.Unmarshal(obj["end_line"], &end)
		if obj == nil {
			body = content
			total = len(strings.Split(content, "\n"))
		}
		s := fmt.Sprintf("%d lines", total)
		if path != "" {
			s = fmt.Sprintf("%s L%d-%d of %d lines", path, start, end, total)
		}
		if h := firstHeadingOrSymbol(body); h != "" {
			s += "; " + h
		}
		return s
	case "codex_list_tree", "list_files":
		var tree string
		_ = json.Unmarshal(obj["tree"], &tree)
		if obj == nil {
			tree = content
		}
		top := topTreeEntries(tree)
		if len(top) == 0 {
			return "0 results"
		}
		return fmt.Sprintf("%d entries: %s", len(top), strings.Join(top, ", "))
	case "codex_similar_code":
		var res []struct {
			File string `json:"file"`
		}
		_ = json.Unmarshal(obj["results"], &res)
		if len(res) == 0 {
			return "0 results"
		}
		files := make([]string, 0, 3)
		for i := 0; i < len(res) && i < 3; i++ {
			files = append(files, res[i].File)
		}
		return fmt.Sprintf("%d matches: %s", len(res), strings.Join(files, ", "))
	case "codex_dependents_of", "codex_dependencies_of":
		var list []string
		if json.Unmarshal(obj["dependents"], &list) != nil || list == nil {
			_ = json.Unmarshal(obj["dependencies"], &list)
		}
		if len(list) == 0 {
			if _, notFound := obj["error"]; notFound {
				return "module not found"
			}
			return "0 results"
		}
		return fmt.Sprintf("%d: %s", len(list), strings.Join(list, ", "))
	}
	// Generic: count the first array in a JSON object, else the first line.
	for _, k := range []string{"patterns", "results", "items", "edges"} {
		var arr []json.RawMessage
		if json.Unmarshal(obj[k], &arr) == nil && obj[k] != nil {
			if len(arr) == 0 {
				return "0 results"
			}
			return fmt.Sprintf("%d %s", len(arr), k)
		}
	}
	lines := nonEmptyLines(content)
	if len(lines) == 0 {
		return "0 results"
	}
	return fmt.Sprintf("%d lines: %s", len(lines), lines[0])
}

// errorClass names the kind of a tool error in a few words.
func errorClass(content string) string {
	lc := strings.ToLower(content)
	switch {
	case strings.Contains(lc, "503"), strings.Contains(lc, "temporarily unavailable"),
		strings.Contains(lc, "service unavailable"), strings.Contains(lc, "try again"), strings.Contains(lc, "retry later"):
		// A transient outage, not a tool that cannot answer here.
		return "transient"
	case strings.Contains(lc, "not ready"):
		return "not ready"
	case strings.Contains(lc, "unavailable"), strings.Contains(lc, "not available"), strings.Contains(lc, "not configured"):
		return "unavailable"
	case strings.Contains(lc, "no such file"), strings.Contains(lc, "not found"), strings.Contains(lc, "does not exist"):
		return "not found"
	case strings.Contains(lc, "deadline exceeded"), strings.Contains(lc, "timeout"), strings.Contains(lc, "timed out"):
		return "timeout"
	case strings.Contains(lc, "invalid argument"), strings.Contains(lc, "is required"):
		return "invalid arguments"
	}
	lines := nonEmptyLines(content)
	if len(lines) == 0 {
		return "unknown"
	}
	return truncateRunes(lines[0], 100)
}

// isDeadToolError reports a "this tool cannot answer here" error: retrying
// it later in the same task will not help.
func isDeadToolError(content string) bool {
	switch errorClass(content) {
	case "not ready", "unavailable":
		return true
	}
	return false
}

var symbolLine = regexp.MustCompile(`^\s*(export\s+)?(default\s+)?(async\s+)?(func|function|class|interface|type|const|let|var|def|struct|enum|module|package|public|private|protected|model|CREATE)\b`)

// firstHeadingOrSymbol is a file's first markdown heading or declaration.
func firstHeadingOrSymbol(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "#!") && !strings.HasPrefix(t, "#include") {
			return truncateRunes(t, 70)
		}
		if symbolLine.MatchString(t) {
			return truncateRunes(t, 70)
		}
	}
	return ""
}

// topTreeEntries returns the first-level entries of an indented tree.
func topTreeEntries(tree string) []string {
	var out []string
	for i, ln := range strings.Split(tree, "\n") {
		if i == 0 && !strings.HasPrefix(ln, " ") && strings.HasSuffix(strings.TrimSpace(ln), "/") {
			continue // the root label
		}
		if ln == "" {
			continue
		}
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		if indent <= 2 {
			out = append(out, strings.TrimSpace(ln))
		}
	}
	return out
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func firstN(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
