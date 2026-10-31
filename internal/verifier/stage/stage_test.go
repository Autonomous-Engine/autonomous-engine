package stage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type stmt struct {
	sql  string
	args []any
}

// fakeTx records every statement; the real workgraph/guards/outbox writers
// run against it.
type fakeTx struct {
	pgx.Tx
	state      models.WorkItemState
	failOn     string
	stmts      []stmt
	committed  bool
	rolledBack bool
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.stmts = append(f.stmts, stmt{sql, args})
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return pgconn.CommandTag{}, errors.New("injected failure on " + f.failOn)
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.stmts = append(f.stmts, stmt{sql, args})
	return stateRow{f.state}
}

func (f *fakeTx) Commit(context.Context) error   { f.committed = true; return nil }
func (f *fakeTx) Rollback(context.Context) error { f.rolledBack = true; return nil }

type stateRow struct{ state models.WorkItemState }

func (r stateRow) Scan(dest ...any) error {
	switch d := dest[0].(type) {
	case *bool: // workgraph.SupersededTx: a live subtask
		*d = false
	default:
		*(dest[0].(*models.WorkItemState)) = r.state
	}
	return nil
}

type fakeDB struct {
	state  models.WorkItemState
	failOn string
	txs    []*fakeTx
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	tx := &fakeTx{state: d.state, failOn: d.failOn}
	if tx.state == "" {
		tx.state = models.WorkItemStateImplementation
	}
	d.txs = append(d.txs, tx)
	return tx, nil
}

func tables(tx *fakeTx) []string {
	var out []string
	for _, s := range tx.stmts {
		switch {
		case strings.Contains(s.sql, "INSERT INTO work_item_artifacts"):
			out = append(out, "artifact")
		case strings.Contains(s.sql, "INSERT INTO verification_results"):
			out = append(out, "result:"+s.args[4].(string))
		case strings.Contains(s.sql, "FOR UPDATE"):
			out = append(out, "lock")
		case strings.Contains(s.sql, "LIKE $2 || '%'"):
			out = append(out, "superseded")
		case strings.Contains(s.sql, "UPDATE work_items SET state"):
			out = append(out, "state:"+string(s.args[0].(models.WorkItemState)))
		case strings.Contains(s.sql, "INSERT INTO work_item_transitions"):
			out = append(out, "transition")
		case strings.Contains(s.sql, "INSERT INTO outbox_events"):
			out = append(out, "outbox:"+s.args[3].(string))
		case strings.Contains(s.sql, "INSERT INTO agent_assignments"):
			out = append(out, "assign")
		case strings.Contains(s.sql, "UPDATE agent_assignments"):
			out = append(out, "assignment:"+string(s.args[0].(models.AssignmentStatus)))
		default:
			out = append(out, "other:"+strings.TrimSpace(s.sql))
		}
	}
	return out
}

func outboxBody(t *testing.T, tx *fakeTx, topic string) Message {
	t.Helper()
	for _, s := range tx.stmts {
		if strings.Contains(s.sql, "INSERT INTO outbox_events") && s.args[3] == topic {
			var m Message
			if err := json.Unmarshal(s.args[5].([]byte), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no outbox row for %s in %v", topic, tables(tx))
	return Message{}
}

func approveVerdict() verifier.Verdict {
	return verifier.Verdict{
		SchemaVersion: verifier.SchemaVersion,
		PerCriterion:  []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusMet, Evidence: []verifier.Ref{"a.go:1"}, Note: "done\n  well"}},
		Defects:       []verifier.Defect{},
		Overall:       verifier.OverallApprove,
	}
}

func TestDecisionForMapsTheGate(t *testing.T) {
	cases := []struct {
		gate       verifier.Decision
		secondPass bool
		want       Decision
	}{
		{verifier.Decision{Action: verifier.ActionApprove}, true, DecisionApprovePendingSecondPass},
		{verifier.Decision{Action: verifier.ActionApprove}, false, DecisionApprove},
		{verifier.Decision{Action: verifier.ActionApprove, SecondPassRequired: true}, false, DecisionApprovePendingSecondPass},
		{verifier.Decision{Action: verifier.ActionRequestChanges}, true, DecisionRequestChanges},
		{verifier.Decision{Action: verifier.ActionEscalate}, false, DecisionEscalate},
		{verifier.Decision{Action: "bogus"}, false, DecisionEscalate},
	}
	for _, c := range cases {
		if got := DecisionFor(c.gate, c.secondPass); got != c.want {
			t.Errorf("DecisionFor(%+v, %t) = %s, want %s", c.gate, c.secondPass, got, c.want)
		}
	}
	for _, d := range []Decision{DecisionApprove, DecisionApprovePendingSecondPass} {
		if !d.Approves() {
			t.Errorf("%s must approve", d)
		}
	}
	for _, d := range []Decision{DecisionRequestChanges, DecisionEscalate, DecisionNotRun, DecisionError, ""} {
		if d.Approves() {
			t.Errorf("%q must not approve (D1)", d)
		}
	}
}

func TestSecondPassEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "false": false, " OFF ": false, "0": false, "no": false} {
		t.Setenv(SecondPassEnv, v)
		if got := SecondPassEnabled(); got != want {
			t.Errorf("%s=%q → %t, want %t", SecondPassEnv, v, got, want)
		}
	}
}

