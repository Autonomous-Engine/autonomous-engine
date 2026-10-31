package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// cycleTree is a 40-line a.ts.
func cycleTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "export const v%d = %d;\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(root, "a.ts"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func linesOfA(a, b int) scriptedCall {
	return scriptedCall{"read_file", fmt.Sprintf(`{"path":"a.ts","start_line":%d,"end_line":%d}`, a, b)}
}

var (
	rA = linesOfA(1, 5)
	rB = linesOfA(10, 12)
	rC = linesOfA(20, 30)
	rD = linesOfA(31, 35)
)

func repeatCalls(n int, pattern ...scriptedCall) []scriptedCall {
	var out []scriptedCall
	for len(out) < n {
		out = append(out, pattern[len(out)%len(pattern)])
	}
	return out
}

func userMessages(r adapter.CompletionRequest, prefix string) int {
	n := 0
	for _, m := range r.Messages {
		if m.Role == adapter.RoleUser && strings.HasPrefix(m.Content, prefix) {
			n++
		}
	}
	return n
}

// A period-3 cycle (A B C ×3) gets one warning naming the three ranges;
// three more turns of it end the loop with a cycle LoopStuck.
func TestCycleWarnsOnceThenEndsTheLoop(t *testing.T) {
	calls := append([]scriptedCall{rD}, repeatCalls(20, rA, rB, rC)...)
	model, err := stuckLoop(t, cycleTree(t), calls, LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || s.Kind != StuckKindCycle || !errors.Is(err, ErrLoopStuck) {
		t.Fatalf("err = %v, want a cycle LoopStuck", err)
	}
	// Turn 1 is D; turns 2-10 are the first 9 cycle turns (warning after
	// turn 10); turns 11-13 repeat it: the 13th call ends the loop.
	if len(model.reqs) != 13 {
		t.Fatalf("calls = %d, want 13", len(model.reqs))
	}
	last := model.reqs[len(model.reqs)-1]
	if got := userMessages(last, "CYCLE: you have read the same 3 range(s) 3 times each in your last 9 turns: read_file a.ts 1-5 → read_file a.ts 10-12 → read_file a.ts 20-30"); got != 1 {
		t.Fatalf("cycle warnings = %d, want 1:\n%v", got, last.Messages)
	}
	if s.Tool != "read_file" || s.Path != "a.ts" || !strings.Contains(s.Reason, "cycle:") || !strings.Contains(s.Reason, "a.ts 20-30") {
		t.Fatalf("LoopStuck = %+v", *s)
	}
}

// Leaving the cycle after the warning resets the count; a second cycle
// later ends the loop without a second warning.
func TestSecondCycleAfterTheWarningEndsTheLoop(t *testing.T) {
	calls := repeatCalls(9, rA, rB, rC)                          // warning after turn 9
	calls = append(calls, rA, rB, rD)                            // two cycle turns, then out
	calls = append(calls, repeatCalls(9, rD, linesOfA(2, 3))...) // a new 2-cycle with D
	calls = append(calls, repeatCalls(10, rA)...)
	model, err := stuckLoop(t, cycleTree(t), calls, LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || s.Kind != StuckKindCycle || !strings.Contains(s.Reason, "after an earlier cycle warning") {
		t.Fatalf("err = %v, want the second cycle to end the loop", err)
	}
	if got := userMessages(model.reqs[len(model.reqs)-1], "CYCLE:"); got != 1 {
		t.Fatalf("cycle warnings = %d, want 1", got)
	}
}

// Four distinct reads in rotation, or varied reads, are not a cycle; and
// without StuckControl nothing watches turns.
func TestNoCycleWithoutRepetitionOrStuckControl(t *testing.T) {
	model, err := stuckLoop(t, cycleTree(t), repeatCalls(11, rA, rB, rC, rD), LoopOptions{StuckControl: true})
	if err != nil {
		t.Fatalf("period-4 rotation: err = %v", err)
	}
	if userMessages(model.reqs[len(model.reqs)-1], "CYCLE:") != 0 {
		t.Fatal("a period-4 rotation was warned as a cycle")
	}
	model, err = stuckLoop(t, cycleTree(t), repeatCalls(16, rA, rB, rC), LoopOptions{})
	if err != nil {
		t.Fatalf("without StuckControl: err = %v", err)
	}
	if userMessages(model.reqs[len(model.reqs)-1], "CYCLE:") != 0 {
		t.Fatal("cycle warning without StuckControl")
	}
}

// The sliding-window detector ends the loop when it fires again after its
// warning and no turn since made a new call (a period-4 rotation, which
// the cycle rule does not cover).
func TestSlidingWindowEndsAfterItsWarningWithoutNewCalls(t *testing.T) {
	model, err := stuckLoop(t, cycleTree(t), repeatCalls(30, rA, rB, rC, rD), LoopOptions{StuckControl: true})
	var s *LoopStuck
	if !errors.As(err, &s) || s.Kind != StuckKindSlidingWindow || !strings.Contains(s.Reason, "no new call since the warning") {
		t.Fatalf("err = %v, want a sliding_window LoopStuck", err)
	}
	if len(model.reqs) != 16 {
		t.Fatalf("calls = %d, want 16 (warning after turn 8, end at turn 16)", len(model.reqs))
	}
}

// New calls since the warning are progress: the window warns again
// instead of ending (the R10 exit after 3 recoveries still applies).
func TestSlidingWindowWithNewCallsKeepsWarning(t *testing.T) {
	var calls []scriptedCall
	for i := 1; i <= 20; i++ {
		calls = append(calls, linesOfA(i, i+1))
	}
	model, err := stuckLoop(t, cycleTree(t), calls, LoopOptions{StuckControl: true})
	if err != nil {
		t.Fatalf("err = %v, want the loop to run to its answer", err)
	}
	if got := userMessages(model.reqs[len(model.reqs)-1], "WARNING: You have been calling the same tools"); got != 2 {
		t.Fatalf("sliding-window warnings = %d, want 2", got)
	}
}

// ReadOnly: the first repeat of a read keeps its content with a note, the
// second is a one-line answer, the third ends the loop.
func TestReadOnlyRepeatedReadsEndTheLoop(t *testing.T) {
	calls := repeatCalls(10, rA, rB)
	opts := LoopOptions{StuckControl: true, ReadOnly: true, EvidenceLedger: true, DedupReads: true}
	model, err := stuckLoop(t, cycleTree(t), calls, opts)
	var s *LoopStuck
	if !errors.As(err, &s) || s.Kind != StuckKindRepeatedRead || s.Path != "a.ts" || s.Attempts != 4 {
		t.Fatalf("err = %v, want a repeated_read LoopStuck", err)
	}
	if len(model.reqs) != 7 {
		t.Fatalf("calls = %d, want 7 (A's fourth read)", len(model.reqs))
	}
	// Request 4 has turn 3's result (A's first repeat): content and note.
	third := lastToolResult(model.reqs[3])
	if !strings.Contains(third, "export const v1 = 1;") || !strings.Contains(third, "[repeated read] You already read these lines in turn 1") {
		t.Fatalf("first repeat:\n%s", third)
	}
	// Request 6 has turn 5's result (A's second repeat): one line.
	fifth := lastToolResult(model.reqs[5])
	if strings.Contains(fifth, "export const") || !strings.Contains(fifth, "a.ts: unchanged since your last read of lines 1-5") || !strings.Contains(fifth, "ends this task as stuck") {
		t.Fatalf("second repeat:\n%s", fifth)
	}
}

// Without ReadOnly (implementation) a repeated read is not counted.
func TestRepeatedReadsWithoutReadOnlyAreNotCounted(t *testing.T) {
	model, err := stuckLoop(t, cycleTree(t), repeatCalls(7, rA, rB), LoopOptions{StuckControl: true})
	var s *LoopStuck
	if errors.As(err, &s) && s.Kind == StuckKindRepeatedRead {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(lastToolResult(model.reqs[3]), "[repeated read]") {
		t.Fatal("repeat note without ReadOnly")
	}
}

// A JSON result (codex_read_file) gets the note as a field, so it stays
// parseable for the ledger.
func TestRepeatNoteKeepsJSONParseable(t *testing.T) {
	got := withNote("{\n  \"content\": \"x\",\n  \"end_line\": 3,\n  \"start_line\": 1\n}", "[repeated read] n")
	if jsonField(got, "content") != "x" || jsonInt(got, "end_line") != 3 || jsonField(got, "note_repeat") != "[repeated read] n" {
		t.Fatalf("note broke the JSON:\n%s", got)
	}
	if got := withNote("plain", "n"); got != "plain\n\nn" {
		t.Fatalf("plain = %q", got)
	}
}
