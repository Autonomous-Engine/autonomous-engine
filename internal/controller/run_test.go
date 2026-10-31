package controller

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MemoryRecorder is an in-memory Recorder.
type memRecorder struct {
	mu      sync.Mutex
	rows    []EventRow
	state   State
	detail  Detail
	failAll bool
}

func (m *memRecorder) AppendEvent(_ context.Context, _ uuid.UUID, row EventRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAll {
		return errors.New("injected")
	}
	m.rows = append(m.rows, row)
	return nil
}

func (m *memRecorder) RecordTransition(_ context.Context, _ uuid.UUID, rows []EventRow, to State, d Detail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAll {
		return errors.New("injected")
	}
	m.rows = append(m.rows, rows...)
	m.state, m.detail = to, d
	return nil
}

func (m *memRecorder) RecordTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, rows []EventRow, to *State, d *Detail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, rows...)
	if to != nil {
		m.state = *to
	}
	if d != nil {
		m.detail = *d
	}
	return nil
}

func newTestRun(rec Recorder) *Run {
	return NewRun(rec, Started{ID: uuid.New(), Attempt: 1, NextSeq: 2}, uuid.New(), "task-1")
}

func TestRunRecordsToolRowsAndTransitions(t *testing.T) {
	rec := &memRecorder{}
	r := newTestRun(rec)
	r.SetTestCommand("go test ./... -count=1")
	var lines []string
	r.OnTransition = func(tr Transition) { lines = append(lines, tr.LogLine()) }
	ctx := context.Background()

	r.Observe(ctx, Event{Kind: EventTool, CallID: "c1", Tool: "read_file", Path: "add.go"})
	r.Observe(ctx, write("add_test.go"))
	r.Observe(ctx, Event{Kind: EventTool, CallID: "red", Tool: "run_command", Command: "go test ./... -count=1", ExitCode: exit(1)})
	r.Observe(ctx, edit("add.go"))
	if got := r.Observe(ctx, Event{Kind: EventTool, CallID: "green", Tool: "run_command", Command: "go test ./... -count=1", ExitCode: exit(0)}); got != StateGreenObserved {
		t.Fatalf("Observe returned %s, want green_observed", got)
	}

	var seqs []int64
	var kinds []string
	for _, row := range rec.rows {
		seqs = append(seqs, row.Seq)
		kinds = append(kinds, row.Kind)
	}
	wantKinds := []string{"tool", "tool", "transition", "tool", "transition", "tool", "transition", "tool", "transition"}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("kinds = %v, want %v", kinds, wantKinds)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] || seqs[i] != int64(i+2) {
			t.Fatalf("row %d = %s seq %d, want %s seq %d (all: %v %v)", i, kinds[i], seqs[i], wantKinds[i], i+2, kinds, seqs)
		}
	}
	if rec.state != StateGreenObserved {
		t.Fatalf("persisted state = %s", rec.state)
	}
	if rec.detail.Red == nil || rec.detail.Red.CallID != "red" || rec.detail.Green == nil || rec.detail.Green.CallID != "green" {
		t.Fatalf("detail red/green = %+v / %+v", rec.detail.Red, rec.detail.Green)
	}
	if len(lines) != 4 || lines[1] != "controller: red_observed (go test ./... -count=1 exit 1, call red)" {
		t.Fatalf("lifecycle lines = %q", lines)
	}

	// The terminal state is written in the caller's tx and leaves memory alone.
	tr, err := r.FinishTx(ctx, nil, FinalVerified, "")
	if err != nil || tr == nil || tr.To != StateFinalVerified {
		t.Fatalf("FinishTx = %+v, %v", tr, err)
	}
	if rec.state != StateFinalVerified || rec.detail.Final == nil || rec.detail.Final.Outcome != FinalVerified {
		t.Fatalf("after FinishTx: state %s detail %+v", rec.state, rec.detail.Final)
	}
	if r.State() != StateGreenObserved {
		t.Fatalf("in-memory state changed by FinishTx: %s", r.State())
	}
	// A fallback outcome after a rollback derives from the same state, with
	// fresh seqs.
	last := rec.rows[len(rec.rows)-1].Seq
	tr, _ = r.FinishTx(ctx, nil, FinalBlocked, "fallback")
	if tr == nil || tr.From != StateGreenObserved || tr.To != StateBlocked || tr.Seq <= last {
		t.Fatalf("fallback FinishTx = %+v (last seq %d)", tr, last)
	}
}

