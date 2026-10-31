package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── Source guards ──────────────────────────────────────────────────────────

// orchestratorSources returns the package's non-test Go sources with comments
// stripped, so prose about the old mechanism cannot fail a test about code.
func orchestratorSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	line := regexp.MustCompile(`(?m)//.*$`)
	block := regexp.MustCompile(`(?s)/\*.*?\*/`)
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = line.ReplaceAllString(block.ReplaceAllString(string(b), " "), " ")
	}
	if len(out) == 0 {
		t.Fatal("found no sources to scan")
	}
	return out
}

// TestOrchestrator_OnlyStateChangedIsPublishedDirectly pins the M1-04a
// conversion: every event that announces a state change goes through the
// outbox in the transaction that makes the change. The single direct publish
// left is state.changed (websocket/Jira fan-out telemetry), and it lives in
// publishStateChanged, which is only called after a commit.
func TestOrchestrator_OnlyStateChangedIsPublishedDirectly(t *testing.T) {
	publishCall := regexp.MustCompile(`\.Publish\(([^\n]*)`)
	direct := 0
	for name, code := range orchestratorSources(t) {
		for _, forbidden := range []string{"publishJSON(", ".PublishRequested(", ".PublishMessage(", "guards.BlockWorkItem("} {
			if strings.Contains(code, forbidden) {
				t.Errorf("%s calls %s — state-change events must be enqueued with outbox.EnqueueJSON / PublishTx inside the transition's transaction", name, forbidden)
			}
		}
		for _, m := range publishCall.FindAllStringSubmatch(code, -1) {
			direct++
			if !strings.Contains(m[1], "topicStateChanged") {
				t.Errorf("%s: direct publish %q is not state.changed; only the state.changed telemetry may bypass the outbox", name, strings.TrimSpace(m[0]))
			}
		}
	}
	if direct != 1 {
		t.Errorf("found %d direct .Publish( calls, want exactly 1 (publishStateChanged)", direct)
	}
	if topicStateChanged != "state.changed" {
		t.Errorf("topicStateChanged = %q, want state.changed (consumers subscribe to that name)", topicStateChanged)
	}
}

// TestOrchestrator_StartsAndWaitsForOneNamedDispatcher pins the wiring in
// main(): exactly one dispatcher, named after the service, waited for on
// shutdown.
func TestOrchestrator_StartsAndWaitsForOneNamedDispatcher(t *testing.T) {
	starts, named, waits := 0, 0, 0
	for _, code := range orchestratorSources(t) {
		starts += strings.Count(code, "outbox.Start(") + strings.Count(code, "outbox.StartWithOptions(") + strings.Count(code, "outbox.StartForService(")
		named += strings.Count(code, `outbox.StartForService(ctx, pool, kb, log.Default(), "status-orchestrator")`)
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || named != 1 {
		t.Errorf("dispatcher starts = %d (named via StartForService: %d), want exactly 1 StartForService(..., \"status-orchestrator\")", starts, named)
	}
	if waits != 1 {
		t.Errorf("dispatcher.Wait() calls = %d, want exactly 1 on the shutdown path", waits)
	}
}

// ─── Fakes ──────────────────────────────────────────────────────────────────

// journal is a shared, ordered record of what the fakes saw, so tests can
// assert "state.changed only after commit".
type journal struct {
	mu      sync.Mutex
	entries []string
}

func (j *journal) add(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, s)
}

func (j *journal) list() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.entries...)
}

// fakeExec is one statement the fake transaction received.
type fakeExec struct {
	sql  string
	args []any
}

