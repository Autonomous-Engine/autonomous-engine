package main

import (
	"context"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/configstore"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Env-gated (TEST_POSTGRES_DSN / TEST_DATABASE_URL) checks for the R2
// review fixes.

// R2 review fix 1: recording a result fails transiently (a trigger raises
// 40001 on the task row update, limited by a sequence that is not rolled
// back); it is retried in process and, when every attempt fails, the work
// item is blocked naming the task.
func TestIntegration_RecordResultRetriesThenBlocks(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	fastRecordRetries(t)
	for _, sql := range []string{
		`CREATE TABLE IF NOT EXISTS r2_fail_record (task_id TEXT PRIMARY KEY, failures INT NOT NULL)`,
		`CREATE SEQUENCE IF NOT EXISTS r2_fail_record_seq`,
		`CREATE OR REPLACE FUNCTION r2_fail_record_fn() RETURNS trigger AS $$
		DECLARE n INT;
		BEGIN
			SELECT failures INTO n FROM r2_fail_record WHERE task_id = NEW.task_id;
			IF FOUND AND nextval('r2_fail_record_seq') <= n THEN
				RAISE EXCEPTION 'r2 injected failure for %', NEW.task_id USING ERRCODE = '40001';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS r2_fail_record_trg ON research_round_tasks`,
		`CREATE TRIGGER r2_fail_record_trg BEFORE UPDATE ON research_round_tasks FOR EACH ROW EXECUTE FUNCTION r2_fail_record_fn()`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DROP TRIGGER IF EXISTS r2_fail_record_trg ON research_round_tasks`)
		_, _ = pool.Exec(c, `DROP FUNCTION IF EXISTS r2_fail_record_fn()`)
		_, _ = pool.Exec(c, `DROP TABLE IF EXISTS r2_fail_record`)
		_, _ = pool.Exec(c, `DROP SEQUENCE IF EXISTS r2_fail_record_seq`)
	})
	inject := func(taskID string, n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `SELECT setval('r2_fail_record_seq', 1, false)`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO r2_fail_record (task_id, failures) VALUES ($1, $2) ON CONFLICT (task_id) DO UPDATE SET failures = EXCLUDED.failures`, taskID, n); err != nil {
			t.Fatal(err)
		}
	}

	wg := workgraph.New(pool)
	store := &pgRoundStore{pool: pool}
	dispatch := func() (uuid.UUID, []researchTaskMessage) {
		workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)
		var tasks []researchTaskMessage
		for _, typ := range []string{"code_trace", "impact_assessment"} {
			tasks = append(tasks, researchTaskMessage{TaskID: uuid.NewString(), WorkID: workID.String(), ProductID: productID.String(), ResearchType: typ})
		}
		if err := commitResearchDispatch(ctx, pool, wg, workID, workID, tasks, roundFor(workID, prdHash("p"), tasks, nil)); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		return workID, tasks
	}
	block := func(workID uuid.UUID) func(string) error {
		return func(reason string) error { return blockInOwnTx(ctx, pool, workID, workID, reason) }
	}

	// Two failures: recorded on the third attempt, not blocked.
	workID, tasks := dispatch()
	inject(tasks[0].TaskID, 2)
	res := researchResultMessage{TaskID: tasks[0].TaskID, WorkID: workID.String(), Status: ResearchStatusOK, FindingType: "code_trace"}
	d, _, handled, err := recordOrBlock(ctx, store, res, block(workID))
	if handled || err != nil || !d.Known || d.Pending != 1 {
		t.Fatalf("after 2 injected failures: handled=%v err=%v decision=%+v, want recorded", handled, err, d)
	}
	if st, _, _ := stateOf(t, pool, workID); st != models.WorkItemStateResearch {
		t.Fatalf("state = %s, want research", st)
	}

	// Every attempt fails: the item is blocked naming the task.
	workID, tasks = dispatch()
	inject(tasks[0].TaskID, 100)
	res = researchResultMessage{TaskID: tasks[0].TaskID, WorkID: workID.String(), Status: ResearchStatusOK, FindingType: "code_trace"}
	_, _, handled, err = recordOrBlock(ctx, store, res, block(workID))
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v, want handled by a block", handled, err)
	}
	if st, _, _ := stateOf(t, pool, workID); st != models.WorkItemStateBlocked {
		t.Fatalf("state = %s, want blocked", st)
	}
	var blockedEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2 AND payload::text LIKE '%' || $3 || '%'`,
		workID, bus.TopicWorkBlocked, tasks[0].TaskID).Scan(&blockedEvents); err != nil || blockedEvents != 1 {
		t.Fatalf("work.blocked events naming the task = %d (%v), want 1", blockedEvents, err)
	}
}

type stubBriefLLM struct{ calls int }

func (s *stubBriefLLM) Execute(context.Context, strategy.Intent) (*strategy.Result, error) {
	s.calls++
	return &strategy.Result{Content: "# Technical Brief\nreused research"}, nil
}

// R2 review fix 2, end to end: every research type is reusable, so
// dispatchResearchRound dispatches nothing and synthesizes. The dispatch is
// unguarded; the prd.ready guard row commits with the brief. A crash between
// the dispatch and synthesis leaves prd.ready unrecorded, its redelivery
// dispatches again (superseding the stranded round) and finishes; a further
// redelivery is a duplicate.
func TestIntegration_AllReusedCrashThenRedeliveryFinishes(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	wg := workgraph.New(pool)
	store := &pgRoundStore{pool: pool}
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)
	const prd = "the PRD"
	types := []string{"code_trace"}

	// An earlier round whose code_trace is ok, now blocked.
	earlier := &researchRound{ID: uuid.New(), WorkID: workID, PRDHash: prdHash(prd), PRDContent: prd, AggregateID: workID, Status: roundOpen, CreatedAt: time.Now(),
		Tasks: []roundTask{{TaskID: uuid.NewString(), ResearchType: "code_trace", Status: ResearchStatusOK,
			Result: &researchResultMessage{Status: ResearchStatusOK, FindingType: "code_trace", Evidence: "seeded evidence"}}}}
	if err := commitResearchDispatch(ctx, pool, wg, workID, workID, nil, earlier); err != nil {
		t.Fatalf("seed earlier round: %v", err)
	}
	if err := store.Close(ctx, earlier.ID, roundBlocked); err != nil {
		t.Fatal(err)
	}

	stub := &stubBriefLLM{}
	origLLM, origFinish := resolveBriefLLM, finishAllReused
	resolveBriefLLM = func(context.Context, *agentregistry.Registry, uuid.UUID, *configstore.ConfigStore, *pgxpool.Pool) (briefLLM, error) {
		return stub, nil
	}
	t.Cleanup(func() { resolveBriefLLM, finishAllReused = origLLM, origFinish })

	ledger := idempotency.NewLedger(pool)
	eventID := uuid.New()
	msg := &bus.Message{Topic: bus.TopicPRDReady, Key: workID.String(), Headers: map[string]string{bus.HeaderEventID: eventID.String()}}
	in := dispatchInput{
		workID: workID, aggregateID: workID,
		payload:         prdReadyMessage{WorkID: workID.String(), ProductID: productID.String(), WorkType: "bug", PRDContent: prd},
		startingContext: prd, taskTypes: types,
	}
	deliver := func() error {
		dctx := idempotency.WithDelivery(ctx, ledger.NewDelivery(agentID, msg))
		return dispatchResearchRound(dctx, pool, wg, nil, nil, nil, newCoordinator(), store, in)
	}

	// Delivery 1 crashes after the dispatch commit: synthesis never runs.
	finishAllReused = func(context.Context, *pgxpool.Pool, *workgraph.WorkGraph, *agentregistry.Registry, *configstore.ConfigStore, bus.MessageBus, *coordinator, roundStore, *researchRound, bool) error {
		return nil
	}
	if err := deliver(); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	if seen, err := ledger.Seen(ctx, agentID, eventID); err != nil || seen {
		t.Fatalf("prd.ready recorded by the all-reused dispatch (seen=%v err=%v): a crash now would strand the round", seen, err)
	}
	var stranded uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM research_rounds WHERE work_item_id = $1 AND status = 'synthesizing'`, workID).Scan(&stranded); err != nil {
		t.Fatalf("no synthesizing round after the crash: %v", err)
	}

	// Delivery 2 (redelivery after restart) finishes.
	finishAllReused = origFinish
	if err := deliver(); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("synthesis ran %d times, want 1", stub.calls)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM research_rounds WHERE id = $1`, stranded).Scan(&status); err != nil || status != roundSuperseded {
		t.Fatalf("stranded round status = %q (%v), want superseded", status, err)
	}
	var completed, artifacts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM research_rounds WHERE work_item_id = $1 AND status = 'completed'`, workID).Scan(&completed); err != nil || completed != 1 {
		t.Fatalf("completed rounds = %d (%v), want 1 (closed in the brief's transaction)", completed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM work_item_artifacts WHERE work_item_id = $1 AND artifact_type = 'technical_brief'`, workID).Scan(&artifacts); err != nil || artifacts != 1 {
		t.Fatalf("technical briefs = %d (%v), want 1", artifacts, err)
	}
	if seen, err := ledger.Seen(ctx, agentID, eventID); err != nil || !seen {
		t.Fatalf("prd.ready not recorded with the brief (seen=%v err=%v)", seen, err)
	}
	var briefEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND topic = $2`, workID, bus.TopicTechnicalBriefReady).Scan(&briefEvents); err != nil || briefEvents != 1 {
		t.Fatalf("technical-brief.ready events = %d (%v), want 1", briefEvents, err)
	}

	// Delivery 3 is acknowledged by the ledger without running.
	handler := ledger.Handler(agentID, func(ctx context.Context, m *bus.Message) error {
		t.Fatal("a recorded prd.ready ran again")
		return nil
	})
	if err := handler(ctx, msg); err != nil {
		t.Fatalf("delivery 3: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("synthesis ran %d times after the duplicate, want 1", stub.calls)
	}
}
