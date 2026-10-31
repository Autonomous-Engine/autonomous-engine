package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
	"github.com/autonomousengine/autonomous-engine/internal/plan"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier is the read side of *pgxpool.Pool (or a pgx.Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Limits on what is copied out of the run event stream.
const (
	maxRunCommands  = 40
	maxWrittenPaths = 50
	maxTransitions  = 30
	maxRunEvents    = 5000
	maxCommandLen   = 300
)

// Requirements are the criteria and task specs of a work item.
type Requirements struct {
	Criteria  []plan.Criterion
	TaskSpecs []TaskSpec
}

// planTaskMessage is the part of plan_tasks.message (an
// implementation.tasks payload) the verifier reads: the structured spec
// and the cited criteria. Title, description and the brief prose are not
// decoded.
type planTaskMessage struct {
	TaskSpec *plan.TaskSpec   `json:"task_spec"`
	Criteria []plan.Criterion `json:"criteria"`
	// NonCodeCriteria are the plan's non-code criteria (R3), marked; read
	// before the brief so the marked copy wins.
	NonCodeCriteria []plan.Criterion `json:"non_code_criteria"`
}

// LoadRequirements reads the work item's criteria and task specs: every
// subtask's plan_tasks.message (task_spec and criteria) in plan order,
// then the latest technical_brief artifact's criteria for any id no task
// carried. Criteria are deduplicated by id (first seen wins); an entry
// without a well-formed id is dropped (logged). A missing source is not an
// error: a legacy item simply has no criteria.
func LoadRequirements(ctx context.Context, q Querier, workID uuid.UUID) (Requirements, error) {
	rb := requirementsBuilder{workID: workID.String(), seen: map[string]bool{}}

	rows, err := q.Query(ctx, `SELECT id::text, message FROM plan_tasks
		WHERE parent_work_id = $1 OR id = $1 ORDER BY created_at, id`, workID)
	if err != nil {
		return rb.req, fmt.Errorf("read plan tasks: %w", err)
	}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return rb.req, fmt.Errorf("scan plan task: %w", err)
		}
		rb.addPlanTask(id, raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rb.req, fmt.Errorf("read plan tasks: %w", err)
	}

	var raw []byte
	err = q.QueryRow(ctx, `SELECT content FROM work_item_artifacts
		WHERE work_item_id = $1 AND artifact_type = 'technical_brief' AND content->'criteria' IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`, workID).Scan(&raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rb.req, fmt.Errorf("read technical brief: %w", err)
	default:
		rb.addBrief(raw)
	}
	return rb.req, nil
}

