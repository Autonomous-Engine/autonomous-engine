package strategy

import (
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// PromptScaffold bundles the per-tier knobs that an agent's system prompt
// and tool-loop should respect. The product thesis is that the codex +
// LSP + verify tools make ANY model (gemma → Opus) faster than Claude
// Code CLI on raw grep. But the shape of the scaffolding around those
// tools should still differ by tier — gemma wants plan-then-execute and
// explicit hints, Opus wants minimal hand-holding.
//
// We deliberately keep this struct small. Anything richer belongs in the
// Strategy interface, which is per-intent. This struct is per-agent-turn.
type PromptScaffold struct {
	// RequirePlan forces the model to emit a plan before executing. The
	// corresponding system-prompt snippet is returned by PlanDirective().
	RequirePlan bool

	// SelfCritique asks the model to critique its own output before
	// declaring done. Expensive, so only used for Constrained mode.
	SelfCritique bool

	// ExhaustiveSurfaceHint tells the agent to call `codex_references_to`-
	// style tools before answering "where is X used?" questions. Weak
	// models otherwise produce surface-level answers from a single grep.
	ExhaustiveSurfaceHint bool

	// MaxIterations is the tool-loop safety valve. Stuck detection still
	// triggers earlier in practice; this is the HARD cap.
	MaxIterations int

	// Sampling controls multi-sample generation. Nil → single-pass with
	// the strategy's default temperature.
	Sampling *SamplingMultiSample
}

// SamplingMultiSample describes an N-draft sweep with varying temperatures.
// Only Constrained mode uses it for now; the consumer is free to ignore
// the knob until it wires in a draft-and-merge pass.
type SamplingMultiSample struct {
	N         int
	TempStops []float64
}

// PromptScaffolding returns the tier-appropriate scaffold for the given
// operating mode. Call this once per agent invocation — agents pass the
// mode in from the resolved ModelProfile (agent_definitions → model_profiles).
//
// Tier rationale:
//   - Constrained (gemma 4:26b, llama, small open-weights): plan-first,
//     self-critique, explicit hints, 30-iteration cap, 3-temp multi-sample.
//   - Standard (GPT-4o mini, Sonnet 3.5 Haiku, mid Qwen): plan-first, 20 iters.
//   - Advanced (Opus 4.x, Sonnet 4.x, o1): trust the model, 15 iters, no
//     plan prefix — the model will self-structure and the extra turn is waste.
func PromptScaffolding(mode models.OperatingMode) PromptScaffold {
	switch mode {
	case models.OperatingModeConstrained:
		return PromptScaffold{
			RequirePlan:           true,
			SelfCritique:          true,
			ExhaustiveSurfaceHint: true,
			MaxIterations:         30,
			Sampling: &SamplingMultiSample{
				N:         3,
				TempStops: []float64{0.2, 0.5, 0.8},
			},
		}
	case models.OperatingModeStandard:
		return PromptScaffold{
			RequirePlan:   true,
			MaxIterations: 40,
		}
	case models.OperatingModeAdvanced:
		return PromptScaffold{
			MaxIterations: 15,
		}
	default:
		// Unknown tier — default to Standard to be safe rather than hand
		// Opus a gemma scaffold or vice-versa.
		return PromptScaffold{
			RequirePlan:   true,
			MaxIterations: 40,
		}
	}
}

// PromptPreamble returns a text block the agent can prepend (or append) to
// its system prompt to activate the scaffolding. Centralising the strings
// here keeps every agent consistent and lets us A/B the wording in one
// place. Empty string when every knob is off.
func (ps PromptScaffold) PromptPreamble() string {
	var b strings.Builder
	if ps.RequirePlan {
		// The plan goes in the same reply as the first tool calls: a reply
		// that is only a plan ends a tool loop (R6 T2 — the implementation
		// prompt demanded a tool call first while this asked for a plan
		// first).
		b.WriteString("## Plan\n\n" +
			"In your first reply, write a short bullet plan of the tools you will use and in what order, " +
			"and make the first tool calls in that same reply. If the plan proves wrong, revise it briefly " +
			"and continue. A plan keeps you from calling the same tool repeatedly with slightly different " +
			"arguments.\n\n")
	}
	if ps.ExhaustiveSurfaceHint {
		b.WriteString("## Be exhaustive on 'where is X used' questions\n\n" +
			"When answering questions of the form 'who depends on X' / 'everywhere Y is called' / " +
			"'what are the surface areas of Z', use `codex_dependents_of` or `codex_find_references` " +
			"BEFORE you answer. A single grep will miss indirection. Enumerate every hit — do not stop at " +
			"the first plausible match.\n\n")
	}
	if ps.SelfCritique {
		b.WriteString("## Self-critique\n\n" +
			"Before producing the final answer, do one silent pass: 'Is there a surface I haven't inspected? " +
			"Is any import in my generated code unverified? Did I confuse two similarly-named symbols?' " +
			"If the answer to any is yes, call one more tool to resolve it before replying.\n\n")
	}
	return b.String()
}

// ParseMode normalises the operating_mode string as stored in model_profiles.
// Empty / unknown strings fall back to Standard, matching the zero-value
// behaviour of PromptScaffolding.
func ParseMode(s string) models.OperatingMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "constrained":
		return models.OperatingModeConstrained
	case "advanced":
		return models.OperatingModeAdvanced
	case "standard":
		return models.OperatingModeStandard
	default:
		return models.OperatingModeStandard
	}
}
