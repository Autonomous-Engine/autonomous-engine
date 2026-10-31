package stage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/verifier"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// V1 against a real PostgreSQL: the verdict artifact, the
// verification_results row, the verification.complete outbox event and the
// delivery's guard row commit in one transaction; the same delivery again
// is a duplicate that writes nothing; ReusableVerdict then finds the
// verdict for the head and LatestByTask the record.
func TestIntegration_CommitIsAtomicWithTheGuard(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	parent, task := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', 'implementation', 'R14 commit parent', 'd')`, []any{parent}},
		{`INSERT INTO work_items (id, type, state, title, description, parent_id) VALUES ($1, 'subtask', 'passed', 'R14 commit subtask', 'd', $2)`, []any{task, parent}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	eventID := uuid.New()
	t.Cleanup(func() {
		for _, q := range []struct {
			sql string
			arg any
		}{
			{`DELETE FROM processed_events WHERE event_id = $1`, eventID},
			{`DELETE FROM outbox_events WHERE aggregate_id = $1`, parent},
			{`DELETE FROM work_items WHERE parent_id = $1`, parent},
			{`DELETE FROM work_items WHERE id = $1`, parent},
		} {
			if _, err := pool.Exec(context.Background(), q.sql, q.arg); err != nil {
				t.Logf("cleanup %q: %v", q.sql, err)
			}
		}
	})
	ledger := idempotency.NewLedger(pool)
	msg := &bus.Message{Topic: bus.TopicImplementationComplete, Key: parent.String(), Value: []byte(`{}`), Headers: map[string]string{bus.HeaderEventID: eventID.String()}}

	head := "0123456789abcdef0123456789abcdef01234567"
	v := verifier.Verdict{SchemaVersion: 1, Overall: verifier.OverallEscalate, Defects: []verifier.Defect{},
		PerCriterion: []verifier.CriterionResult{{ID: "AC-1", Status: verifier.StatusNotVerifiable, Evidence: []verifier.Ref{}, Note: "no test"}}}
	g := verifier.Gate(v)
	newOutcome := func() *Outcome {
		return &Outcome{
			Message:  Message{WorkID: parent.String(), TaskID: task.String(), HeadSHA: head, Decision: DecisionEscalate, Reason: g.Reason},
			Result:   Result{WorkID: parent, TaskID: task, HeadSHA: head, Decision: DecisionEscalate, Reason: g.Reason, VerifierVersion: verifier.Version},
			Artifact: NewArtifact(parent, "review-agent", NewArtifactContent(v, g, string(DecisionEscalate), "agent/x", head, head, task.String())),
		}
	}
	commit := func() error {
		dctx := idempotency.WithDelivery(ctx, ledger.NewDelivery("verification-stage", msg))
		return Commit(ctx, idempotency.Wrap(dctx, pool), workgraph.New(pool), "review-agent", newOutcome())
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	if err := commit(); !idempotency.IsDuplicate(err) {
		t.Fatalf("second commit of the same delivery = %v, want a duplicate", err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1 AND content->>'kind' = 'verifier'`, parent); n != 1 {
		t.Errorf("verdict artifacts = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM verification_results WHERE work_item_id = $1`, parent); n != 1 {
		t.Errorf("results = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, parent, bus.TopicVerificationComplete); n != 1 {
		t.Errorf("verification.complete events = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM processed_events WHERE consumer = 'verification-stage' AND event_id = $1`, eventID); n != 1 {
		t.Errorf("guard rows = %d, want 1", n)
	}
	sv, ok, err := ReusableVerdict(ctx, pool, parent, task, head)
	if err != nil || !ok || sv.Result.Decision != DecisionEscalate || sv.Content.Gate.Action != verifier.ActionEscalate || sv.Content.TaskID != task.String() {
		t.Fatalf("ReusableVerdict = %+v ok=%t err=%v", sv, ok, err)
	}
	if _, ok, _ := ReusableVerdict(ctx, pool, parent, task, ""); ok {
		t.Error("an empty head must never match a stored verdict")
	}
	// A sibling subtask with the same head is judged against its own
	// requirements: it does not inherit the verdict (review minor 4).
	sibling := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description, parent_id) VALUES ($1, 'subtask', 'passed', 'R14 sibling', 'd', $2)`, sibling, parent); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReusableVerdict(ctx, pool, parent, sibling, head); err != nil || ok {
		t.Errorf("a sibling with the same head inherited the verdict (ok=%t err=%v)", ok, err)
	}
	// review-agent on pr.ready does not know the subtask: any matches.
	if sv, ok, err := ReusableVerdict(ctx, pool, parent, uuid.Nil, head); err != nil || !ok || sv.Result.TaskID != task {
		t.Errorf("any-subtask lookup = %+v ok=%t err=%v", sv.Result, ok, err)
	}
	latest, err := LatestByTask(ctx, pool, parent)
	if err != nil || latest[task].Decision != DecisionEscalate {
		t.Fatalf("LatestByTask = %+v, %v", latest, err)
	}
	if r, ok, err := LatestForTask(ctx, pool, task); err != nil || !ok || r.HeadSHA != head {
		t.Fatalf("LatestForTask = %+v ok=%t err=%v", r, ok, err)
	}
}
