package tools

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrNoEvidence is wrapped by *NoEvidence.
var ErrNoEvidence = errors.New("no evidence")

// NoEvidence (R18 E1) ends a loop whose model answered without running a
// single tool call, again after LoopOptions.EvidenceNudge. Note keeps the
// final answer (the prose the model wrote instead of calling tools).
type NoEvidence struct {
	// Answers is how many tool-less answers the loop received (2).
	Answers int `json:"answers"`
	// Note is the last tool-less answer, bounded.
	Note string `json:"note,omitempty"`
}

func (e *NoEvidence) Error() string {
	return fmt.Sprintf("no evidence: the model answered %d times without running a tool call", e.Answers)
}

func (e *NoEvidence) Unwrap() error { return ErrNoEvidence }

// noEvidenceNoteMax bounds NoEvidence.Note.
const noEvidenceNoteMax = 4000

func boundNote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= noEvidenceNoteMax {
		return s
	}
	cut := noEvidenceNoteMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// textToolCallRE matches a tool call written as JSON text in a reply
// ({"name": "codex_read_file", "arguments": …}): a call the model meant to
// make that the adapter did not turn into a tool call.
var textToolCallRE = regexp.MustCompile(`\{\s*"name"\s*:\s*"[A-Za-z_][A-Za-z0-9_]*"\s*,\s*"arguments"\s*:`)

// planHeadingRE matches a reply that opens with a plan heading
// ("### Plan", "## Plan:", "Plan:"), the shape the scaffold's first-reply
// instruction produces when the model writes the plan but no calls.
var planHeadingRE = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:\*\*)?plan(?:\*\*)?\s*:?\s*(?:\n|$)`)

// planTriggers reports whether a loop judges looksLikeUnexecutedPlan: the
// research loops (EvidenceNudge) and the implementation loops
// (StuckControl), which must act with tools. A chat-style loop (neither
// option) keeps the phrase-only prose check (R18 review m1).
func planTriggers(opts LoopOptions) bool {
	return opts.EvidenceNudge != "" || opts.StuckControl
}

// looksLikeUnexecutedPlan is the R18 addition to the prose-planning check:
// a reply that writes tool calls as JSON text, or — on the first turn —
// opens with a plan heading, planned work it did not do. The phrase list
// ("let me", "i'll", "next step", …) does not match attempt 15's
// "### Plan … #### Step 1: Read the specified files … ```json {…}```".
func looksLikeUnexecutedPlan(content string, firstTurn bool) bool {
	if textToolCallRE.MatchString(content) {
		return true
	}
	return firstTurn && planHeadingRE.MatchString(content)
}