// fakeTx is the slice of pgx.Tx the conversion uses: Exec (outbox and prompt
// inserts), QueryRow (the pending-escalation check), Commit and Rollback.
type fakeTx struct {
	pgx.Tx
	j                 *journal
	execs             []fakeExec
	execErr           error // returned by Exec when its SQL contains execErrOn
	execErrOn         string
	pendingEscalation bool
	store             *fakeStore // answers the row-lock read
	committed         bool
	rolledBack        bool
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.execErr != nil && strings.Contains(sql, t.execErrOn) {
		return pgconn.CommandTag{}, t.execErr
	}
	t.execs = append(t.execs, fakeExec{sql: sql, args: args})
	t.j.add("exec")
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("boolRow: %d destinations", len(dest))
	}
	p, ok := dest[0].(*bool)
	if !ok {
		return fmt.Errorf("boolRow: destination %T", dest[0])
	}
	*p = r.v
	return nil
}

// lockRow answers lockWorkItemStateSQL with (state, previous_state).
type lockRow struct {
	state    string
	previous *string
	err      error
}

func (r lockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 2 {
		return fmt.Errorf("lockRow: %d destinations", len(dest))
	}
	s, ok1 := dest[0].(*string)
	p, ok2 := dest[1].(**string)
	if !ok1 || !ok2 {
		return fmt.Errorf("lockRow: destinations %T, %T", dest[0], dest[1])
	}
	*s, *p = r.state, r.previous
	return nil
}

func (t *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	t.j.add("query")
	switch sql {
	case pendingEscalationPromptSQL:
		return boolRow{v: t.pendingEscalation}
	case lockWorkItemStateSQL:
		t.j.add("lock")
		if t.store == nil {
			return lockRow{err: errors.New("fakeTx: no store for the row lock")}
		}
		id := args[0].(uuid.UUID)
		it, ok := t.store.items[id]
		if !ok {
			return lockRow{err: pgx.ErrNoRows}
		}
		var prev *string
		if p, ok := t.store.previous[id]; ok {
			s := string(p)
			prev = &s
		}
		return lockRow{state: string(it.State), previous: prev}
	}
	return boolRow{}
}

func (t *fakeTx) Commit(context.Context) error {
	t.committed = true
	t.j.add("commit")
	return nil
}

func (t *fakeTx) Rollback(context.Context) error {
	if !t.committed {
		t.rolledBack = true
		t.j.add("rollback")
	}
	return nil
}

// fakeOutboxRow is one outbox insert the fake transaction received.
type fakeOutboxRow struct {
	Aggregate uuid.UUID
	Topic     string
	Key       string
	Payload   map[string]any
}

// outboxRows decodes the outbox inserts (insertArgs order: id, aggregate_type,
// aggregate_id, topic, key, payload, headers).
func (t *fakeTx) outboxRows() []fakeOutboxRow {
	var out []fakeOutboxRow
	for _, e := range t.execs {
		if !strings.Contains(e.sql, "outbox_events") {
			continue
		}
		var payload map[string]any
		_ = json.Unmarshal(e.args[5].([]byte), &payload)
		out = append(out, fakeOutboxRow{e.args[2].(uuid.UUID), e.args[3].(string), e.args[4].(string), payload})
	}
	return out
}

func (t *fakeTx) promptInserts() []fakeExec {
	var out []fakeExec
	for _, e := range t.execs {
		if strings.Contains(e.sql, "user_interactions") {
			out = append(out, e)
		}
	}
	return out
}

type fakeBeginner struct{ tx *fakeTx }

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) {
	b.tx.j.add("begin")
	return b.tx, nil
}

// recordingBus records direct publishes into the journal.
type recordingBus struct {
	bus.MessageBus
	j    *journal
	sent []bus.Message
}

func (b *recordingBus) Publish(_ context.Context, topic, key string, value []byte) error {
	b.sent = append(b.sent, bus.Message{Topic: topic, Key: key, Value: value})
	b.j.add("publish:" + topic)
	return nil
}

