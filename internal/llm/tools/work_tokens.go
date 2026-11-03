package tools

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// The budget counts work, not context. input_tokens + output_tokens on
// every call charged the whole re-sent prompt again: on a 70-call item,
// 470k of the 500k was the same system prompt, task, ledger and pins
// being resent against 31k of output, and a loop making a correct edit per
// call looked exactly like one churning. A call's work tokens are its
// output plus the input it had not sent before — the first prompt and its
// tool schemas, then each turn's new tool results and nudges. The ledger
// message is re-rendered every call and is not counted as new.

// sentTracker remembers the message contents a loop has already sent.
type sentTracker struct {
	seen  map[string]bool
	first bool
}

func newSentTracker() *sentTracker { return &sentTracker{seen: map[string]bool{}, first: true} }

// newInputTokens estimates the input tokens req adds over what was sent
// before (chars/4), and records req's messages as sent.
func (s *sentTracker) newInputTokens(req adapter.CompletionRequest) int64 {
	chars := 0
	if s.first {
		s.first = false
		if b, err := json.Marshal(req.Tools); err == nil {
			chars += len(b)
		}
	}
	for _, m := range req.Messages {
		key := string(m.Role) + "\x00" + m.Content
		if isLedgerMessage(m) || s.seen[key] {
			continue
		}
		s.seen[key] = true
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Arguments)
		}
	}
	return int64(chars / 4)
}

// noProgressWindow is how many consecutive tool turns may pass with no
// progress — no edit or write applied, no command run, no finding
// recorded, no call the loop had not made before — before the loop ends
// as stuck (no_progress). This is the runaway guard the budget was meant
// to be: churn stops; correct work does not.
// Above the cycle guard's window (9 turns + 3 after its warning), so a
// cycle keeps its own, more specific diagnosis.
const noProgressWindow = 12

// progressTracker decides whether a tool turn made progress.
type progressTracker struct {
	seen map[string]bool
	idle int
}

func newProgressTracker() *progressTracker { return &progressTracker{seen: map[string]bool{}} }

// observe records a turn's calls and results and reports whether the idle
// window is now full.
func (p *progressTracker) observe(calls []adapter.ToolCall, results []adapter.ToolResult) (full bool) {
	progress := false
	for k, c := range calls {
		if k >= len(results) {
			break
		}
		r := results[k]
		fp := c.Name + "\x00" + canonicalArgs(c.Arguments)
		novel := !p.seen[fp]
		p.seen[fp] = true
		if r.IsError || strings.HasPrefix(strings.TrimSpace(r.Content), "REJECTED") || isRereadResult(r.Content) {
			continue
		}
		why := ""
		switch {
		case IsFileEditTool(c.Name) || isWriteTool(c.Name):
			why = "edit applied"
		case c.Name == "run_command":
			why = "command run"
		case c.Name == "record_finding" && isFindingRecorded(r):
			why = "finding recorded"
		case novel:
			why = "new call"
		}
		if why != "" {
			progress = true
			log.Printf("tool-loop: progress: %s (%s)", why, describeCall(c))
		}
	}
	if progress {
		p.idle = 0
		return false
	}
	p.idle++
	return p.idle >= noProgressWindow
}
