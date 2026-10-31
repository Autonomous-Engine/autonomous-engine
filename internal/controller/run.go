package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Recorder persists a run's events. *Store satisfies it; tests record in
// memory.
type Recorder interface {
	// AppendEvent writes one event row on its own.
	AppendEvent(ctx context.Context, runID uuid.UUID, row EventRow) error
	// RecordTransition writes rows and the run's new state and detail in
	// one transaction.
	RecordTransition(ctx context.Context, runID uuid.UUID, rows []EventRow, to State, detail Detail) error
	// RecordTx writes rows (and the state, when to is set) in the caller's
	// transaction.
	RecordTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, rows []EventRow, to *State, detail *Detail) error
}

// Run is one live implementation attempt. Observe is safe for concurrent
// use (the executor runs read-only tool batches in parallel): events are
// derived and sequenced one at a time. Every method is a no-op on a nil
// *Run, so an agent whose run could not be created keeps working without
// a controller.
type Run struct {
	ID         uuid.UUID
	WorkItemID uuid.UUID
	TaskID     string
	Attempt    int

	// OnTransition, when set, is called (under the run's lock) after each
	// state change has been handed to the recorder. The agent logs
	// Transition.LogLine from it.
	OnTransition func(Transition)
	// Logf reports persistence failures. Persistence is telemetry-grade:
	// a failed write is logged and the run continues.
	Logf func(format string, args ...any)

	mu     sync.Mutex
	rec    Recorder
	state  State
	rc     RunContext
	seq    int64
	detail Detail
}

// NewRun returns the live run for a started row. nextSeq is the first free
// event seq (Started.NextSeq).
func NewRun(rec Recorder, s Started, workItemID uuid.UUID, taskID string) *Run {
	return &Run{
		ID: s.ID, WorkItemID: workItemID, TaskID: taskID, Attempt: s.Attempt,
		rec: rec, state: StateInspect, seq: s.NextSeq - 1,
		detail: Detail{Previous: s.Previous},
	}
}

// SetTestCommand records the repository's detected test command, which
// IsTestCommand accepts in addition to the known runners.
func (r *Run) SetTestCommand(cmd string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.rc.TestCommand = cmd
	r.mu.Unlock()
}