// TransitionStateTx applies the state machine the way workgraph does: on an
// invalid transition it returns the current state and an error wrapping
// workgraph.ErrInvalidTransition.
func (f *fakeStore) TransitionStateTx(_ context.Context, tx pgx.Tx, id uuid.UUID, toState models.WorkItemState, _ string, reason string) (models.WorkItemState, error) {
	if tx == nil {
		return "", errors.New("nil tx")
	}
	it, ok := f.items[id]
	if !ok {
		return "", fmt.Errorf("work item %s not found", id)
	}
	from := it.State
	if !workgraph.IsValidTransition(from, toState) {
		return from, fmt.Errorf("%w from %s to %s", workgraph.ErrInvalidTransition, from, toState)
	}
	f.applied = append(f.applied, struct {
		ID      uuid.UUID
		ToState models.WorkItemState
		Reason  string
	}{id, toState, reason})
	if ft, ok := tx.(*fakeTx); ok {
		ft.j.add("transition:" + string(toState))
	}
	it.State = toState
	return from, nil
}

func (f *fakeStore) CreateWorkItemTx(_ context.Context, tx pgx.Tx, item *models.WorkItem) error {
	if tx == nil {
		return errors.New("nil tx")
	}
	f.created = append(f.created, item)
	if ft, ok := tx.(*fakeTx); ok {
		ft.j.add("create:" + item.ID.String())
	}
	return nil
}

type harness struct {
	j     *journal
	tx    *fakeTx
	db    *fakeBeginner
	bus   *recordingBus
	store *fakeStore
}

func newHarness(items ...*models.WorkItem) *harness {
	j := &journal{}
	st := &fakeStore{items: map[uuid.UUID]*models.WorkItem{}, previous: map[uuid.UUID]models.WorkItemState{}}
	tx := &fakeTx{j: j, store: st}
	for _, it := range items {
		st.items[it.ID] = it
	}
	return &harness{j: j, tx: tx, db: &fakeBeginner{tx: tx}, bus: &recordingBus{j: j}, store: st}
}

func productItem(state models.WorkItemState, parent *uuid.UUID) *models.WorkItem {
	pid := uuid.New()
	return &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: state, Title: "item", ProductID: &pid, ParentID: parent}
}

func indexOf(entries []string, want string) int {
	for i, e := range entries {
		if e == want {
			return i
		}
	}
	return -1
}

// ─── transitionAndPublish ───────────────────────────────────────────────────

// TestTransitionAndPublish_EffectsShareTheTransitionCommit: the transition and
// every effect run in one transaction; state.changed is published directly
// and only after the commit, with the from-state read under the row lock.
func TestTransitionAndPublish_EffectsShareTheTransitionCommit(t *testing.T) {
	item := productItem(models.WorkItemStateHumanReview, nil)
	h := newHarness(item)
	msg := qaReadyMessage{WorkID: item.ID.String(), Title: "t"}

	err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, item.ID, models.WorkItemStateQA, "review approved, auto-merge allowed",
		enqueueEffect(bus.TopicQAReady, item.ID.String(), msg))
	if err != nil {
		t.Fatalf("transitionAndPublish: %v", err)
	}
	if !h.tx.committed || h.tx.rolledBack {
		t.Fatalf("committed=%t rolledBack=%t, want a single commit", h.tx.committed, h.tx.rolledBack)
	}
	rows := h.tx.outboxRows()
	if len(rows) != 1 || rows[0].Topic != bus.TopicQAReady || rows[0].Key != item.ID.String() || rows[0].Aggregate != item.ID {
		t.Fatalf("outbox rows = %+v, want one qa.ready keyed and aggregated by the item", rows)
	}
	if rows[0].Payload["work_id"] != item.ID.String() || rows[0].Payload[outbox.FieldEventID] == nil {
		t.Errorf("payload = %v, want the qa.ready message plus event_id", rows[0].Payload)
	}
	entries := h.j.list()
	commit, pub := indexOf(entries, "commit"), indexOf(entries, "publish:state.changed")
	if commit < 0 || pub < 0 || pub < commit {
		t.Fatalf("journal = %v, want state.changed published after commit", entries)
	}
	if indexOf(entries, "transition:qa") > commit {
		t.Errorf("journal = %v, transition must precede commit", entries)
	}
	var ev stateChangedEvent
	if err := json.Unmarshal(h.bus.sent[0].Value, &ev); err != nil {
		t.Fatalf("decode state.changed: %v", err)
	}
	if ev.FromState != string(models.WorkItemStateHumanReview) || ev.ToState != string(models.WorkItemStateQA) || ev.WorkID != item.ID.String() {
		t.Errorf("state.changed = %+v", ev)
	}
	if h.bus.sent[0].Key != item.ID.String() {
		t.Errorf("state.changed key = %q", h.bus.sent[0].Key)
	}
}