// LoadRequirementsForTask reads the requirements one subtask is verified
// against (R14, the verification stage verifies each subtask's head before
// its PR): the subtask's own plan_tasks.message (its task_spec, the
// criteria it cites and the plan's non-code criteria), then the parent's
// technical_brief criteria that no plan task carries, so a criterion every
// step silently dropped is still judged. Criteria owned only by a sibling
// subtask are left to that sibling's verification. A task with no
// plan_tasks row (a direct item, a review-fix subtask) falls back to the
// parent's whole requirements (LoadRequirements).
func LoadRequirementsForTask(ctx context.Context, q Querier, workID, taskID uuid.UUID) (Requirements, error) {
	if taskID == uuid.Nil || taskID == workID {
		return LoadRequirements(ctx, q, workID)
	}
	rb := requirementsBuilder{workID: workID.String(), seen: map[string]bool{}}
	carried := map[string]bool{}
	found := false

	rows, err := q.Query(ctx, `SELECT id::text, message FROM plan_tasks
		WHERE parent_work_id = $1 OR id = $1 OR id = $2 ORDER BY created_at, id`, workID, taskID)
	if err != nil {
		return rb.req, fmt.Errorf("read plan tasks: %w", err)
	}
	type planRow struct {
		id  string
		raw []byte
	}
	var own []planRow
	for rows.Next() {
		var r planRow
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return rb.req, fmt.Errorf("scan plan task: %w", err)
		}
		var m planTaskMessage
		if json.Unmarshal(r.raw, &m) == nil {
			for _, c := range m.Criteria {
				carried[c.ID] = true
			}
			for _, c := range m.NonCodeCriteria {
				carried[c.ID] = true
			}
		}
		if r.id == taskID.String() {
			found = true
			own = append(own, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rb.req, fmt.Errorf("read plan tasks: %w", err)
	}
	if !found {
		return LoadRequirements(ctx, q, workID)
	}
	for _, r := range own {
		rb.addPlanTask(r.id, r.raw)
	}

	var raw []byte
	err = q.QueryRow(ctx, `SELECT content FROM work_item_artifacts
		WHERE work_item_id = $1 AND artifact_type = 'technical_brief' AND content->'criteria' IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`, workID).Scan(&raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rb.req, fmt.Errorf("read technical brief: %w", err)
	default:
		rb.addBriefExcept(raw, carried)
	}
	return rb.req, nil
}

// LoadForTask reads one subtask's requirements (LoadRequirementsForTask)
// and its own recorded evidence.
func LoadForTask(ctx context.Context, q Querier, workID, taskID uuid.UUID) (Requirements, Evidence, error) {
	req, err := LoadRequirementsForTask(ctx, q, workID, taskID)
	if err != nil {
		return req, Evidence{}, fmt.Errorf("load requirements: %w", err)
	}
	evFor := taskID
	if evFor == uuid.Nil {
		evFor = workID
	}
	ev, err := LoadEvidence(ctx, q, evFor)
	if err != nil {
		return req, ev, fmt.Errorf("load evidence: %w", err)
	}
	return req, ev, nil
}

// requirementsBuilder accumulates criteria (deduplicated by id, first seen
// wins) and task specs from the stored documents. It decodes only the
// structured fields; the prose next to them is never read.
type requirementsBuilder struct {
	workID string
	seen   map[string]bool
	req    Requirements
}

func (rb *requirementsBuilder) addCriteria(cs []plan.Criterion, from string) {
	for _, c := range cs {
		if !plan.ValidCriterionID(c.ID) || strings.TrimSpace(c.Text) == "" {
			log.Printf("verifier: work_id=%s: ignoring criterion %q from %s (no valid id or text)", rb.workID, c.ID, from)
			continue
		}
		if rb.seen[c.ID] {
			continue
		}
		rb.seen[c.ID] = true
		rb.req.Criteria = append(rb.req.Criteria, c)
	}
}

// addPlanTask reads one plan_tasks.message (an implementation.tasks
// payload): its task_spec and criteria.
func (rb *requirementsBuilder) addPlanTask(id string, raw []byte) {
	var m planTaskMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Printf("verifier: work_id=%s: plan task %s message unreadable: %v", rb.workID, id, err)
		return
	}
	if m.TaskSpec != nil {
		rb.req.TaskSpecs = append(rb.req.TaskSpecs, TaskSpec{TaskID: id, Spec: *m.TaskSpec})
	}
	// R3: only non_code_criteria (the planner's) may carry a non-code mark.
	cited := plan.BriefCriteria{Criteria: m.Criteria}
	plan.StripNonCode(&cited)
	rb.addCriteria(cited.Criteria, "plan task "+id)
	var nonCode []plan.Criterion
	for _, c := range m.NonCodeCriteria {
		if c.NonCode {
			nonCode = append(nonCode, c)
		}
	}
	rb.addCriteria(nonCode, "plan task "+id+" (non-code)")
}

// addBriefExcept reads a technical_brief artifact's criteria, skipping the
// ids in except (criteria some plan task carries).
func (rb *requirementsBuilder) addBriefExcept(raw []byte, except map[string]bool) {
	var brief struct {
		Criteria *plan.BriefCriteria `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &brief); err != nil {
		log.Printf("verifier: work_id=%s: technical brief criteria unreadable: %v", rb.workID, err)
		return
	}
	if brief.Criteria == nil {
		return
	}
	plan.StripNonCode(brief.Criteria)
	var rest []plan.Criterion
	for _, c := range brief.Criteria.Criteria {
		if !except[c.ID] {
			rest = append(rest, c)
		}
	}
	rb.addCriteria(rest, "technical brief")
}

// addBrief reads a technical_brief artifact's criteria object.
func (rb *requirementsBuilder) addBrief(raw []byte) {
	var brief struct {
		Criteria *plan.BriefCriteria `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &brief); err != nil {
		log.Printf("verifier: work_id=%s: technical brief criteria unreadable: %v", rb.workID, err)
		return
	}
	if brief.Criteria != nil {
		plan.StripNonCode(brief.Criteria)
		rb.addCriteria(brief.Criteria.Criteria, "technical brief")
	}
}

