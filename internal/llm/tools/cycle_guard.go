package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// R17: a live run's impact-assessment research (attempt 14) read
// public/index.html 836-853 → landing/index.html 855 → src/server/index.ts
// 33-60 in a fixed rotation for 25 turns (176k tokens). The identical-call
// detector compares adjacent turns only, so a period-3 cycle never trips
// it; the sliding-window detector looks at tool names only, so it fires on
// eight different reads as readily as on a cycle, and it only warned.

// LoopStuck kinds (LoopStuck.Kind). The R10 exits leave Kind empty.
const (
	// StuckKindCycle: the same ≤3 tool turns repeated 3 times each in the
	// last 9 turns, and 3 more turns of them after one warning.
	StuckKindCycle = "cycle"
	// StuckKindRepeatedRead: a read of a read-only tree repeated a third
	// time (LoopOptions.ReadOnly).
	StuckKindRepeatedRead = "repeated_read"
	// StuckKindSlidingWindow: the sliding-window detector fired again after
	// its warning, and no turn since the warning made a new call.
	StuckKindSlidingWindow = "sliding_window"
)

const (
	cycleWindow       = 9 // turns
	cycleMaxDistinct  = 3 // distinct turn fingerprints in the window
	cycleMinRepeats   = 3 // each of them at least this often
	cycleAfterWarning = 3 // turns of the cycle after the warning end the loop
	// readRepeatLimit: the third repeat of an identical read of a
	// read-only tree ends the loop.
	readRepeatLimit = 3
)

// cycleGuard watches whole tool turns over the loop (StuckControl).
type cycleGuard struct {
	window []string          // the latest turn fingerprints, at most cycleWindow
	descs  map[string]string // turn fingerprint → what the turn did
	seen   map[string]bool   // every call fingerprint so far

	firstOf map[string]turnCall // turn fingerprint → its first call

	warned  bool            // the one cycle warning was given
	cycle   map[string]bool // the turns it named, while they keep repeating
	order   []string        // those turns, in the order they came
	repeats int             // turns of the named cycle since the warning

	// The sliding-window detector's state: its warning was given, and
	// whether a turn since made a call never made before.
	slidingWarned     bool
	novelSinceSliding bool
}

func newCycleGuard() *cycleGuard {
	return &cycleGuard{descs: map[string]string{}, seen: map[string]bool{}, firstOf: map[string]turnCall{}}
}

// turnCall is one call of a turn: its fingerprint (tool, canonical
// arguments, and for a file tool the file's sha) and a short description.
type turnCall struct {
	call adapter.ToolCall
	fp   string
	path string
	desc string
}

func (e *Executor) turnCalls(calls []adapter.ToolCall) []turnCall {
	out := make([]turnCall, 0, len(calls))
	for _, c := range calls {
		fp, p := e.callFingerprint(c)
		if fp == "" {
			fp = c.Name + "\x00" + canonicalArgs(c.Arguments)
		}
		out = append(out, turnCall{call: c, fp: fp, path: p, desc: describeCall(c)})
	}
	return out
}

// describeCall is "tool path a-b" for a file tool, else "tool(args)".
func describeCall(c adapter.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(c.Arguments), &args)
	if p, _ := args["path"].(string); strings.TrimSpace(p) != "" {
		s := c.Name + " " + pinPath(p)
		a, _ := args["start_line"].(float64)
		b, _ := args["end_line"].(float64)
		switch {
		case a > 0 && b > 0:
			s += fmt.Sprintf(" %d-%d", int(a), int(b))
		case a > 0:
			s += fmt.Sprintf(" from %d", int(a))
		}
		return s
	}
	return c.Name + "(" + truncateRunes(canonicalArgs(c.Arguments), 60) + ")"
}