func TestSummariesAreOneLineAndCapped(t *testing.T) {
	v := approveVerdict()
	v.PerCriterion = append(v.PerCriterion, verifier.CriterionResult{ID: "AC-2", Status: verifier.StatusNotVerifiable, Note: strings.Repeat("x", 400)})
	s := Summaries(v)
	if len(s) != 2 || s[0].Note != "done well" || s[0].Status != "met" {
		t.Fatalf("summaries = %+v", s)
	}
	if n := len([]rune(s[1].Note)); n != maxNote+1 || !strings.HasSuffix(s[1].Note, "…") {
		t.Fatalf("note not capped: %d runes", n)
	}
}

// V1: the verdict artifact, the verification_results row, the
// verification.complete event and the assignment commit in one
// transaction, the message naming the committed ids.
func TestCommitWritesArtifactResultEventAndAssignmentInOneTx(t *testing.T) {
	parent, task, asg := uuid.New(), uuid.New(), uuid.New()
	v := approveVerdict()
	g := verifier.Gate(v)
	content := NewArtifactContent(v, g, string(DecisionApprove), "agent/x", "abc", "abc", task.String())
	o := &Outcome{
		Message:      Message{WorkID: parent.String(), TaskID: task.String(), HeadSHA: "abc", Decision: DecisionApprove, Reason: g.Reason, Criteria: Summaries(v)},
		Result:       Result{WorkID: parent, TaskID: task, HeadSHA: "abc", Decision: DecisionApprove, Reason: g.Reason, VerifierVersion: verifier.Version},
		Artifact:     NewArtifact(parent, "review-agent", content),
		AssignmentID: asg,
	}
	db := &fakeDB{}
	if err := Commit(context.Background(), db, workgraph.New(nil), "review-agent", o); err != nil {
		t.Fatal(err)
	}
	if len(db.txs) != 1 || !db.txs[0].committed {
		t.Fatalf("want one committed tx, got %d", len(db.txs))
	}
	got := strings.Join(tables(db.txs[0]), " ")
	want := "artifact result:approve outbox:" + bus.TopicVerificationComplete + " assignment:completed"
	if got != want {
		t.Fatalf("statements = %s\nwant %s", got, want)
	}
	m := outboxBody(t, db.txs[0], bus.TopicVerificationComplete)
	if m.VerdictArtifactID == "" || m.VerdictArtifactID != o.Artifact.ID.String() {
		t.Errorf("message verdict_artifact_id = %q, want the committed artifact %s", m.VerdictArtifactID, o.Artifact.ID)
	}
	if m.ResultID == "" || m.ResultID != o.Result.ID.String() {
		t.Errorf("message result_id = %q, want %s", m.ResultID, o.Result.ID)
	}
	if o.Result.VerdictArtifactID == nil || *o.Result.VerdictArtifactID != o.Artifact.ID {
		t.Errorf("result does not reference the artifact")
	}
	if m.Timestamp == "" {
		t.Error("message has no timestamp")
	}
}

// decision error: the block (transition + work.blocked) commits with the
// result and the event; the assignment is failed.
func TestCommitErrorBlocksTheParentInTheSameTx(t *testing.T) {
	parent, task := uuid.New(), uuid.New()
	o := &Outcome{
		Message:      Message{WorkID: parent.String(), TaskID: task.String(), Decision: DecisionError, Reason: "head: no head commit to verify"},
		Result:       Result{WorkID: parent, TaskID: task, Decision: DecisionError, Reason: "head: no head commit to verify"},
		Block:        "verification could not run: head: no head commit to verify",
		AssignmentID: uuid.New(),
	}
	db := &fakeDB{}
	if err := Commit(context.Background(), db, workgraph.New(nil), "review-agent", o); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tables(db.txs[0]), " ")
	want := "result:error outbox:verification.complete lock superseded lock state:blocked transition outbox:work.blocked assignment:failed"
	if got != want {
		t.Fatalf("statements = %s\nwant %s", got, want)
	}
}

