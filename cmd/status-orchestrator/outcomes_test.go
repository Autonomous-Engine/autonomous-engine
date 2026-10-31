package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/idempotency"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outcomes"
	"github.com/google/uuid"
)

// M4-05: the outcome upsert is a txEffect of the transition that produced
// the fact.

func (t *fakeTx) outcomeUpserts() []fakeExec {
	var out []fakeExec
	for _, e := range t.execs {
		if strings.Contains(e.sql, "INSERT INTO work_item_outcomes") {
			out = append(out, e)
		}
	}
	return out
}

// The subtask's release decision commits with the subtask's transition and
// is keyed by the parent (the top-level item the outcome describes).
func TestOutcomeEffect_SharesTheTransitionCommitAndTargetsTheParent(t *testing.T) {
	parentID := uuid.New()
	sub := productItem(models.WorkItemStateImplementation, &parentID)
	h := newHarness(sub)

	err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, sub.ID, models.WorkItemStatePassed, "implementation complete: verified",
		outcomeEffect(outcomes.Facts{TaskID: sub.ID.String(), Decision: subtaskDecision(models.WorkItemStatePassed, true)}))
	if err != nil {
		t.Fatalf("transitionAndPublish: %v", err)
	}
	ups := h.tx.outcomeUpserts()
	if len(ups) != 1 {
		t.Fatalf("outcome upserts = %d, want 1", len(ups))
	}
	args := ups[0].args
	if args[0] != parentID {
		t.Errorf("outcome row = %v, want the parent %s", args[0], parentID)
	}
	if args[1] != sub.ID.String() || args[2] != outcomes.DecisionRelease {
		t.Errorf("decision args = %v %v", args[1], args[2])
	}
	entries := h.j.list()
	if tr, commit := indexOf(entries, "transition:passed"), indexOf(entries, "commit"); tr < 0 || commit < tr {
		t.Fatalf("journal = %v, want the transition before the single commit", entries)
	}
	if !h.tx.committed || h.tx.rolledBack {
		t.Fatalf("committed=%t rolledBack=%t", h.tx.committed, h.tx.rolledBack)
	}
}

// A failed outcome write rolls the transition back: the fact and the state
// change never diverge.
func TestOutcomeEffect_FailureRollsBackTheTransition(t *testing.T) {
	item := productItem(models.WorkItemStateQA, nil)
	h := newHarness(item)
	boom := errors.New("outcomes down")
	h.tx.execErr, h.tx.execErrOn = boom, "work_item_outcomes"

	err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, item.ID, models.WorkItemStatePassed, "QA passed",
		outcomeEffect(outcomes.Facts{QAResult: outcomes.QAPass}))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the outcome failure", err)
	}
	if h.tx.committed || !h.tx.rolledBack || len(h.bus.sent) != 0 {
		t.Fatalf("committed=%t rolledBack=%t published=%d", h.tx.committed, h.tx.rolledBack, len(h.bus.sent))
	}
}

// QA and merge facts reach the statement; a top-level item is its own row.
func TestOutcomeEffect_QAAndMergeFacts(t *testing.T) {
	item := productItem(models.WorkItemStateHumanReview, nil)
	h := newHarness(item)
	merged := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	m := mergeFacts(prMergedPayload{PRNumber: 42, Repository: "acme/app", HeadSHA: "h1", MergeCommitSHA: "m1", MergedAt: merged.Format(time.RFC3339)})
	if err := transitionAndPublish(context.Background(), h.db, h.store, h.bus, item.ID, models.WorkItemStateQA, "PR merged",
		outcomeEffect(outcomes.Facts{Merge: m})); err != nil {
		t.Fatal(err)
	}
	args := h.tx.outcomeUpserts()[0].args
	want := []any{item.ID, nil, nil, nil, 42, "acme/app", "h1", "m1", merged}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v\nwant %#v", args, want)
	}
}

// A held pr.merged records the merge without consuming the delivery's
// guard, so the codex.updated enqueue that follows carries it.
func TestRecordHeldMerge_LeavesTheGuardForCodexUpdated(t *testing.T) {
	item := productItem(models.WorkItemStateHumanReview, nil)
	h := newHarness(item)
	msg := &bus.Message{Topic: bus.TopicPRMerged, Headers: map[string]string{bus.HeaderEventID: uuid.NewString()}}
	d := idempotency.NewLedger(nil).NewDelivery(idempotencyConsumer, msg)
	ctx := idempotency.WithDelivery(context.Background(), d)

	// handlePRMerged passes the raw pool (wg.DB()); the helpers wrap it.
	recordHeldMerge(ctx, h.db, h.store, item.ID, outcomeEffect(outcomes.Facts{Merge: &outcomes.Merge{PRNumber: 3, MergeSHA: "m"}}))
	if len(h.tx.outcomeUpserts()) != 1 || !h.tx.committed {
		t.Fatalf("merge not recorded: upserts=%d committed=%t", len(h.tx.outcomeUpserts()), h.tx.committed)
	}
	if guards := h.tx.guardInserts(); guards != 0 || d.Recorded() {
		t.Fatalf("held merge consumed the guard (inserts=%d recorded=%t)", guards, d.Recorded())
	}

	if err := enqueueAlone(ctx, h.db, item.ID, bus.TopicCodexUpdated, item.ID.String(), map[string]string{"trigger": "post_merge"}); err != nil {
		t.Fatal(err)
	}
	if guards := h.tx.guardInserts(); guards != 1 || !d.Recorded() {
		t.Fatalf("codex.updated did not carry the guard (inserts=%d recorded=%t)", guards, d.Recorded())
	}
}

func (t *fakeTx) guardInserts() int {
	n := 0
	for _, e := range t.execs {
		if strings.Contains(e.sql, "processed_events") {
			n++
		}
	}
	return n
}

func TestSubtaskDecision(t *testing.T) {
	cases := []struct {
		to       models.WorkItemState
		evidence bool
		want     string
	}{
		{models.WorkItemStatePassed, true, outcomes.DecisionRelease},
		{models.WorkItemStatePassed, false, outcomes.DecisionLegacyRelease},
		{models.WorkItemStateHumanReview, true, outcomes.DecisionHumanReview},
		{models.WorkItemStateBlocked, true, outcomes.DecisionBlock},
		{models.WorkItemStateBlocked, false, outcomes.DecisionBlock},
	}
	for _, c := range cases {
		if got := subtaskDecision(c.to, c.evidence); got != c.want {
			t.Errorf("subtaskDecision(%s, %t) = %q, want %q", c.to, c.evidence, got, c.want)
		}
	}
}

func TestMergeFacts(t *testing.T) {
	if mergeFacts(prMergedPayload{WorkID: "x", Title: "t"}) != nil {
		t.Fatal("a pre-M4-05 payload must record no merge")
	}
	m := mergeFacts(prMergedPayload{PRNumber: 7, Timestamp: "2026-10-03T10:00:00Z"})
	if m == nil || m.MergedAt == nil || m.MergedAt.Hour() != 10 {
		t.Fatalf("merge = %+v, want merged_at from the event timestamp", m)
	}
	m = mergeFacts(prMergedPayload{PRNumber: 7, MergedAt: "2026-10-03T09:00:00Z", Timestamp: "2026-10-03T10:00:00Z"})
	if m.MergedAt.Hour() != 9 {
		t.Fatalf("merged_at = %v, want the PR's merged_at over the event time", m.MergedAt)
	}
}
