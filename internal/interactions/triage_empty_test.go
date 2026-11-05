package interactions

import (
	"context"
	"testing"
)

type recordingTriageLLM struct{ called bool }

func (s *recordingTriageLLM) Classify(context.Context, string) (string, error) {
	s.called = true
	return `{"classified_as":"codex_pattern","confidence":0.99,"proposed_entry":{}}`, nil
}

// Approve and Retry answers carried no text, yet the "codex fact" override
// forced each into an empty codex_patterns row and bumped the codex
// version (F38). An answer with nothing in it is never a codex entry.
func TestClassifyWithOverrideNeverPromotesAnEmptyAnswer(t *testing.T) {
	llm := &recordingTriageLLM{}
	tr := NewTriage(llm, 0.8)
	cf := SaveAsCodexFact
	res, err := tr.ClassifyWithOverride(context.Background(), "Approve the PRD?", "   ", &cf)
	if err != nil {
		t.Fatal(err)
	}
	if res.ShouldPromote || res.ClassifiedAs != ClassifiedEphemeral {
		t.Fatalf("res = %+v, want ephemeral and not promoted", res)
	}
	if llm.called {
		t.Fatal("an empty answer must not be sent to the classifier")
	}
}
