package interactions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/interactions"
)

// scriptedLLM returns a fixed reply or error. Used so the triage logic
// can be tested without any real LLM.
type scriptedLLM struct {
	reply string
	err   error
}

func (s *scriptedLLM) Classify(_ context.Context, _ string) (string, error) {
	return s.reply, s.err
}

func TestTriage_RecognizesAntipattern(t *testing.T) {
	llm := &scriptedLLM{reply: `{"classified_as":"codex_antipattern","confidence":0.92,"proposed_entry":{"section":"antipatterns","entry":"don't use int IDs; use GUIDs"}}`}
	tri := interactions.NewTriage(llm, 0.8)

	res, err := tri.Classify(context.Background(), "Don't use int IDs, we use GUIDs", "we use GUIDs")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.ClassifiedAs != interactions.ClassifiedCodexAntipattern {
		t.Errorf("classified_as = %q, want codex_antipattern", res.ClassifiedAs)
	}
	if !res.ShouldPromote {
		t.Errorf("ShouldPromote = false, want true (0.92 >= 0.8, codex-eligible)")
	}
}

func TestTriage_LowConfidenceDoesNotPromote(t *testing.T) {
	llm := &scriptedLLM{reply: `{"classified_as":"codex_pattern","confidence":0.55,"proposed_entry":{"section":"patterns","entry":"use X"}}`}
	tri := interactions.NewTriage(llm, 0.8)

	res, err := tri.Classify(context.Background(), "q", "a")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.ShouldPromote {
		t.Error("ShouldPromote = true with conf 0.55 < 0.8, want false")
	}
}

func TestTriage_AdHocIsNotPromoted(t *testing.T) {
	llm := &scriptedLLM{reply: `{"classified_as":"ad_hoc","confidence":0.75,"proposed_entry":null}`}
	tri := interactions.NewTriage(llm, 0.8)

	res, _ := tri.Classify(context.Background(), "what should I name this PR?", "call it feat/fax")
	if res.ShouldPromote {
		t.Error("ShouldPromote = true for ad_hoc, want false")
	}
	if res.ClassifiedAs != interactions.ClassifiedAdHoc {
		t.Errorf("ClassifiedAs = %q, want ad_hoc", res.ClassifiedAs)
	}
}

func TestTriage_OverrideEphemeralForcesNoCodex(t *testing.T) {
	llm := &scriptedLLM{reply: `{"classified_as":"codex_pattern","confidence":0.95}`}
	tri := interactions.NewTriage(llm, 0.8)

	override := interactions.SaveAsEphemeral
	res, err := tri.ClassifyWithOverride(context.Background(), "q", "a", &override)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.ClassifiedAs != interactions.ClassifiedEphemeral {
		t.Errorf("ephemeral override should yield ephemeral, got %q", res.ClassifiedAs)
	}
	if res.ShouldPromote {
		t.Error("ephemeral override must not promote")
	}
}

func TestTriage_OverrideCodexFactForcesPromote(t *testing.T) {
	llm := &scriptedLLM{reply: `{"classified_as":"ad_hoc","confidence":0.40,"proposed_entry":null}`}
	tri := interactions.NewTriage(llm, 0.8)

	override := interactions.SaveAsCodexFact
	res, _ := tri.ClassifyWithOverride(context.Background(), "q", "GUIDs not ints", &override)
	if !res.ShouldPromote {
		t.Error("codex_fact override must force promote regardless of classifier")
	}
	if !res.ClassifiedAs.IsCodex() {
		t.Errorf("codex_fact override must yield codex_* classification, got %q", res.ClassifiedAs)
	}
}

func TestTriage_UnparseableLLMReplyDegrades(t *testing.T) {
	// Real LLMs sometimes return prose / malformed JSON. Triage must degrade
	// to ad_hoc instead of propagating the error.
	llm := &scriptedLLM{reply: "I'm not sure, honestly."}
	tri := interactions.NewTriage(llm, 0.8)

	res, err := tri.Classify(context.Background(), "q", "a")
	if err != nil {
		t.Fatalf("should not error on unparseable: %v", err)
	}
	if res.ClassifiedAs != interactions.ClassifiedAdHoc {
		t.Errorf("ClassifiedAs = %q, want ad_hoc (unparseable fallback)", res.ClassifiedAs)
	}
	if res.ShouldPromote {
		t.Error("unparseable must not promote")
	}
}

func TestTriage_LLMErrorPropagates(t *testing.T) {
	// Real error from the LLM (network, timeout) should propagate so the
	// caller can retry.
	llm := &scriptedLLM{err: errors.New("timeout")}
	tri := interactions.NewTriage(llm, 0.8)

	if _, err := tri.Classify(context.Background(), "q", "a"); err == nil {
		t.Error("LLM error must propagate")
	}
}