// A compound configured test command (detected, or the repository policy
// override) set through SetTestCommand is evidence when run verbatim.
func TestRunConfiguredCompoundCommandIsEvidence(t *testing.T) {
	rec := &memRecorder{}
	r := newTestRun(rec)
	r.SetTestCommand("make lint; make test")
	ctx := context.Background()
	r.Observe(ctx, write("a_test.go"))
	if got := r.Observe(ctx, run("make lint; make test", 2)); got != StateRedObserved {
		t.Fatalf("after failing configured command: %s, want red_observed", got)
	}
	r.Observe(ctx, edit("a.go"))
	if got := r.Observe(ctx, run("make lint;  make test", 0)); got != StateGreenObserved {
		t.Fatalf("after passing configured command: %s, want green_observed", got)
	}
	// An unconfigured pipe is still not evidence.
	r2 := newTestRun(&memRecorder{})
	r2.SetTestCommand("go test ./... -count=1")
	r2.Observe(ctx, write("a_test.go"))
	if got := r2.Observe(ctx, run("go test ./... | tail", 1)); got != StateTestAuthored {
		t.Fatalf("piped unconfigured command advanced to %s", got)
	}
}

func TestRunNoTestAuthoredNoteIsRecorded(t *testing.T) {
	rec := &memRecorder{}
	r := newTestRun(rec)
	ctx := context.Background()
	r.Observe(ctx, edit("add.go"))
	r.Observe(ctx, run("go test ./...", 0))
	lastRow := rec.rows[len(rec.rows)-1]
	if r.State() != StateImplement || lastRow.Kind != KindTool || lastRow.Note != "no test authored this run" {
		t.Fatalf("state %s, last row %+v", r.State(), lastRow)
	}
}

func TestRunRegressionClearsGreen(t *testing.T) {
	rec := &memRecorder{}
	r := newTestRun(rec)
	ctx := context.Background()
	for _, ev := range []Event{write("a_test.go"), run("go test ./...", 1), edit("a.go"), run("go test ./...", 0), run("go test ./...", 1)} {
		r.Observe(ctx, ev)
	}
	if r.State() != StateImplement || rec.detail.Green != nil || rec.detail.Red == nil {
		t.Fatalf("state %s, detail %+v", r.State(), rec.detail)
	}
}

func TestRunPersistenceFailureDoesNotStopTheRun(t *testing.T) {
	rec := &memRecorder{failAll: true}
	r := newTestRun(rec)
	var logged int
	r.Logf = func(string, ...any) { logged++ }
	r.Observe(context.Background(), write("a_test.go"))
	if r.State() != StateTestAuthored || logged != 1 {
		t.Fatalf("state %s logged %d", r.State(), logged)
	}
}

func TestNilRunIsNoOp(t *testing.T) {
	var r *Run
	r.SetTestCommand("x")
	r.Note(context.Background(), "x")
	if r.Observe(context.Background(), write("a_test.go")) != "" || r.State() != "" {
		t.Fatal("nil run must be a no-op")
	}
	if tr, err := r.FinishTx(context.Background(), nil, FinalVerified, ""); tr != nil || err != nil {
		t.Fatal("nil run FinishTx must be a no-op")
	}
}

func TestRunConcurrentObserveSequencesEvents(t *testing.T) {
	rec := &memRecorder{}
	r := newTestRun(rec)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Observe(context.Background(), Event{Kind: EventTool, Tool: "read_file", Path: "x.go"})
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, row := range rec.rows {
		if seen[row.Seq] {
			t.Fatalf("duplicate seq %d", row.Seq)
		}
		seen[row.Seq] = true
	}
	if len(seen) != 50 {
		t.Fatalf("got %d rows", len(seen))
	}
}
