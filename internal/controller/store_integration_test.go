package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store against a real PostgreSQL (migration 052). Skips without
// TEST_POSTGRES_DSN / TEST_DATABASE_URL.

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN / TEST_DATABASE_URL not set — skipping DB-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func seedWorkItem(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	productID, workID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m301-test-"+productID.String()[:8]); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'implementation', 'M3-01 parent', $2)`, workID, productID); err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, productID)
	})
	return workID
}

// A regression clears the dashboard's green; a run that finished
// unverified is not "open"; the superseded note takes the next seq after
// what the previous run actually wrote.
func TestIntegration_StoreStaleGreenAndSupersede(t *testing.T) {
	pool := integrationPool(t)
	workID := seedWorkItem(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	// Regression: green then a failing run.
	taskA := uuid.NewString()
	sa, err := store.StartRun(ctx, workID, taskA)
	if err != nil {
		t.Fatal(err)
	}
	ra := NewRun(store, sa, workID, taskA)
	for _, ev := range []Event{write("a_test.go"), run("go test ./...", 1), edit("a.go"),
		{Kind: EventTool, CallID: "green-1", Tool: "run_command", Command: "go test ./...", ExitCode: exit(0)}} {
		ra.Observe(ctx, ev)
	}
	sum := func() RunSummary {
		t.Helper()
		sums, err := store.LatestRuns(ctx, uuid.MustParse(taskA))
		if err != nil || len(sums) != 1 {
			t.Fatalf("LatestRuns = %+v, %v", sums, err)
		}
		return sums[0]
	}
	if s := sum(); s.Green == nil || s.Green.ToolCallID != "green-1" {
		t.Fatalf("green before regression = %+v", s.Green)
	}
	ra.Observe(ctx, run("go test ./...", 1))
	if s := sum(); s.State != StateImplement || s.Green != nil || s.Red == nil {
		t.Fatalf("after regression: state %s green %+v red %+v; want implement, no green", s.State, s.Green, s.Red)
	}
	ra.Observe(ctx, Event{Kind: EventTool, CallID: "green-2", Tool: "run_command", Command: "go test ./...", ExitCode: exit(0)})
	if s := sum(); s.Green == nil || s.Green.ToolCallID != "green-2" {
		t.Fatalf("green after re-green = %+v", s.Green)
	}

	// Finished unverified: state stays implement, detail.final set → no superseded note.
	taskB := uuid.NewString()
	sb, _ := store.StartRun(ctx, workID, taskB)
	rb := NewRun(store, sb, workID, taskB)
	rb.Observe(ctx, edit("b.go"))
	tx, _ := pool.Begin(ctx)
	if _, err := rb.FinishTx(ctx, tx, FinalUnverified, "evidence outcome partially_verified"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Events(ctx, sb.ID)
	if _, err := store.StartRun(ctx, workID, taskB); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Events(ctx, sb.ID)
	if len(after) != len(before) {
		t.Fatalf("a finished-unverified run got a superseded note: %+v", after[len(after)-1])
	}

	// Open run whose in-memory seq lags what it wrote: the note uses MAX(seq)+1.
	taskC := uuid.NewString()
	sc, _ := store.StartRun(ctx, workID, taskC)
	rc := NewRun(store, sc, workID, taskC)
	rc.Observe(ctx, edit("c.go")) // seqs 2 (tool), 3 (transition)
	if err := store.AppendEvent(ctx, sc.ID, EventRow{Seq: 9, Kind: KindNote, Note: "written by a live process"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRun(ctx, workID, taskC); err != nil {
		t.Fatalf("StartRun over a live run: %v", err)
	}
	evc, _ := store.Events(ctx, sc.ID)
	if last := evc[len(evc)-1]; last.Seq != 10 || last.Kind != KindNote {
		t.Fatalf("superseded note = %+v, want seq 10", last)
	}
}

func TestIntegration_StoreRunLifecycle(t *testing.T) {
	pool := integrationPool(t)
	workID := seedWorkItem(t, pool)
	store := NewStore(pool)
	ctx := context.Background()
	taskID := uuid.NewString()

	s1, err := store.StartRun(ctx, workID, taskID)
	if err != nil || s1.Attempt != 1 || s1.Previous != nil {
		t.Fatalf("StartRun #1 = %+v, %v", s1, err)
	}
	r := NewRun(store, s1, workID, taskID)
	r.SetTestCommand("go test ./... -count=1")
	for _, ev := range []Event{
		write("calc_test.go"),
		{Kind: EventTool, CallID: "call-red", Tool: "run_command", Command: "go test ./... -count=1", ExitCode: exit(1)},
		edit("calc.go"),
		{Kind: EventTool, CallID: "call-green", Tool: "run_command", Command: "go test ./... -count=1", ExitCode: exit(0)},
	} {
		r.Observe(ctx, ev)
	}

	// Terminal state in the caller's transaction: rolled back, then committed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinishTx(ctx, tx, FinalVerified, ""); err != nil {
		t.Fatalf("FinishTx: %v", err)
	}
	_ = tx.Rollback(ctx)
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM implementation_runs WHERE id = $1`, s1.ID).Scan(&state); err != nil || state != "green_observed" {
		t.Fatalf("after rollback state = %q, %v; want green_observed", state, err)
	}
	tx, _ = pool.Begin(ctx)
	if _, err := r.FinishTx(ctx, tx, FinalBlocked, "push rejected"); err != nil {
		t.Fatalf("FinishTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	events, err := store.Events(ctx, s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for _, e := range events {
		if e.Kind == KindTransition {
			transitions = append(transitions, string(e.FromState)+"→"+string(e.ToState))
		}
	}
	want := []string{"inspect→test_authored", "test_authored→red_observed", "red_observed→implement", "implement→green_observed", "green_observed→blocked"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	}
	if events[0].Kind != KindNote || events[0].Seq != 1 {
		t.Fatalf("first event = %+v, want the start note", events[0])
	}

	// Redelivery while open: attempt 2, previous run gets a note.
	s1b, err := store.StartRun(ctx, workID, taskID)
	if err != nil || s1b.Attempt != 2 || s1b.Previous == nil || s1b.Previous.State != StateBlocked {
		t.Fatalf("StartRun #2 = %+v, %v", s1b, err)
	}
	r2 := NewRun(store, s1b, workID, taskID)
	r2.Observe(ctx, edit("calc.go"))
	s3, err := store.StartRun(ctx, workID, taskID)
	if err != nil || s3.Attempt != 3 || s3.Previous.State != StateImplement {
		t.Fatalf("StartRun #3 = %+v, %v", s3, err)
	}
	ev2, _ := store.Events(ctx, s1b.ID)
	if last := ev2[len(ev2)-1]; last.Kind != KindNote || last.Note == "" {
		t.Fatalf("superseded note missing: %+v", last)
	}

	// Dashboard summary: latest attempt per task with its own evidence.
	sums, err := store.LatestRuns(ctx, workID)
	if err != nil || len(sums) != 1 || sums[0].Attempt != 3 || sums[0].State != StateInspect || sums[0].Red != nil {
		t.Fatalf("LatestRuns = %+v, %v", sums, err)
	}
	// The same summary by subtask id.
	byTask, err := store.LatestRuns(ctx, uuid.MustParse(taskID))
	if err != nil || len(byTask) != 1 {
		t.Fatalf("LatestRuns(task) = %+v, %v", byTask, err)
	}

	// Attempt 1's red/green evidence, as the dashboard reads it.
	var redCall, greenCall string
	if err := pool.QueryRow(ctx, `SELECT detail->'red'->>'call_id', detail->'green'->>'call_id' FROM implementation_runs WHERE id = $1`, s1.ID).Scan(&redCall, &greenCall); err != nil || redCall != "call-red" || greenCall != "call-green" {
		t.Fatalf("detail evidence = %q / %q, %v", redCall, greenCall, err)
	}

	// The state CHECK rejects unknown states.
	if _, err := pool.Exec(ctx, `UPDATE implementation_runs SET state = 'done' WHERE id = $1`, s1.ID); err == nil {
		t.Fatal("state CHECK accepted 'done'")
	}
	// Cascade: deleting the work item removes runs and events.
	if _, err := pool.Exec(ctx, `DELETE FROM work_items WHERE id = $1`, workID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM implementation_run_events WHERE run_id = $1`, s1.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d events survived the work item delete", n)
	}
}
