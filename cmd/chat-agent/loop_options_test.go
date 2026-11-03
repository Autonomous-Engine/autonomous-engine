package main

import "testing"

// Live, the chat agent explored the codex for 14 calls instead of creating
// the work item, hit its 15-call cap and the user got "Sorry, I encountered
// an error: tool loop reached safety limit" — no reply, no work item. At
// the cap the agent answers from what it did instead of failing.
func TestChatLoopClosesWithAnAnswerAtItsCap(t *testing.T) {
	if !chatLoopOptions().CloseAtLimit {
		t.Fatal("the chat loop must close with an answer at its iteration cap (CloseAtLimit)")
	}
}
