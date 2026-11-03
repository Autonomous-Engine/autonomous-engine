package tools

import "github.com/autonomousengine/autonomous-engine/internal/llm/adapter"

// LoopCarry hands one pass's compacted transcript and evidence ledger to
// the next (LoopOptions.Carry). The implementation agent re-enters the tool
// loop for a second pass — the no-edit nudge, the verify gate — with the
// original two messages plus the nudge, so the ledger and the pinned file
// contents of the first pass were gone: the pass re-read every file and,
// live, appended the same 14 lines to options.go twice.
type LoopCarry struct {
	// Messages is the transcript as the loop left it: the ledger message
	// (reads, edits, pinned current file contents) and the kept tail.
	Messages []adapter.ChatMessage
	ledger   *evidenceLedger
}

// Resume returns the carried transcript for the next pass, or fallback
// when nothing was carried yet.
func (c *LoopCarry) Resume(fallback []adapter.ChatMessage) []adapter.ChatMessage {
	if c == nil || len(c.Messages) == 0 {
		return fallback
	}
	return append([]adapter.ChatMessage(nil), c.Messages...)
}
