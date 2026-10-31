package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Event row kinds (implementation_run_events.kind).
const (
	KindTool       = "tool"
	KindTransition = "transition"
	KindNote       = "note"
)

// EventRow is one implementation_run_events row.
type EventRow struct {
	Seq        int64  `json:"seq"`
	Kind       string `json:"kind"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Tool       string `json:"tool,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Path       string `json:"path,omitempty"`
	Command    string `json:"command,omitempty"`
	FromState  State  `json:"from_state,omitempty"`
	ToState    State  `json:"to_state,omitempty"`
	Note       string `json:"note,omitempty"`
	// Detail is structured event data (migration 054): the failure
	// records of a pre-commit check run, a plan revision and its evidence
	// (M3-06). Nil for every other event; only written when set, so
	// events without it need no migration 054.
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// EvidencePointer locates the tool call that proved red or green.
type EvidencePointer struct {
	Seq      int64  `json:"seq"`
	CallID   string `json:"call_id"`
	Command  string `json:"command,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// RunRef names another run of the same task.
type RunRef struct {
	ID      string `json:"id"`
	Attempt int    `json:"attempt"`
	State   State  `json:"state"`
}

// FinalRecord is the outcome that ended the run.
type FinalRecord struct {
	Outcome FinalOutcome `json:"outcome"`
	Note    string       `json:"note,omitempty"`
}

// Detail is implementation_runs.detail. Writes merge it into the stored
// document (jsonb ||), so keys other tickets add (M3-06 budgets, M3-07
// failure fingerprints) survive. Red and Green are always written: null
// clears a green pointer after a regression.
type Detail struct {
	Red       *EvidencePointer `json:"red"`
	Green     *EvidencePointer `json:"green"`
	TestFiles []string         `json:"test_files,omitempty"`
	Previous  *RunRef          `json:"previous,omitempty"`
	Final     *FinalRecord     `json:"final,omitempty"`
}

// DB is the pool surface the store uses (*pgxpool.Pool satisfies it).
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// execer is what one write needs: a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store persists runs and their events (migration 052).
type Store struct{ db DB }

// NewStore returns a store over db.
func NewStore(db DB) *Store { return &Store{db: db} }

// Started is a newly created run.
type Started struct {
	ID       uuid.UUID
	Attempt  int
	Previous *RunRef
	// NextSeq is the first free event seq of the new run.
	NextSeq int64
}

// StartRun creates attempt N+1 for taskID, where N is the highest existing
// attempt (0 when none). When the previous attempt never reached a
// terminal state — the agent died or the task was redelivered mid-run — a
// note is appended to it naming the attempt that supersedes it; its state
// is left as its tools last showed. The new run's first event is a note
// naming the previous attempt and its state.
func (s *Store) StartRun(ctx context.Context, workItemID uuid.UUID, taskID string) (Started, error) {
	var out Started
	for try := 0; try < 3; try++ {
		err := withTx(ctx, s.db, func(tx pgx.Tx) error {
			var prev *RunRef
			var pid uuid.UUID
			var pattempt int
			var pstate string
			var pfinished bool
			err := tx.QueryRow(ctx, `SELECT id, attempt, state, detail ? 'final' FROM implementation_runs
				WHERE task_id = $1 ORDER BY attempt DESC LIMIT 1 FOR UPDATE`, taskID).Scan(&pid, &pattempt, &pstate, &pfinished)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return fmt.Errorf("previous run: %w", err)
			default:
				prev = &RunRef{ID: pid.String(), Attempt: pattempt, State: State(pstate)}
			}
			attempt := 1
			if prev != nil {
				attempt = prev.Attempt + 1
			}
			detail := Detail{Previous: prev}
			dj, err := json.Marshal(detail)
			if err != nil {
				return err
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO implementation_runs (work_item_id, task_id, attempt, state, detail)
				VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING id`,
				workItemID, taskID, attempt, string(StateInspect), string(dj)).Scan(&id); err != nil {
				return fmt.Errorf("insert run: %w", err)
			}
			note := fmt.Sprintf("attempt %d started", attempt)
			if prev != nil {
				note = fmt.Sprintf("attempt %d started; previous attempt %d (run %s) ended in state %s", attempt, prev.Attempt, prev.ID, prev.State)
				// Open = no terminal state and no recorded outcome (a run that
				// finished unverified keeps its last tool state but has
				// detail.final, so it is not open).
				if !prev.State.Terminal() && !pfinished {
					note += " without reaching a terminal state; the model conversation is not resumed"
					// The previous run may still be live in another process
					// (its seqs are allocated in memory): take the next seq
					// after what it has actually written.
					var prevSeq int64
					if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM implementation_run_events WHERE run_id = $1`, pid).Scan(&prevSeq); err != nil {
						return fmt.Errorf("previous run seq: %w", err)
					}
					if err := writeEvents(ctx, tx, pid, []EventRow{{
						Seq: prevSeq + 1, Kind: KindNote,
						Note: fmt.Sprintf("superseded by attempt %d (run %s): task redelivered while this run was open", attempt, id),
					}}, nil, nil); err != nil {
						return err
					}
				}
			}
			if err := writeEvents(ctx, tx, id, []EventRow{{Seq: 1, Kind: KindNote, Note: note}}, nil, nil); err != nil {
				return err
			}
			out = Started{ID: id, Attempt: attempt, Previous: prev, NextSeq: 2}
			return nil
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue // a concurrent delivery took the attempt number; recompute
		}
		return out, err
	}
	return out, fmt.Errorf("start run for task %s: attempt number contended", taskID)
}

// AppendEvent writes one event row (pooled; telemetry-grade).
func (s *Store) AppendEvent(ctx context.Context, runID uuid.UUID, row EventRow) error {
	return writeEvents(ctx, s.db, runID, []EventRow{row}, nil, nil)
}

// RecordTransition writes rows and moves the run to `to` with detail, in
// one short transaction.
func (s *Store) RecordTransition(ctx context.Context, runID uuid.UUID, rows []EventRow, to State, detail Detail) error {
	return withTx(ctx, s.db, func(tx pgx.Tx) error {
		return writeEvents(ctx, tx, runID, rows, &to, &detail)
	})
}

// RecordTx writes rows and, when to is set, the run's new state and
// detail, inside the caller's transaction (the agent's outcome tx).
func (s *Store) RecordTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, rows []EventRow, to *State, detail *Detail) error {
	return writeEvents(ctx, tx, runID, rows, to, detail)
}

func writeEvents(ctx context.Context, ex execer, runID uuid.UUID, rows []EventRow, to *State, detail *Detail) error {
	var maxSeq int64
	for _, r := range rows {
		args := []any{runID, r.Seq, r.Kind, r.ToolCallID, r.Tool, r.ExitCode, r.Path, r.Command,
			string(r.FromState), string(r.ToState), r.Note}
		cols, vals := "", ""
		if len(r.Detail) > 0 {
			// Only events that carry detail touch the column (migration 054).
			cols, vals = ", detail", ", $12::jsonb"
			args = append(args, string(r.Detail))
		}
		if _, err := ex.Exec(ctx, `INSERT INTO implementation_run_events
			(run_id, seq, kind, tool_call_id, tool, exit_code, path, command, from_state, to_state, note`+cols+`)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, '')`+vals+`)`,
			args...); err != nil {
			return fmt.Errorf("insert run event seq=%d: %w", r.Seq, err)
		}
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
	}
	if to != nil {
		var dj []byte
		if detail != nil {
			b, err := json.Marshal(detail)
			if err != nil {
				return err
			}
			dj = b
		} else {
			dj = []byte("{}")
		}
		_, err := ex.Exec(ctx, `UPDATE implementation_runs
			SET state = $2, detail = detail || $3::jsonb, last_event_seq = GREATEST(last_event_seq, $4), updated_at = NOW()
			WHERE id = $1`, runID, string(*to), string(dj), maxSeq)
		if err != nil {
			return fmt.Errorf("update run state: %w", err)
		}
		return nil
	}
	if len(rows) > 0 {
		if _, err := ex.Exec(ctx, `UPDATE implementation_runs
			SET last_event_seq = GREATEST(last_event_seq, $2), updated_at = NOW() WHERE id = $1`, runID, maxSeq); err != nil {
			return fmt.Errorf("update run seq: %w", err)
		}
	}
	return nil
}

// MergeDetail merges patch (a JSON object) into the run's detail at the
// top level (jsonb ||): the keys it names are replaced, every other key —
// red/green, final, other tickets' keys — is kept. M3-06 writes "repair"
// and "budget" through it.
func (s *Store) MergeDetail(ctx context.Context, runID uuid.UUID, patch json.RawMessage) error {
	_, err := s.db.Exec(ctx, `UPDATE implementation_runs SET detail = detail || $2::jsonb, updated_at = NOW() WHERE id = $1`,
		runID, string(patch))
	return err
}

// Events returns a run's events in seq order.
func (s *Store) Events(ctx context.Context, runID uuid.UUID) ([]EventRow, error) {
	rows, err := s.db.Query(ctx, `SELECT seq, kind, COALESCE(tool_call_id, ''), COALESCE(tool, ''), exit_code,
		COALESCE(path, ''), COALESCE(command, ''), COALESCE(from_state, ''), COALESCE(to_state, ''), COALESCE(note, ''), created_at,
		detail::text
		FROM implementation_run_events WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var r EventRow
		var from, to string
		var detail *string
		if err := rows.Scan(&r.Seq, &r.Kind, &r.ToolCallID, &r.Tool, &r.ExitCode, &r.Path, &r.Command, &from, &to, &r.Note, &r.CreatedAt, &detail); err != nil {
			return nil, err
		}
		r.FromState, r.ToState = State(from), State(to)
		if detail != nil {
			r.Detail = json.RawMessage(*detail)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TransitionEvidence is the transition row that recorded red or green.
type TransitionEvidence struct {
	Seq        int64     `json:"seq"`
	ToolCallID string    `json:"tool_call_id"`
	Tool       string    `json:"tool"`
	Command    string    `json:"command,omitempty"`
	ExitCode   *int      `json:"exit_code"`
	At         time.Time `json:"at"`
}

// RunSummary is the latest run of one task, for the dashboard. State and
// the red/green evidence are read from the rows as recorded; nothing is
// inferred.
type RunSummary struct {
	ID           string              `json:"id"`
	WorkItemID   string              `json:"work_item_id"`
	TaskID       string              `json:"task_id"`
	Attempt      int                 `json:"attempt"`
	State        State               `json:"state"`
	StartedAt    time.Time           `json:"started_at"`
	UpdatedAt    time.Time           `json:"updated_at"`
	LastEventSeq int64               `json:"last_event_seq"`
	Red          *TransitionEvidence `json:"red"`
	Green        *TransitionEvidence `json:"green"`
	// Repair and Budget are detail.repair / detail.budget as the agent
	// recorded them (M3-06): fingerprint counts, repair attempts, plan
	// revisions and the stop reason; the D4 token and active-time budget.
	// Absent for runs without them.
	Repair json.RawMessage `json:"repair,omitempty"`
	Budget json.RawMessage `json:"budget,omitempty"`
}

// LatestRuns returns the latest attempt of every task whose run belongs to
// id (a parent work item) or whose task is id (a subtask), ordered by
// start time, each with the newest red_observed transition and the
// green_observed transition that still holds (detail.green; nil after a
// regression) of that attempt.
func (s *Store) LatestRuns(ctx context.Context, id uuid.UUID) ([]RunSummary, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM (
			SELECT DISTINCT ON (task_id) id, work_item_id, task_id, attempt, state, started_at, updated_at, last_event_seq,
				(detail->'repair')::text AS repair, (detail->'budget')::text AS budget
			FROM implementation_runs WHERE work_item_id = $1 OR task_id = $2
			ORDER BY task_id, attempt DESC) latest
		ORDER BY started_at`, id, id.String())
	if err != nil {
		return nil, err
	}
	var out []RunSummary
	index := map[uuid.UUID]int{}
	var ids []uuid.UUID
	for rows.Next() {
		var r RunSummary
		var rid, wid uuid.UUID
		var state string
		var repair, budget *string
		if err := rows.Scan(&rid, &wid, &r.TaskID, &r.Attempt, &state, &r.StartedAt, &r.UpdatedAt, &r.LastEventSeq, &repair, &budget); err != nil {
			rows.Close()
			return nil, err
		}
		if repair != nil {
			r.Repair = json.RawMessage(*repair)
		}
		if budget != nil {
			r.Budget = json.RawMessage(*budget)
		}
		r.ID, r.WorkItemID, r.State = rid.String(), wid.String(), State(state)
		index[rid] = len(out)
		ids = append(ids, rid)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []RunSummary{}, nil
	}
	// Green is the transition detail.green points at: the pointer is
	// cleared when a later failing run or write regresses the run, so a
	// stale green is never shown. Red is the newest red_observed.
	ev, err := s.db.Query(ctx, `SELECT DISTINCT ON (e.run_id, e.to_state) e.run_id, e.to_state, e.seq,
			COALESCE(e.tool_call_id, ''), COALESCE(e.tool, ''), COALESCE(e.command, ''), e.exit_code, e.created_at
		FROM implementation_run_events e
		JOIN implementation_runs r ON r.id = e.run_id
		WHERE e.run_id = ANY($1) AND e.kind = 'transition'
		  AND (e.to_state = 'red_observed'
		       OR (e.to_state = 'green_observed' AND e.seq = (r.detail->'green'->>'seq')::bigint))
		ORDER BY e.run_id, e.to_state, e.seq DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer ev.Close()
	for ev.Next() {
		var rid uuid.UUID
		var to string
		var t TransitionEvidence
		if err := ev.Scan(&rid, &to, &t.Seq, &t.ToolCallID, &t.Tool, &t.Command, &t.ExitCode, &t.At); err != nil {
			return nil, err
		}
		i, ok := index[rid]
		if !ok {
			continue
		}
		tt := t
		if State(to) == StateRedObserved {
			out[i].Red = &tt
		} else {
			out[i].Green = &tt
		}
	}
	return out, ev.Err()
}

// withTx runs fn in a transaction on db.
func withTx(ctx context.Context, db DB, fn func(pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