// TestTransitionAndPublish_EffectFailureRollsBackEverything: an effect error
// rolls the transition back and nothing is published.
func TestTransitionAndPublish_EffectFailureRollsBackEverything(t *testing.T) {
	item := productItem(models.WorkItemStateHumanReview, nil)
	h := newHarness(item)
	boom := errors.New("outbox down")
	h.tx.execErr, h.tx.execErrOn = boom, "outbox_events"

	err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, item.ID, models.WorkItemStateQA, "r",
		enqueueEffect(bus.TopicQAReady, item.ID.String(), qaReadyMessage{WorkID: item.ID.String()}))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the enqueue failure", err)
	}
	if h.tx.committed || !h.tx.rolledBack {
		t.Errorf("committed=%t rolledBack=%t, want rollback", h.tx.committed, h.tx.rolledBack)
	}
	if len(h.bus.sent) != 0 {
		t.Errorf("published %d messages after a rollback, want 0", len(h.bus.sent))
	}
}

// TestTransitionAndPublish_InvalidTransitionRunsNoEffect keeps the old
// contract: an invalid transition is an error and no side effect happens.
func TestTransitionAndPublish_InvalidTransitionRunsNoEffect(t *testing.T) {
	item := productItem(models.WorkItemStatePassed, nil)
	h := newHarness(item)
	ran := false
	err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, item.ID, models.WorkItemStateQA, "r",
		func(context.Context, pgx.Tx, *models.WorkItem) error { ran = true; return nil })
	if !errors.Is(err, workgraph.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if ran || len(h.bus.sent) != 0 || h.tx.committed {
		t.Errorf("ran=%t sent=%d committed=%t after an invalid transition", ran, len(h.bus.sent), h.tx.committed)
	}
}

// TestTransitionAndPublish_SubtaskEffectsUseTheParentAggregate: a subtask's
// blocked prompt is ordered under its parent; key and work item stay its own.
func TestTransitionAndPublish_SubtaskEffectsUseTheParentAggregate(t *testing.T) {
	parent := uuid.New()
	sub := productItem(models.WorkItemStateImplementation, &parent)
	sub.Type = models.WorkItemTypeSubtask
	h := newHarness(sub)

	if err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, sub.ID, models.WorkItemStateBlocked, "no evidence",
		blockedPromptEffect("no evidence")); err != nil {
		t.Fatalf("transitionAndPublish: %v", err)
	}
	rows := h.tx.outboxRows()
	if len(rows) != 1 || rows[0].Topic != bus.TopicUserPromptRequested {
		t.Fatalf("outbox rows = %+v, want one user_prompt.requested", rows)
	}
	if rows[0].Aggregate != parent || rows[0].Key != sub.ID.String() {
		t.Errorf("aggregate=%s key=%s, want parent %s and key %s", rows[0].Aggregate, rows[0].Key, parent, sub.ID)
	}
	if len(h.tx.promptInserts()) != 1 {
		t.Errorf("prompt rows = %d, want 1 in the same tx", len(h.tx.promptInserts()))
	}
	if rows[0].Payload["trigger"] != string(interactions.TriggerBlocked) || rows[0].Payload["prompt_type"] != string(interactions.PromptTypeEscalation) {
		t.Errorf("prompt payload = %v, want the blocked escalation prompt", rows[0].Payload)
	}
}

