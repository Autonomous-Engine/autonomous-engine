package main

import (
	"context"
	"testing"
)

type stubLLM struct {
	reply string
	err   error
}

func (s *stubLLM) oneShotPrompt(_ context.Context, _ string) (string, error) {
	return s.reply, s.err
}

func TestTriageAdapter_ExtractsJSONFromMarkdownFence(t *testing.T) {
	inner := `{"classified_as":"codex_pattern","confidence":0.9,"proposed_entry":null}`
	reply := "Here is my classification:\n```json\n" + inner + "\n```\n"
	a := newTriageAdapterForTest(&stubLLM{reply: reply})
	out, err := a.Classify(context.Background(), "doesn't matter")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if out != inner {
		t.Errorf("extracted = %q, want %q", out, inner)
	}
}

func TestTriageAdapter_PassesThroughRawJSON(t *testing.T) {
	inner := `{"classified_as":"ad_hoc","confidence":0.5}`
	a := newTriageAdapterForTest(&stubLLM{reply: inner})
	out, _ := a.Classify(context.Background(), "x")
	if out != inner {
		t.Errorf("passthrough = %q, want %q", out, inner)
	}
}