// LoadEvidence reads, for the work item and its subtasks, the latest
// verification record per task (implementation_verifications) and the
// latest implementation run per task with its events summarised
// (implementation_runs, implementation_run_events). Only structured fields
// are copied: free-text note events (failure records and repair notes of
// later tickets among them) are counted, never quoted, and run detail JSON
// is not read.
func LoadEvidence(ctx context.Context, q Querier, workID uuid.UUID) (Evidence, error) {
	var ev Evidence
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (task_id) task_id, outcome, base_sha, head_sha, evidence
		FROM implementation_verifications
		WHERE work_item_id = $1 OR work_item_id IN (SELECT id FROM work_items WHERE parent_id = $1)
		ORDER BY task_id, created_at DESC`, workID)
	if err != nil {
		return ev, fmt.Errorf("read verification records: %w", err)
	}
	for rows.Next() {
		var rec EvidenceRecord
		var outcome string
		var raw []byte
		if err := rows.Scan(&rec.TaskID, &outcome, &rec.BaseSHA, &rec.HeadSHA, &raw); err != nil {
			rows.Close()
			return ev, fmt.Errorf("scan verification record: %w", err)
		}
		rec.Outcome = evidence.Outcome(outcome)
		var doc evidence.ImplementationEvidence
		if json.Unmarshal(raw, &doc) == nil {
			rec.ChangedFiles = doc.ChangedFiles
			rec.Flags = doc.Flags
			for _, c := range doc.Checks {
				rec.Checks = append(rec.Checks, CheckSummary{Kind: c.Kind, Command: capString(c.Command, maxCommandLen), ExitCode: c.ExitCode, Status: c.Status, SkipReason: c.SkipReason})
			}
		}
		ev.Records = append(ev.Records, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ev, fmt.Errorf("read verification records: %w", err)
	}

	type run struct {
		id string
		RunSummary
	}
	var runs []run
	rrows, err := q.Query(ctx, `SELECT DISTINCT ON (task_id) id::text, task_id, attempt, state
		FROM implementation_runs
		WHERE work_item_id = $1 OR work_item_id IN (SELECT id FROM work_items WHERE parent_id = $1)
		ORDER BY task_id, attempt DESC`, workID)
	if err != nil {
		return ev, fmt.Errorf("read implementation runs: %w", err)
	}
	for rrows.Next() {
		var r run
		if err := rrows.Scan(&r.id, &r.TaskID, &r.Attempt, &r.State); err != nil {
			rrows.Close()
			return ev, fmt.Errorf("scan implementation run: %w", err)
		}
		runs = append(runs, r)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return ev, fmt.Errorf("read implementation runs: %w", err)
	}
	for _, r := range runs {
		s, err := summariseRun(ctx, q, r.id, r.RunSummary)
		if err != nil {
			return ev, err
		}
		ev.Runs = append(ev.Runs, s)
	}
	return ev, nil
}

// runEvent is one implementation_run_events row.
type runEvent struct {
	Seq      int64
	Kind     string
	CallID   string
	Tool     string
	ExitCode *int
	Path     string
	Command  string
	From, To string
	Note     string
}

func summariseRun(ctx context.Context, q Querier, runID string, s RunSummary) (RunSummary, error) {
	rows, err := q.Query(ctx, `SELECT seq, kind, COALESCE(tool_call_id, ''), COALESCE(tool, ''), exit_code,
			COALESCE(path, ''), COALESCE(command, ''), COALESCE(from_state, ''), COALESCE(to_state, ''), COALESCE(note, '')
		FROM implementation_run_events WHERE run_id = $1 ORDER BY seq LIMIT $2`, runID, maxRunEvents)
	if err != nil {
		return s, fmt.Errorf("read run events: %w", err)
	}
	var events []runEvent
	for rows.Next() {
		var e runEvent
		if err := rows.Scan(&e.Seq, &e.Kind, &e.CallID, &e.Tool, &e.ExitCode, &e.Path, &e.Command, &e.From, &e.To, &e.Note); err != nil {
			rows.Close()
			return s, fmt.Errorf("scan run event: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("read run events: %w", err)
	}
	return summariseEvents(s, events), nil
}

// summariseEvents folds a run's events into s.
func summariseEvents(s RunSummary, events []runEvent) RunSummary {
	written := map[string]bool{}
	for _, e := range events {
		switch e.Kind {
		case "tool":
			if e.Tool == "" {
				continue
			}
			if s.ToolCounts == nil {
				s.ToolCounts = map[string]int{}
			}
			s.ToolCounts[e.Tool]++
			if e.Tool == "run_command" && e.Command != "" {
				s.Commands = append(s.Commands, RunCommand{Seq: e.Seq, CallID: e.CallID, Command: capString(e.Command, maxCommandLen), ExitCode: e.ExitCode})
			}
			if (e.Tool == "write_file" || e.Tool == "edit_file" || e.Tool == "insert_after" || e.Tool == "append_to_file") && e.Path != "" && !written[e.Path] && len(s.WrittenPaths) < maxWrittenPaths {
				written[e.Path] = true
				s.WrittenPaths = append(s.WrittenPaths, e.Path)
			}
		case "transition":
			s.Transitions = append(s.Transitions, RunTransition{Seq: e.Seq, From: e.From, To: e.To, Why: capString(e.Note, maxCommandLen)})
		default:
			// "note" and any kind a later ticket adds: counted, not copied.
			s.NoteEvents++
		}
	}
	if len(s.Commands) > maxRunCommands {
		s.Commands = s.Commands[len(s.Commands)-maxRunCommands:]
	}
	if len(s.Transitions) > maxTransitions {
		s.Transitions = s.Transitions[len(s.Transitions)-maxTransitions:]
	}
	return s
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