// TestPromptEffect_ItemWithoutProductSkipsThePrompt: user_interactions needs a
// product; an item without one keeps its transition and gets no prompt
// instead of rolling the transition back.
func TestPromptEffect_ItemWithoutProductSkipsThePrompt(t *testing.T) {
	item := productItem(models.WorkItemStateImplementation, nil)
	item.ProductID = nil
	h := newHarness(item)
	if err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, item.ID, models.WorkItemStateBlocked, "r",
		blockedPromptEffect("r")); err != nil {
		t.Fatalf("transitionAndPublish: %v", err)
	}
	if !h.tx.committed || len(h.tx.promptInserts()) != 0 || len(h.tx.outboxRows()) != 0 {
		t.Errorf("committed=%t prompts=%d events=%d, want the transition alone", h.tx.committed, len(h.tx.promptInserts()), len(h.tx.outboxRows()))
	}
}

// ─── work.blocked ───────────────────────────────────────────────────────────

// TestBlockFromWorkBlocked is the ruling for converted guard blocks: an item
// that is already blocked is not transitioned again (and is not an error),
// and the generic escalation prompt is raised only when no escalation prompt
// is pending for the item.
func TestBlockFromWorkBlocked(t *testing.T) {
	for _, tc := range []struct {
		name           string
		state          models.WorkItemState
		previous       models.WorkItemState // previous_state; "" = NULL
		pending        bool
		wantTransition bool
		wantPrompt     bool
	}{
		{"running item, no prompt yet", models.WorkItemStateImplementation, "", false, true, true},
		{"running item, escalation already pending", models.WorkItemStateImplementation, "", true, true, false},
		{"already blocked, no prompt pending", models.WorkItemStateBlocked, "", false, false, true},
		{"already blocked, prompt pending", models.WorkItemStateBlocked, "", true, false, false},
		// The producer's own escalation prompt was consumed before this
		// event and parked the blocked item in awaiting_user: it is already
		// blocked, and its pending prompt suppresses the generic one.
		{"awaiting_user parked from blocked, prompt pending", models.WorkItemStateAwaitingUser, models.WorkItemStateBlocked, true, false, false},
		{"awaiting_user parked from blocked, no prompt pending", models.WorkItemStateAwaitingUser, models.WorkItemStateBlocked, false, false, true},
		// awaiting_user parked from any other state is a real block.
		{"awaiting_user parked from implementation", models.WorkItemStateAwaitingUser, models.WorkItemStateImplementation, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := productItem(tc.state, nil)
			h := newHarness(item)
			h.tx.pendingEscalation = tc.pending
			if tc.previous != "" {
				h.store.previous[item.ID] = tc.previous
			}

			if err := blockFromWorkBlocked(context.Background(), h.db, h.store, h.bus, item.ID, "guard said no"); err != nil {
				t.Fatalf("blockFromWorkBlocked: %v", err)
			}
			if !h.tx.committed {
				t.Fatal("transaction not committed")
			}
			if got := len(h.store.applied) == 1; got != tc.wantTransition {
				t.Errorf("transitioned = %t, want %t", got, tc.wantTransition)
			}
			if got := len(h.bus.sent) == 1; got != tc.wantTransition {
				t.Errorf("state.changed published = %t, want %t", got, tc.wantTransition)
			}
			if !tc.wantTransition && item.State != tc.state {
				t.Errorf("state = %s, want it left at %s", item.State, tc.state)
			}
			if entries := h.j.list(); indexOf(entries, "lock") < 0 {
				t.Errorf("journal = %v, want the row lock read", entries)
			}
			if got := len(h.tx.promptInserts()) == 1; got != tc.wantPrompt {
				t.Errorf("prompt raised = %t, want %t", got, tc.wantPrompt)
			}
			rows := h.tx.outboxRows()
			if tc.wantPrompt && (len(rows) != 1 || rows[0].Topic != bus.TopicUserPromptRequested) {
				t.Errorf("outbox rows = %+v, want the prompt event", rows)
			}
			if !tc.wantPrompt && len(rows) != 0 {
				t.Errorf("outbox rows = %+v, want none", rows)
			}
		})
	}
}