// State is the run's current state ("" for a nil run).
func (r *Run) State() State {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Detail returns a copy of the run's detail.
func (r *Run) Detail() Detail {
	if r == nil {
		return Detail{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.detail
	d.TestFiles = append([]string(nil), d.TestFiles...)
	return d
}

// Observe derives ev, records it (a tool row, plus a transition row and
// the new state when it advanced) and returns the state after it.
func (r *Run) Observe(ctx context.Context, ev Event) State {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := r.state
	next, advanced, why := Derive(prev, ev, r.rc)
	r.seq++
	toolRow := EventRow{
		Seq: r.seq, Kind: KindTool, ToolCallID: ev.CallID, Tool: ev.Tool,
		ExitCode: ev.ExitCode, Path: normalizePath(ev.Path), Command: ev.Command,
	}
	if !advanced {
		toolRow.Note = why
	}
	r.rc = r.rc.Observe(ev, next)
	if !advanced {
		if err := r.rec.AppendEvent(ctx, r.ID, toolRow); err != nil {
			r.logf("controller: run %s: record tool event seq=%d: %v", r.ID, toolRow.Seq, err)
		}
		return prev
	}

	r.seq++
	tr := Transition{From: prev, To: next, Why: why, Event: ev, Seq: r.seq}
	r.applyDetail(tr)
	trRow := transitionRow(tr)
	if err := r.rec.RecordTransition(ctx, r.ID, []EventRow{toolRow, trRow}, next, r.detail); err != nil {
		r.logf("controller: run %s: record transition %s→%s: %v", r.ID, prev, next, err)
	}
	r.state = next
	if r.OnTransition != nil {
		r.OnTransition(tr)
	}
	return next
}

// Note appends a note event.
func (r *Run) Note(ctx context.Context, text string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	if err := r.rec.AppendEvent(ctx, r.ID, EventRow{Seq: r.seq, Kind: KindNote, Note: text}); err != nil {
		r.logf("controller: run %s: record note: %v", r.ID, err)
	}
}

// Append writes row as the run's next event without deriving anything
// from it — for events the agent itself produces (a pre-commit check run
// and its failure records, a plan revision; M3-06), which are evidence
// for the operator but never move the lifecycle state. It returns the
// row's seq (0 for a nil run).
func (r *Run) Append(ctx context.Context, row EventRow) int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	row.Seq = r.seq
	if err := r.rec.AppendEvent(ctx, r.ID, row); err != nil {
		r.logf("controller: run %s: record %s event seq=%d: %v", r.ID, row.Kind, row.Seq, err)
	}
	return row.Seq
}

// DetailMerger is implemented by recorders that can merge keys into a
// run's detail (*Store). Recorders without it drop merges.
type DetailMerger interface {
	MergeDetail(ctx context.Context, runID uuid.UUID, patch json.RawMessage) error
}

// MergeDetail sets detail[key] = value in the stored run (other keys are
// kept). Telemetry-grade: a failure is logged.
func (r *Run) MergeDetail(ctx context.Context, key string, value any) {
	if r == nil {
		return
	}
	m, ok := r.rec.(DetailMerger)
	if !ok {
		return
	}
	b, err := json.Marshal(map[string]any{key: value})
	if err == nil {
		err = m.MergeDetail(ctx, r.ID, b)
	}
	if err != nil {
		r.logf("controller: run %s: merge detail %s: %v", r.ID, key, err)
	}
}

// FinishTx derives the run's terminal event from outcome and writes it in
// tx — the transaction that commits the agent's outcome — so the run's
// final state commits (or rolls back) with it. The in-memory state is not
// changed: if tx rolls back, a fallback outcome may call FinishTx again
// and derives from the same state.
// It returns the transition when the state changed.
func (r *Run) FinishTx(ctx context.Context, tx pgx.Tx, outcome FinalOutcome, note string) (*Transition, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	ev := Event{Kind: EventFinal, Outcome: outcome}
	next, advanced, why := Derive(r.state, ev, r.rc)
	seq := r.seq + 1
	text := string(outcome)
	if note != "" {
		text += ": " + note
	}
	if !advanced && why != "" {
		text += " (" + why + ")"
	}
	rows := []EventRow{{Seq: seq, Kind: KindNote, Note: "outcome " + text}}
	detail := r.detail
	detail.Final = &FinalRecord{Outcome: outcome, Note: note}
	// Seqs used here are consumed even if tx rolls back (a gap, never a
	// duplicate key for a fallback outcome or a later note).
	r.seq = seq
	if !advanced {
		return nil, r.rec.RecordTx(ctx, tx, r.ID, rows, &r.state, &detail)
	}
	r.seq++
	tr := Transition{From: r.state, To: next, Why: why, Event: ev, Seq: r.seq}
	rows = append(rows, transitionRow(tr))
	if err := r.rec.RecordTx(ctx, tx, r.ID, rows, &next, &detail); err != nil {
		return nil, err
	}
	return &tr, nil
}

func (r *Run) applyDetail(tr Transition) {
	ptr := func() *EvidencePointer {
		return &EvidencePointer{Seq: tr.Seq, CallID: tr.Event.CallID, Command: tr.Event.Command, ExitCode: tr.Event.ExitCode}
	}
	switch tr.To {
	case StateRedObserved:
		r.detail.Red = ptr()
	case StateGreenObserved:
		r.detail.Green = ptr()
	case StateImplement:
		if tr.From == StateGreenObserved || tr.From == StateDiffReviewed {
			r.detail.Green = nil // regression: the green no longer holds
		}
	}
	r.detail.TestFiles = append([]string(nil), r.rc.TestFiles...)
}

func transitionRow(tr Transition) EventRow {
	return EventRow{
		Seq: tr.Seq, Kind: KindTransition, ToolCallID: tr.Event.CallID, Tool: tr.Event.Tool,
		ExitCode: tr.Event.ExitCode, Path: normalizePath(tr.Event.Path), Command: tr.Event.Command,
		FromState: tr.From, ToState: tr.To, Note: tr.Why,
	}
}

func (r *Run) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
		return
	}
	fmt.Printf(format+"\n", args...)
}