// Review minor 3: a parent that already ended (cancelled, rejected,
// passed, released) is not blocked; the result, the event and the guard
// (the transaction) still commit.
func TestCommitErrorLeavesATerminalParentAlone(t *testing.T) {
	for _, st := range []models.WorkItemState{models.WorkItemStateCancelled, models.WorkItemStateRejected, models.WorkItemStatePassed, models.WorkItemStateReleased} {
		parent, task := uuid.New(), uuid.New()
		o := &Outcome{
			Message:    Message{WorkID: parent.String(), TaskID: task.String(), Decision: DecisionError, Reason: "x"},
			Result:     Result{WorkID: parent, TaskID: task, Decision: DecisionError, Reason: "x"},
			Block:      "Verification before the PR could not run",
			Assignment: &models.AgentAssignment{WorkItemID: task, AgentType: models.AgentTypeReview, AgentInstanceID: "review-agent/verification", Status: models.AssignmentStatusActive},
		}
		db := &fakeDB{state: st}
		if err := Commit(context.Background(), db, workgraph.New(nil), "review-agent", o); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		got := strings.Join(tables(db.txs[0]), " ")
		if want := "result:error outbox:verification.complete lock assign assignment:failed"; got != want || !db.txs[0].committed {
			t.Fatalf("%s: statements = %s (committed=%t)\nwant %s", st, got, db.txs[0].committed, want)
		}
	}
}

// Review minor 7: the stage's assignment is inserted and completed in the
// verification's transaction, so a rolled-back commit leaves none behind.
func TestCommitRecordsTheAssignmentInTheTransaction(t *testing.T) {
	parent, task := uuid.New(), uuid.New()
	o := &Outcome{
		Message:    Message{WorkID: parent.String(), TaskID: task.String(), Decision: DecisionNotRun},
		Result:     Result{WorkID: parent, TaskID: task, Decision: DecisionNotRun},
		Assignment: &models.AgentAssignment{WorkItemID: task, AgentType: models.AgentTypeReview, AgentInstanceID: "review-agent/verification", Status: models.AssignmentStatusActive},
	}
	db := &fakeDB{failOn: "INSERT INTO outbox_events"}
	if err := Commit(context.Background(), db, workgraph.New(nil), "review-agent", o); err == nil {
		t.Fatal("want the injected failure")
	}
	if tx := db.txs[0]; tx.committed || !tx.rolledBack || strings.Contains(strings.Join(tables(tx), " "), "assign") {
		t.Fatalf("a failed commit must roll back before (and with) the assignment: %v committed=%t", tables(tx), tx.committed)
	}
	db = &fakeDB{}
	if err := Commit(context.Background(), db, workgraph.New(nil), "review-agent", o); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tables(db.txs[0]), " "); got != "result:not_run outbox:verification.complete assign assignment:completed" {
		t.Fatalf("statements = %s", got)
	}
}

// F2: a transient failure is retried in process; a commit that keeps
// failing blocks the parent alone; nothing is left half-written.
func TestRecordRetriesThenBlocks(t *testing.T) {
	old := CommitRetryBackoff
	CommitRetryBackoff = []time.Duration{time.Millisecond}
	defer func() { CommitRetryBackoff = old }()
	parent := uuid.New()
	db := &fakeDB{failOn: "INSERT INTO verification_results"}
	o := &Outcome{
		Message: Message{WorkID: parent.String(), TaskID: uuid.New().String(), HeadSHA: "abc", Decision: DecisionEscalate},
		Result:  Result{WorkID: parent, TaskID: uuid.New(), HeadSHA: "abc", Decision: DecisionEscalate},
	}
	// The block's own transaction does not touch verification_results, so
	// it succeeds on the fourth Begin.
	if err := Record(context.Background(), db, workgraph.New(nil), "review-agent", o); err != nil {
		t.Fatalf("Record = %v, want nil after a successful block", err)
	}
	if len(db.txs) != MaxCommitAttempts+1 {
		t.Fatalf("transactions = %d, want %d attempts + the block", len(db.txs), MaxCommitAttempts)
	}
	for i := 0; i < MaxCommitAttempts; i++ {
		if db.txs[i].committed || !db.txs[i].rolledBack {
			t.Fatalf("attempt %d committed=%t rolledBack=%t", i+1, db.txs[i].committed, db.txs[i].rolledBack)
		}
	}
	block := db.txs[MaxCommitAttempts]
	if got := strings.Join(tables(block), " "); got != "lock state:blocked transition outbox:work.blocked" || !block.committed {
		t.Fatalf("block tx = %s (committed=%t)", got, block.committed)
	}
}

// A permanent error is not retried.
func TestRetryStopsOnAPermanentError(t *testing.T) {
	n := 0
	err := Retry(context.Background(), "x", func() error { n++; return &pgconn.PgError{Code: "23505"} })
	if err == nil || n != 1 {
		t.Fatalf("Retry ran %d times, err %v", n, err)
	}
}