// observe records one tool turn. It returns the warning to add after the
// turn's results when the turn completes a cycle for the first time, or
// the LoopStuck that ends the loop: three more turns of the warned cycle,
// or a second cycle after the warning.
func (g *cycleGuard) observe(tc []turnCall) (warning string, stuck *LoopStuck) {
	if len(tc) == 0 {
		return "", nil
	}
	fps := make([]string, 0, len(tc))
	descs := make([]string, 0, len(tc))
	for _, c := range tc {
		if !g.seen[c.fp] {
			g.seen[c.fp] = true
			g.novelSinceSliding = true
		}
		fps = append(fps, c.fp)
		descs = append(descs, c.desc)
	}
	sort.Strings(fps)
	key := strings.Join(fps, "\x01")
	if _, ok := g.descs[key]; !ok {
		g.descs[key] = strings.Join(descs, " + ")
		g.firstOf[key] = tc[0]
	}

	if g.cycle != nil {
		if g.cycle[key] {
			g.repeats++
			if g.repeats >= cycleAfterWarning {
				return "", g.stuck(fmt.Sprintf("cycle: %s — repeated %d times each in %d turns, and %d more turns after a warning",
					g.cycleDesc(), cycleMinRepeats, cycleWindow, g.repeats), cycleWindow+g.repeats)
			}
		} else {
			g.cycle, g.repeats = nil, 0
		}
	}

	g.window = append(g.window, key)
	if len(g.window) > cycleWindow {
		g.window = g.window[len(g.window)-cycleWindow:]
	}
	counts, ok := cycleIn(g.window)
	if !ok {
		return "", nil
	}
	if g.warned {
		// The one warning was spent on an earlier cycle.
		g.setCycle(counts)
		return "", g.stuck(fmt.Sprintf("cycle: %s — repeated %d times each in the last %d turns, after an earlier cycle warning",
			g.cycleDesc(), minCount(counts), cycleWindow), cycleWindow)
	}
	g.warned = true
	g.setCycle(counts)
	g.window = g.window[:0]
	return g.warning(counts), nil
}

// cycleIn reports a full window of at most cycleMaxDistinct turn
// fingerprints, each repeated at least cycleMinRepeats times.
func cycleIn(window []string) (map[string]int, bool) {
	if len(window) < cycleWindow {
		return nil, false
	}
	counts := map[string]int{}
	for _, k := range window {
		counts[k]++
	}
	if len(counts) > cycleMaxDistinct {
		return nil, false
	}
	for _, n := range counts {
		if n < cycleMinRepeats {
			return nil, false
		}
	}
	return counts, true
}

func minCount(counts map[string]int) int {
	m := 0
	for _, n := range counts {
		if m == 0 || n < m {
			m = n
		}
	}
	return m
}

// setCycle names the cycle's turns, in the order the window saw them.
func (g *cycleGuard) setCycle(counts map[string]int) {
	g.cycle = map[string]bool{}
	for k := range counts {
		g.cycle[k] = true
	}
	g.repeats = 0
	g.order = nil
	for _, k := range g.window {
		if counts[k] > 0 && !slices.Contains(g.order, k) {
			g.order = append(g.order, k)
		}
	}
}

func (g *cycleGuard) cycleDesc() string {
	parts := make([]string, 0, len(g.order))
	for _, k := range g.order {
		parts = append(parts, g.descs[k])
	}
	return truncateRunes(strings.Join(parts, " → "), stuckReasonMax-160)
}

func (g *cycleGuard) warning(counts map[string]int) string {
	n, times := len(counts), minCount(counts)
	allReads := true
	for k := range counts {
		for _, fp := range strings.Split(k, "\x01") {
			if !isReadTool(strings.SplitN(fp, "\x00", 2)[0]) {
				allReads = false
			}
		}
	}
	what := fmt.Sprintf("made the same %d tool call(s)", n)
	if allReads {
		what = fmt.Sprintf("read the same %d range(s)", n)
	}
	return fmt.Sprintf("CYCLE: you have %s %d times each in your last %d turns: %s. They return nothing new. "+
		"Synthesize your answer now from what you already have, or do something different; "+
		"%d more turns of this cycle end the task as stuck.", what, times, cycleWindow, g.cycleDesc(), cycleAfterWarning)
}

