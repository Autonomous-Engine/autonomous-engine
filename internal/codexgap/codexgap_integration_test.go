package codexgap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/db"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Against a real PostgreSQL (TEST_POSTGRES_DSN, a disposable database: the
// test migrates it).

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
	if err := db.Migrate(ctx, p, filepath.Join("..", "..", "migrations")); err != nil {
		p.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Item is a work item that entered state through a recorded transition.
func Item(t *testing.T, pool *pgxpool.Pool, state models.WorkItemState) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO work_items (id, type, state, title, description) VALUES ($1, 'feature', 'blocked', 'gap item', 'd')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM work_items WHERE id = $1`, id) })
	moveTo(t, pool, id, state)
	return id
}

func moveTo(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, state models.WorkItemState) {
	t.Helper()
	if err := workgraph.New(pool).TransitionState(context.Background(), id, state, "test", "test"); err != nil {
		t.Fatalf("transition to %s: %v", state, err)
	}
}

func gap(t *testing.T, item uuid.UUID, id string) []byte {
	t.Helper()
	raw, _ := json.Marshal(Event{GapID: id, WorkItemID: item.String(), CodexTool: "codex_find_module", Query: "q", FallbackTool: "search_code", FoundVia: "grep"})
	return raw
}

func record(t *testing.T, pool *pgxpool.Pool, raw []byte, threshold int) Result {
	t.Helper()
	var res Result
	if err := workgraph.WithTx(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		res, err = RecordTx(context.Background(), tx, raw, threshold)
		return err
	}); err != nil {
		t.Fatalf("RecordTx: %v", err)
	}
	return res
}

// More than the threshold of empty lookups in one implementation run marks
// it ungrounded; a redelivered event is not counted again; a new run starts
// from zero and a clean latest run is not ungrounded.
func TestIntegration_Threshold(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	item := Item(t, pool, models.WorkItemStateImplementation)

	for i := 1; i <= 5; i++ {
		res := record(t, pool, gap(t, item, uuid.NewString()), 5)
		if !res.Recorded || res.Count != i || res.Ungrounded {
			t.Fatalf("event %d: %+v", i, res)
		}
	}
	if u, _, err := Ungrounded(ctx, pool, item); err != nil || u {
		t.Fatalf("5 lookups with threshold 5: ungrounded=%v err=%v, want false", u, err)
	}
	sixth := gap(t, item, uuid.NewString())
	res := record(t, pool, sixth, 5)
	if !res.Ungrounded || !res.Newly || res.Count != 6 || res.Stage != models.WorkItemStateImplementation {
		t.Fatalf("sixth event: %+v", res)
	}
	if dup := record(t, pool, sixth, 5); dup.Recorded {
		t.Errorf("a redelivered event was counted again: %+v", dup)
	}
	u, why, err := Ungrounded(ctx, pool, uuid.New(), item)
	if err != nil || !u || why == "" {
		t.Fatalf("Ungrounded = %v %q %v", u, why, err)
	}

	// Not counted: planner-scope gaps and items outside a research or
	// implementation run.
	planner, _ := json.Marshal(Event{WorkItemID: item.String(), Reason: "planner_missed_file", FallbackTool: "edit_file"})
	if r := record(t, pool, planner, 5); r.Recorded {
		t.Errorf("planner gap counted: %+v", r)
	}
	verifying := Item(t, pool, models.WorkItemStateResearch)
	moveTo(t, pool, verifying, models.WorkItemStatePlanning)
	if r := record(t, pool, gap(t, verifying, uuid.NewString()), 0); r.Recorded {
		t.Errorf("a planning-state gap was counted: %+v", r)
	}

	// The item re-enters implementation (a re-run): the new run starts at
	// zero, and its being the latest run clears the ungrounded verdict.
	time.Sleep(10 * time.Millisecond)
	moveTo(t, pool, item, models.WorkItemStateBlocked)
	moveTo(t, pool, item, models.WorkItemStateImplementation)
	if r := record(t, pool, gap(t, item, uuid.NewString()), 5); r.Count != 1 || r.Ungrounded {
		t.Errorf("first lookup of the re-run: %+v", r)
	}
	if u, _, _ := Ungrounded(ctx, pool, item); u {
		t.Error("the latest implementation run is clean; the item must not be ungrounded")
	}
}

// gapAt is an empty lookup the agent recorded at at.
func gapAt(t *testing.T, item uuid.UUID, at time.Time) []byte {
	t.Helper()
	raw, _ := json.Marshal(Event{GapID: uuid.NewString(), WorkItemID: item.String(), CodexTool: "codex_find_module", Query: "q",
		FallbackTool: "search_code", Timestamp: at.UTC().Format(time.RFC3339Nano)})
	return raw
}

// A prompt round-trip mid-run (implementation → awaiting_user →
// implementation) does not start a new run: the lookups before and after
// it add up, and the run is ungrounded.
func TestIntegration_PromptRoundTripKeepsTheRun(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	item := Item(t, pool, models.WorkItemStateImplementation)
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 3; i++ {
		record(t, pool, gapAt(t, item, time.Now()), 5)
	}
	moveTo(t, pool, item, models.WorkItemStateAwaitingUser)
	moveTo(t, pool, item, models.WorkItemStateImplementation)
	time.Sleep(5 * time.Millisecond)
	var last Result
	for i := 0; i < 3; i++ {
		last = record(t, pool, gapAt(t, item, time.Now()), 5)
	}
	if last.Count != 6 || !last.Ungrounded {
		t.Errorf("after the round-trip: %+v, want 6 lookups in one ungrounded run", last)
	}
	if u, _, _ := Ungrounded(ctx, pool, item); !u {
		t.Error("the run must stay ungrounded across a prompt round-trip")
	}
}

// A research lookup consumed late (the parent is in implementation by
// then) counts for the research run its timestamp falls in; one from the
// planning interval counts for no run.
func TestIntegration_LaggingGapCountsForItsOwnRun(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	item := Item(t, pool, models.WorkItemStateResearch)
	time.Sleep(5 * time.Millisecond)
	var researchGaps [][]byte
	for i := 0; i < 6; i++ {
		researchGaps = append(researchGaps, gapAt(t, item, time.Now()))
	}
	time.Sleep(5 * time.Millisecond)
	moveTo(t, pool, item, models.WorkItemStatePlanning)
	time.Sleep(5 * time.Millisecond)
	planningGap := gapAt(t, item, time.Now())
	time.Sleep(5 * time.Millisecond)
	moveTo(t, pool, item, models.WorkItemStateImplementation)

	var last Result
	for _, g := range researchGaps {
		last = record(t, pool, g, 5)
	}
	if last.Stage != models.WorkItemStateResearch || last.Count != 6 || !last.Ungrounded {
		t.Errorf("lagging research gaps: %+v, want 6 in the research run", last)
	}
	if r := record(t, pool, planningGap, 5); r.Recorded {
		t.Errorf("a planning-interval gap was counted: %+v", r)
	}
	if u, why, _ := Ungrounded(ctx, pool, item); !u || !strings.Contains(why, "research") {
		t.Errorf("Ungrounded = %v %q, want the research run", u, why)
	}
}

// A payload without gap_id is deduplicated on its content.
func TestIntegration_NoGapIDDedupesOnPayload(t *testing.T) {
	pool := integrationPool(t)
	item := Item(t, pool, models.WorkItemStateResearch)
	raw, _ := json.Marshal(Event{WorkItemID: item.String(), CodexTool: "codex_grep", Query: "x", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)})
	if r := record(t, pool, raw, 5); !r.Recorded || r.Stage != models.WorkItemStateResearch {
		t.Fatalf("first: %+v", r)
	}
	if r := record(t, pool, raw, 5); r.Recorded {
		t.Errorf("the same payload was counted twice: %+v", r)
	}
}

func TestThresholdFromEnv(t *testing.T) {
	t.Setenv(ThresholdEnv, "")
	if got := ThresholdFromEnv(); got != DefaultThreshold {
		t.Errorf("unset = %d", got)
	}
	t.Setenv(ThresholdEnv, "2")
	if got := ThresholdFromEnv(); got != 2 {
		t.Errorf("2 = %d", got)
	}
	t.Setenv(ThresholdEnv, "lots")
	if got := ThresholdFromEnv(); got != DefaultThreshold {
		t.Errorf("malformed = %d", got)
	}
}
