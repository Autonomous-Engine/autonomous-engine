package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeRunReader struct {
	runs []controller.RunSummary
	err  error
}

func (f fakeRunReader) LatestRuns(context.Context, uuid.UUID) ([]controller.RunSummary, error) {
	return f.runs, f.err
}

func TestImplementationRunsNeverFailsTheDetail(t *testing.T) {
	ctx := context.Background()
	h := &Handlers{}
	if got := h.implementationRuns(ctx, uuid.New()); got == nil || len(got) != 0 {
		t.Fatalf("no reader: %v", got)
	}
	h.runs = fakeRunReader{err: errors.New("relation does not exist")}
	if got := h.implementationRuns(ctx, uuid.New()); got == nil || len(got) != 0 {
		t.Fatalf("reader error: %v", got)
	}
	want := []controller.RunSummary{{ID: "r1", TaskID: "t1", Attempt: 2, State: controller.StateRedObserved}}
	h.runs = fakeRunReader{runs: want}
	if got := h.implementationRuns(ctx, uuid.New()); len(got) != 1 || got[0].State != controller.StateRedObserved {
		t.Fatalf("rows: %v", got)
	}
}

type captureExec struct {
	sql  string
	args []any
}

func (c *captureExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.sql, c.args = sql, args
	return pgconn.CommandTag{}, nil
}

func TestPersistToolEventWritesPhaseAndExitCode(t *testing.T) {
	var ev consoleToolEvent
	if err := json.Unmarshal([]byte(`{"work_item_id":"w","task_id":"t","agent_type":"implementation-agent","tool":"run_command",
		"arguments":"{}","result":"FAIL","is_error":false,"timestamp":"2026-10-03T12:00:00Z","phase":"red_observed","exit_code":1}`), &ev); err != nil {
		t.Fatal(err)
	}
	c := &captureExec{}
	if err := persistToolEvent(context.Background(), c, ev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.sql, "phase, exit_code") || c.args[8] != "red_observed" || *(c.args[9].(*int)) != 1 {
		t.Fatalf("sql %q args %v", c.sql, c.args)
	}

	// An agent without a controller sends neither: both are written as NULL.
	var plain consoleToolEvent
	_ = json.Unmarshal([]byte(`{"work_item_id":"w","tool":"read_file"}`), &plain)
	if err := persistToolEvent(context.Background(), c, plain); err != nil {
		t.Fatal(err)
	}
	if c.args[8] != "" || c.args[9].(*int) != nil || !strings.Contains(c.sql, "NULLIF($9, '')") {
		t.Fatalf("plain event args %v", c.args)
	}
}

// ── Integration (TEST_POSTGRES_DSN) ──────────────────────────────────────────

func runsIntegrationPool(t *testing.T) *pgxpool.Pool {
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
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// The detail payload and the tool-events list carry exactly what the agent
// recorded: the run's state, its red/green transition rows, and each
// console event's phase and exit code.
func TestIntegration_DetailCarriesRecordedRunsAndPhases(t *testing.T) {
	pool := runsIntegrationPool(t)
	ctx := context.Background()
	productID, workID, taskID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO products (id, name) VALUES ($1, $2)`, productID, "m301-dash-"+productID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, product_id) VALUES ($1, 'feature', 'implementation', 'M3-02 detail', $2)`, workID, productID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tool_events WHERE work_item_id = $1`, workID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, workID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM products WHERE id = $1`, productID)
	})

	store := controller.NewStore(pool)
	started, err := store.StartRun(ctx, workID, taskID.String())
	if err != nil {
		t.Fatal(err)
	}
	run := controller.NewRun(store, started, workID, taskID.String())
	one := 1
	for _, ev := range []controller.Event{
		{Kind: controller.EventTool, CallID: "w1", Tool: "write_file", Path: "a_test.go"},
		{Kind: controller.EventTool, CallID: "red-call", Tool: "run_command", Command: "go test ./...", ExitCode: &one},
	} {
		phase := run.Observe(ctx, ev)
		if err := persistToolEvent(ctx, pool, consoleToolEvent{
			WorkItemID: workID.String(), TaskID: taskID.String(), AgentType: "implementation-agent",
			Tool: ev.Tool, Phase: string(phase), ExitCode: ev.ExitCode,
		}); err != nil {
			t.Fatal(err)
		}
	}

	h := NewHandlers(workgraph.New(pool), nil, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	h.GetWorkItem(rec, httptest.NewRequest(http.MethodGet, "/api/work-items/detail?id="+workID.String(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Runs []controller.RunSummary `json:"implementation_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Runs) != 1 {
		t.Fatalf("implementation_runs = %+v", detail.Runs)
	}
	r := detail.Runs[0]
	if r.State != controller.StateRedObserved || r.TaskID != taskID.String() || r.Red == nil || r.Red.ToolCallID != "red-call" || *r.Red.ExitCode != 1 || r.Green != nil {
		t.Fatalf("run summary = %+v", r)
	}

	rec = httptest.NewRecorder()
	h.ListToolEvents(rec, httptest.NewRequest(http.MethodGet, "/api/work-items/tool-events?work_item_id="+workID.String(), nil))
	var events []struct {
		Tool     string `json:"tool"`
		Phase    string `json:"phase"`
		ExitCode *int   `json:"exit_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode tool events: %v (%s)", err, rec.Body.String())
	}
	if len(events) != 2 || events[0].Phase != "test_authored" || events[0].ExitCode != nil ||
		events[1].Phase != "red_observed" || events[1].ExitCode == nil || *events[1].ExitCode != 1 {
		t.Fatalf("tool events = %+v", events)
	}
}