func (g *cycleGuard) stuck(reason string, attempts int) *LoopStuck {
	first := g.firstOf[g.order[0]]
	return &LoopStuck{Tool: first.call.Name, Path: first.path, Kind: StuckKindCycle, Reason: reason, Attempts: attempts, Fingerprint: first.fp}
}

// readRepeats counts identical reads of a read-only tree
// (LoopOptions.ReadOnly): research reads a pinned snapshot, so reading the
// same lines again returns exactly what the model was given before.
type readRepeats struct {
	count map[string]int // read fingerprint → reads so far
	span  map[string][2]int
	turn  map[string]int // the turn of the first read
}

func newReadRepeats() *readRepeats {
	return &readRepeats{count: map[string]int{}, span: map[string][2]int{}, turn: map[string]int{}}
}

// check runs before a turn: the third repeat of a read ends the loop.
// It returns, by call index, how many times each read was made before.
func (r *readRepeats) check(tc []turnCall, allowed map[string]bool) (map[int]int, *LoopStuck) {
	prior := map[int]int{}
	pending := map[string]int{}
	for i, c := range tc {
		if !isReadTool(c.call.Name) || !allowed[c.call.Name] || c.path == "" {
			continue
		}
		n := r.count[c.fp] + pending[c.fp]
		if n == 0 {
			pending[c.fp]++
			continue
		}
		if n >= readRepeatLimit {
			return nil, &LoopStuck{Tool: c.call.Name, Path: c.path, Kind: StuckKindRepeatedRead, Attempts: n + 1, Fingerprint: c.fp,
				Reason: fmt.Sprintf("repeated read: %s read %d times; the tree is read-only, so every repeat returned what the first read did", c.desc, n+1)}
		}
		prior[i] = n
		pending[c.fp]++
	}
	return prior, nil
}

// annotate rewrites the results of repeated reads once they ran: the first
// repeat keeps its content and says so; the second gets a one-line answer
// (with dedup on) that says the next repeat ends the task. Then it counts
// the turn's reads.
func (r *readRepeats) annotate(tc []turnCall, prior map[int]int, results []adapter.ToolResult, turn int, short bool) {
	for i, c := range tc {
		if !isReadTool(c.call.Name) || c.path == "" || i >= len(results) {
			continue
		}
		res := &results[i]
		n, repeated := prior[i]
		if !repeated {
			if res.IsError || resultLooksFailed(c.call.Name, res.Content) {
				continue
			}
			var args map[string]any
			_ = json.Unmarshal([]byte(c.call.Arguments), &args)
			if _, ok := r.span[c.fp]; !ok {
				a, b := readSpan(args, res.Content)
				r.span[c.fp], r.turn[c.fp] = [2]int{a, b}, turn
			}
			r.count[c.fp]++
			continue
		}
		r.count[c.fp]++
		left := readRepeatLimit - n
		switch {
		case n >= 2 && short:
			sp := r.span[c.fp]
			res.Content = fmt.Sprintf("%s%s%d-%d (read %d times, the tree is read-only); synthesize your answer from what you have or read something new — %d more read of these lines ends this task as stuck",
				c.path, rereadMarker, sp[0], sp[1], n+1, left)
			res.IsError = false
		default:
			res.Content = withNote(res.Content, fmt.Sprintf("[repeated read] You already read these lines in turn %d; the tree is read-only, so they are unchanged. Use that result: %d more read(s) of them end this task as stuck.", r.turn[c.fp], left))
		}
	}
}

// withNote adds a note to a result: as a "note_repeat" field of a JSON
// object (codex results stay parseable), else as a trailing paragraph.
func withNote(content, note string) string {
	var obj map[string]any
	if strings.HasPrefix(strings.TrimSpace(content), "{") && json.Unmarshal([]byte(content), &obj) == nil {
		obj["note_repeat"] = note
		if b, err := json.MarshalIndent(obj, "", "  "); err == nil {
			return string(b)
		}
	}
	return content + "\n\n" + note
}
