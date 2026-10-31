package stage

import (
	"bytes"
	"encoding/json"
	"log"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// MarkUngrounded applies the evidence flag "ungrounded" (codex coherence
// Part 3: the item's latest research or implementation run recorded more
// empty codex lookups than CODEX_GAP_THRESHOLD, internal/codexgap) to o,
// treating the run like one with missing evidence: no automatic approval.
//
//   - An approving decision (approve, approve_pending_second_pass) becomes
//     escalate: no PR, the parent goes to human review with why as the
//     reason (the existing human path). The verdict artifact, when there is
//     one, keeps the verifier's own verdict.
//   - Any other decision is kept.
//   - The evidence carried on verification.complete (Message.Implementation)
//     gets the flag, so pr-coordinator and status-orchestrator judge it with
//     the same rule (evidence.ReleaseDecision: never released automatically).
func (o *Outcome) MarkUngrounded(why string) {
	if o.Message.Decision.Approves() {
		reason := "The verifier approved, but " + why + ": the run worked without the codex, so it is not approved automatically. Human review required."
		o.Message.Decision, o.Message.Reason = DecisionEscalate, reason
		o.Result.Decision, o.Result.Reason = DecisionEscalate, reason
	}
	o.Message.Implementation = flagImplementation(o.Message.Implementation)
}

// flagImplementation adds evidence.FlagUngrounded to the evidence of an
// implementation.complete payload; a payload without evidence (or one that
// cannot be read) is returned unchanged.
func flagImplementation(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return raw
	}
	ev, ok := fields["evidence"]
	if !ok || len(bytes.TrimSpace(ev)) == 0 || bytes.Equal(bytes.TrimSpace(ev), []byte("null")) {
		return raw
	}
	flagged, err := evidence.WithFlag(ev, evidence.FlagUngrounded)
	if err != nil {
		log.Printf("verification stage: cannot flag the evidence ungrounded: %v", err)
		return raw
	}
	fields["evidence"] = flagged
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}
