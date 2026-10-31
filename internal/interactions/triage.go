// internal/interactions/triage.go — answer-classification logic. Decides
// whether a user reply should be promoted to a codex entry, held as an
// ad-hoc qdrant-only note, or marked ephemeral. Consumed by
// knowledge-indexer (which wraps an LLM call implementing TriageLLM).
// See spec §8.
package interactions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TriageLLM is a narrow LLM contract so this package can be tested without
// depending on the full internal/llm adapter graph.
type TriageLLM interface {
	Classify(ctx context.Context, prompt string) (string, error)
}

type Triage struct {
	llm                TriageLLM
	promotionThreshold float64
}

// NewTriage constructs a classifier. promotionThreshold is the minimum
// confidence required for a codex_* classification to auto-promote.
// Typically 0.8 — configurable via env on the knowledge-indexer side.
func NewTriage(llm TriageLLM, promotionThreshold float64) *Triage {
	return &Triage{llm: llm, promotionThreshold: promotionThreshold}
}

// TriageResult is the output of a Classify call.
type TriageResult struct {
	ClassifiedAs  ClassifiedAs
	Confidence    float64
	ProposedEntry map[string]any
	ShouldPromote bool
}

// Classify with no user override — confidence gate decides promotion.
func (t *Triage) Classify(ctx context.Context, question, answer string) (*TriageResult, error) {
	return t.ClassifyWithOverride(ctx, question, answer, nil)
}

// ClassifyWithOverride honors a user save-as choice:
//   - SaveAsEphemeral: skip the LLM entirely, mark ephemeral, do not promote.
//   - SaveAsCodexFact: call the LLM to pick a codex section, but force
//     promotion regardless of confidence.
//   - nil: default — promote iff LLM says codex_* AND confidence >= threshold.
func (t *Triage) ClassifyWithOverride(ctx context.Context, question, answer string, override *SaveAsOverride) (*TriageResult, error) {
	if override != nil && *override == SaveAsEphemeral {
		return &TriageResult{ClassifiedAs: ClassifiedEphemeral, ShouldPromote: false}, nil
	}

	prompt := buildClassifierPrompt(question, answer)
	raw, err := t.llm.Classify(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("triage llm: %w", err)
	}

	parsed, ok := parseClassification(raw)
	if !ok {
		// Unparseable reply — degrade gracefully. No codex, no ephemeral
		// flag. The caller still records the answer in qdrant.
		return &TriageResult{ClassifiedAs: ClassifiedAdHoc}, nil
	}

	res := &TriageResult{
		ClassifiedAs:  ClassifiedAs(parsed.ClassifiedAs),
		Confidence:    parsed.Confidence,
		ProposedEntry: parsed.ProposedEntry,
	}
	switch {
	case override != nil && *override == SaveAsCodexFact:
		if !res.ClassifiedAs.IsCodex() {
			// Classifier disagreed but user overrode. Default to pattern
			// section — the proposed_entry (if any) is informative for payload.
			res.ClassifiedAs = ClassifiedCodexPattern
		}
		res.ShouldPromote = true
	case res.ClassifiedAs.IsCodex() && res.Confidence >= t.promotionThreshold:
		res.ShouldPromote = true
	default:
		res.ShouldPromote = false
		if res.ClassifiedAs.IsCodex() {
			// classifier said codex but confidence too low — demote to ad_hoc
			res.ClassifiedAs = ClassifiedAdHoc
		}
	}
	return res, nil
}

type classificationReply struct {
	ClassifiedAs  string         `json:"classified_as"`
	Confidence    float64        `json:"confidence"`
	ProposedEntry map[string]any `json:"proposed_entry"`
}

func parseClassification(raw string) (classificationReply, bool) {
	trimmed := strings.TrimSpace(raw)
	var r classificationReply
	if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
		return classificationReply{}, false
	}
	if r.ClassifiedAs == "" {
		return classificationReply{}, false
	}
	return r, true
}

func buildClassifierPrompt(question, answer string) string {
	var b strings.Builder
	b.WriteString(classifierSystem)
	b.WriteString("\n\nQUESTION:\n")
	b.WriteString(question)
	b.WriteString("\n\nANSWER:\n")
	b.WriteString(answer)
	b.WriteString("\n\nReturn ONLY the JSON object, no prose.\n")
	return b.String()
}

const classifierSystem = `You classify answers the user gave to an AI agent. Return one of:

  codex_module_correction   — fact about which files/functions exist ("use app/lib/mail.ts sendMail, not sendEmail which doesn't exist")
  codex_pattern             — preferred way to do something ("we use React Server Actions for form submits")
  codex_antipattern         — something to avoid ("don't use int IDs, we use GUIDs")
  codex_domain_fact         — business/domain rule ("VAT only applies in EU")
  codex_style_rule          — code-style preference beyond lint ("error messages must start with lowercase")
  ad_hoc                    — specific to this work item, not worth codifying
  ephemeral                 — just this run, user explicitly scoped it

Output JSON: {"classified_as": <one of the above>, "confidence": 0..1, "proposed_entry": {"section": "...", "entry": "..."} or null}`