// TestBlockFromWorkBlocked_TerminalItemIsAnError: a cancelled item cannot be
// blocked; nothing is written and the caller logs it as before.
func TestBlockFromWorkBlocked_TerminalItemIsAnError(t *testing.T) {
	item := productItem(models.WorkItemStateCancelled, nil)
	h := newHarness(item)
	err := blockFromWorkBlocked(context.Background(), h.db, h.store, h.bus, item.ID, "r")
	if !errors.Is(err, workgraph.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if h.tx.committed || len(h.tx.execs) != 0 || len(h.bus.sent) != 0 {
		t.Errorf("committed=%t execs=%d sent=%d", h.tx.committed, len(h.tx.execs), len(h.bus.sent))
	}
}

// ─── fix subtasks ───────────────────────────────────────────────────────────

func TestDispatchFixSubtask_RowAndEventInOneTx(t *testing.T) {
	parent := productItem(models.WorkItemStateImplementation, nil)
	h := newHarness(parent)
	fix := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeSubtask, State: models.WorkItemStateImplementation, ParentID: &parent.ID, Title: "Address review feedback"}
	msg := map[string]interface{}{"task_id": fix.ID.String(), "work_id": parent.ID.String(), "is_review_fix": true}

	if err := dispatchFixSubtask(context.Background(), h.db, h.store, parent.ID, fix, msg); err != nil {
		t.Fatalf("dispatchFixSubtask: %v", err)
	}
	if !h.tx.committed || len(h.store.created) != 1 {
		t.Fatalf("committed=%t created=%d", h.tx.committed, len(h.store.created))
	}
	rows := h.tx.outboxRows()
	if len(rows) != 1 || rows[0].Topic != bus.TopicImplementationTasks || rows[0].Key != parent.ID.String() || rows[0].Aggregate != parent.ID {
		t.Fatalf("outbox rows = %+v, want implementation.tasks keyed and aggregated by the parent", rows)
	}
	if rows[0].Payload["task_id"] != fix.ID.String() || rows[0].Payload["is_review_fix"] != true {
		t.Errorf("payload = %v", rows[0].Payload)
	}
	entries := h.j.list()
	if indexOf(entries, "create:"+fix.ID.String()) > indexOf(entries, "commit") {
		t.Errorf("journal = %v", entries)
	}
}

func TestDispatchFixSubtask_FailureLeavesNeither(t *testing.T) {
	parent := productItem(models.WorkItemStateImplementation, nil)
	h := newHarness(parent)
	boom := errors.New("outbox down")
	h.tx.execErr, h.tx.execErrOn = boom, "outbox_events"
	fix := &models.WorkItem{ID: uuid.New(), ParentID: &parent.ID}
	err := dispatchFixSubtask(context.Background(), h.db, h.store, parent.ID, fix, map[string]interface{}{"task_id": fix.ID.String()})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if h.tx.committed || !h.tx.rolledBack {
		t.Errorf("committed=%t rolledBack=%t", h.tx.committed, h.tx.rolledBack)
	}
}

// sequenceBeginner hands out a fresh fake transaction per Begin, so a test can
// see a failed transaction followed by a second one.
type sequenceBeginner struct {
	txs  []*fakeTx
	next int
}

func (b *sequenceBeginner) Begin(context.Context) (pgx.Tx, error) {
	tx := b.txs[b.next]
	b.next++
	tx.j.add("begin")
	return tx, nil
}

