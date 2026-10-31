package main

import (
	"github.com/autonomousengine/autonomous-engine/internal/failures"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools"
)

// stopLoopStuck (R10 W3): a tool loop kept failing the same way
// (tools.LoopStuck) and no later pass got past it. The attempt stops on the
// structured failure path (finalizeStopped): work in progress committed,
// not pushed, the item blocked with the stuck calls listed as failure
// records — not a budget burn.
const stopLoopStuck = "loop_stuck"

// stuckStopInfo is the stop for the attempt's LoopStuck failures (the last
// one is the reason); rr is the pre-commit repair result when it ran.
func stuckStopInfo(stuck []tools.LoopStuck, rr *repairResult) *stopInfo {
	last := stuck[len(stuck)-1]
	return &stopInfo{reason: stopLoopStuck, detail: last.Error(), repair: rr, stuck: append([]tools.LoopStuck(nil), stuck...)}
}

// stuckRecords are the failure records of LoopStuck failures: tooling
// failures on the file the call targeted, the reason as the message.
func stuckRecords(stuck []tools.LoopStuck) []failures.Record {
	out := make([]failures.Record, 0, len(stuck))
	for _, s := range stuck {
		r := failures.Record{Category: failures.CategoryTooling, Tool: s.Tool, File: s.Path, Message: s.Reason}
		r.Fingerprint = failures.Fingerprint(r)
		out = append(out, r)
	}
	return out
}

// stuckTracker is a tracker holding the LoopStuck records (the escalation
// prompt lists a tracker's top fingerprints).
func stuckTracker(stuck []tools.LoopStuck) *failures.Tracker {
	t := failures.NewTracker()
	t.Observe(stuckRecords(stuck), "loop_stuck", false)
	return t
}