// TestDispatchFixOrBlock_FailureBlocksTheParent: a fix dispatch that cannot
// commit is reported by blocking the parent with the reason and the blocked
// prompt, never by carrying on as if the fix had been dispatched.
func TestDispatchFixOrBlock_FailureBlocksTheParent(t *testing.T) {
	parent := productItem(models.WorkItemStateImplementation, nil)
	h := newHarness(parent)
	boom := errors.New("outbox down")
	failing := &fakeTx{j: h.j, execErr: boom, execErrOn: "outbox_events"}
	second := &fakeTx{j: h.j}
	db := &sequenceBeginner{txs: []*fakeTx{failing, second}}
	fix := &models.WorkItem{ID: uuid.New(), ParentID: &parent.ID, Title: "Fix QA-reported bugs"}

	dispatchFixOrBlock(context.Background(), db, h.store, h.bus, "qa.bugs", parent.ID, fix, map[string]interface{}{"task_id": fix.ID.String()})

	if failing.committed || !failing.rolledBack {
		t.Fatalf("fix transaction committed=%t rolledBack=%t", failing.committed, failing.rolledBack)
	}
	if !second.committed || parent.State != models.WorkItemStateBlocked {
		t.Fatalf("parent state = %s (block tx committed=%t), want blocked", parent.State, second.committed)
	}
	if len(second.promptInserts()) != 1 {
		t.Errorf("blocked prompt rows = %d, want 1", len(second.promptInserts()))
	}
	if len(h.bus.sent) != 1 || h.bus.sent[0].Topic != topicStateChanged {
		t.Errorf("sent = %+v, want one state.changed for the block", h.bus.sent)
	}
	last := h.store.applied[len(h.store.applied)-1]
	if !strings.Contains(last.Reason, "could not dispatch fix subtask") {
		t.Errorf("block reason %q must say the fix was not dispatched", last.Reason)
	}
	// The raw database error goes to the log, never into the reason or the
	// user-visible prompt.
	if strings.Contains(last.Reason, "outbox down") {
		t.Errorf("block reason %q leaks the raw error", last.Reason)
	}
	for _, p := range second.promptInserts() {
		for _, a := range p.args {
			if s, ok := a.(string); ok && strings.Contains(s, "outbox down") {
				t.Errorf("prompt field %q leaks the raw error", s)
			}
		}
	}
}

// ─── standalone events ──────────────────────────────────────────────────────

// TestEnqueueAlone_CodexUpdated: an event with no accompanying write still
// goes through the outbox, in a short transaction of its own.
func TestEnqueueAlone_CodexUpdated(t *testing.T) {
	h := newHarness()
	workID := uuid.New()
	payload := map[string]string{"trigger": "post_merge", "repository_id": "", "since": "HEAD~1"}
	if err := enqueueAlone(context.Background(), h.db, workID, bus.TopicCodexUpdated, workID.String(), payload); err != nil {
		t.Fatalf("enqueueAlone: %v", err)
	}
	rows := h.tx.outboxRows()
	if !h.tx.committed || len(rows) != 1 || rows[0].Topic != bus.TopicCodexUpdated || rows[0].Key != workID.String() || rows[0].Payload["trigger"] != "post_merge" {
		t.Fatalf("committed=%t rows=%+v", h.tx.committed, rows)
	}
}

// TestRaisePromptAlone keeps the legacy "prompt even when the transition
// failed" branches: the prompt commits in its own transaction.
func TestRaisePromptAlone(t *testing.T) {
	item := productItem(models.WorkItemStateHumanReview, nil)
	h := newHarness(item)
	if err := raisePromptAlone(context.Background(), h.db, h.store, item.ID, humanReviewPromptEffect("check it")); err != nil {
		t.Fatalf("raisePromptAlone: %v", err)
	}
	rows := h.tx.outboxRows()
	if !h.tx.committed || len(h.tx.promptInserts()) != 1 || len(rows) != 1 || rows[0].Payload["trigger"] != string(interactions.TriggerApprovalHumanReview) {
		t.Fatalf("committed=%t prompts=%d rows=%+v", h.tx.committed, len(h.tx.promptInserts()), rows)
	}
	if len(h.store.applied) != 0 || len(h.bus.sent) != 0 {
		t.Errorf("raisePromptAlone transitioned or published state.changed")
	}
}
