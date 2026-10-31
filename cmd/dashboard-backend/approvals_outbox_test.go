package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// outboxTx is a pgx.Tx that records statements and the outbox inserts made on
// it. QueryRow answers TransitionFromAwaitingUserToStateTx's locking select
// with state. Unexpected methods panic through the nil embedded interface.
type outboxTx struct {
	pgx.Tx
	state models.WorkItemState

	sqls      []string
	args      [][]any
	commits   int
	rollbacks int

	// The outbox inserts: calls counts them; the rest are the last one's.
	calls     int
	aggregate uuid.UUID
	topic     string
	key       string
	payload   []byte
	err       error // returned by the outbox insert
}

func (f *outboxTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	if strings.Contains(sql, "INSERT INTO outbox_events") {
		f.calls++
		f.aggregate, _ = args[2].(uuid.UUID)
		f.topic, _ = args[3].(string)
		f.key, _ = args[4].(string)
		f.payload, _ = args[5].([]byte)
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.CommandTag{}, nil
}

type stateRow struct{ s models.WorkItemState }

func (r stateRow) Scan(dest ...any) error {
	*(dest[0].(*models.WorkItemState)) = r.s
	return nil
}

func (f *outboxTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.sqls = append(f.sqls, sql)
	f.args = append(f.args, args)
	return stateRow{f.state}
}

func (f *outboxTx) Commit(context.Context) error   { f.commits++; return nil }
func (f *outboxTx) Rollback(context.Context) error { f.rollbacks++; return nil }

type txBeginnerFake struct {
	tx     *outboxTx
	begins int
}

func (b *txBeginnerFake) Begin(context.Context) (pgx.Tx, error) {
	b.begins++
	return b.tx, nil
}

type transitionCall struct {
	tx     pgx.Tx
	id     uuid.UUID
	to     models.WorkItemState
	reason string
}

// approvalsFake serves one work item and its artifacts and records
// transitions, which must arrive on the handler's transaction.
type approvalsFake struct {
	item        *models.WorkItem
	artifacts   map[models.ArtifactType][]*models.WorkItemArtifact
	transitions []transitionCall
	transErr    error
}

func (f *approvalsFake) GetWorkItem(_ context.Context, id uuid.UUID) (*models.WorkItem, error) {
	if f.item == nil || f.item.ID != id {
		return nil, errors.New("not found")
	}
	cp := *f.item
	return &cp, nil
}

func (f *approvalsFake) GetArtifacts(_ context.Context, _ uuid.UUID, t models.ArtifactType) ([]*models.WorkItemArtifact, error) {
	return f.artifacts[t], nil
}

func (f *approvalsFake) TransitionStateTx(_ context.Context, tx pgx.Tx, id uuid.UUID, to models.WorkItemState, _ string, reason string) (models.WorkItemState, error) {
	f.transitions = append(f.transitions, transitionCall{tx: tx, id: id, to: to, reason: reason})
	if f.transErr != nil {
		return "", f.transErr
	}
	return models.WorkItemStateBlocked, nil
}

func (f *approvalsFake) TransitionFromAwaitingUserToStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, to models.WorkItemState, agentID, reason string) error {
	f.transitions = append(f.transitions, transitionCall{tx: tx, id: id, to: to, reason: reason})
	return f.transErr
}

type approvalHarness struct {
	h   *Handlers
	g   *approvalsFake
	db  *txBeginnerFake
	tx  *outboxTx
	itm *models.WorkItem
}

func newApprovalHarness(item *models.WorkItem) *approvalHarness {
	tx := &outboxTx{state: models.WorkItemStateAwaitingUser}
	db := &txBeginnerFake{tx: tx}
	g := &approvalsFake{item: item, artifacts: map[models.ArtifactType][]*models.WorkItemArtifact{}}
	return &approvalHarness{h: &Handlers{approvals: g, txdb: db}, g: g, db: db, tx: tx, itm: item}
}

// assertOneCommittedEvent checks the M1-04b contract for an approval: one
// transaction, committed, the transition on it, and exactly one event.
func (a *approvalHarness) assertOneCommittedEvent(t *testing.T, to models.WorkItemState, topic, key string, aggregate uuid.UUID) map[string]any {
	t.Helper()
	if a.db.begins != 1 || a.tx.commits != 1 || a.tx.rollbacks != 0 {
		t.Fatalf("begins=%d commits=%d rollbacks=%d, want one committed transaction", a.db.begins, a.tx.commits, a.tx.rollbacks)
	}
	if len(a.g.transitions) != 1 || a.g.transitions[0].tx != a.tx || a.g.transitions[0].to != to {
		t.Fatalf("transitions = %+v, want one to %s on the handler's tx", a.g.transitions, to)
	}
	if a.tx.calls != 1 {
		t.Fatalf("outbox inserts = %d, want 1", a.tx.calls)
	}
	if a.tx.topic != topic || a.tx.key != key || a.tx.aggregate != aggregate {
		t.Errorf("event topic/key/aggregate = %s/%s/%s, want %s/%s/%s", a.tx.topic, a.tx.key, a.tx.aggregate, topic, key, aggregate)
	}
	var body map[string]any
	if err := json.Unmarshal(a.tx.payload, &body); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if body[outbox.FieldEventID] == nil {
		t.Errorf("payload has no event_id: %v", body)
	}
	return body
}

func prdArtifact(t *testing.T) *models.WorkItemArtifact {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"prd": "the PRD", "work_type": "feature"})
	return &models.WorkItemArtifact{Type: models.ArtifactTypePRD, Content: b}
}

func post(h http.HandlerFunc, url string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, url, strings.NewReader(body)))
	return rec
}

func blockedSubtask() *models.WorkItem {
	parent, product, repo := uuid.New(), uuid.New(), uuid.New()
	return &models.WorkItem{
		ID: uuid.New(), Type: models.WorkItemTypeSubtask, State: models.WorkItemStateBlocked,
		ParentID: &parent, ProductID: &product, RepositoryID: &repo, Title: "t", Description: "d",
	}
}

// ─── RetryWorkItem ───────────────────────────────────────────────────────────

func TestRetryWorkItem_SubtaskRetryEnqueuesImplementationTasks(t *testing.T) {
	item := blockedSubtask()
	a := newApprovalHarness(item)
	rec := post(a.h.RetryWorkItem, "/api/work-items/retry?id="+item.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateImplementation, bus.TopicImplementationTasks, item.ID.String(), *item.ParentID)
	if body["work_id"] != item.ParentID.String() || body["task_id"] != item.ID.String() || body["retry"] != true {
		t.Errorf("payload = %v", body)
	}
}

func TestRetryWorkItem_WithPRDEnqueuesPRDReady(t *testing.T) {
	product := uuid.New()
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateFailed, ProductID: &product}
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypePRD] = []*models.WorkItemArtifact{prdArtifact(t)}
	rec := post(a.h.RetryWorkItem, "/api/work-items/retry?id="+item.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStatePRD, bus.TopicPRDReady, item.ID.String(), item.ID)
	if body["prd_content"] != "the PRD" || body["work_type"] != "feature" || body["retry"] != true || body["product_id"] != product.String() {
		t.Errorf("payload = %v", body)
	}
}

func TestRetryWorkItem_WithoutPRDEnqueuesIntake(t *testing.T) {
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateBlocked, Description: "desc"}
	a := newApprovalHarness(item)
	rec := post(a.h.RetryWorkItem, "/api/work-items/retry?id="+item.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateIntake, bus.TopicIntake, item.ID.String(), item.ID)
	if body["description"] != "desc" || body["source"] != "retry" {
		t.Errorf("payload = %v", body)
	}
}

// A failed enqueue rolls the transition back and fails the request; before the
// conversion it was logged and the request reported success.
func TestRetryWorkItem_EnqueueFailureRollsBackAndFails(t *testing.T) {
	item := blockedSubtask()
	a := newApprovalHarness(item)
	a.tx.err = errors.New("outbox insert failed")
	rec := post(a.h.RetryWorkItem, "/api/work-items/retry?id="+item.ID.String(), "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if a.tx.commits != 0 || a.tx.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", a.tx.commits, a.tx.rollbacks)
	}
}

// ─── UpdateAndRetry ──────────────────────────────────────────────────────────

func TestCommitUpdateAndRetry_DescriptionStateTransitionAndIntakeShareOneTx(t *testing.T) {
	product := uuid.New()
	item := &models.WorkItem{ID: uuid.New(), State: models.WorkItemStateBlocked, ProductID: &product}
	tx := &outboxTx{}
	db := &txBeginnerFake{tx: tx}
	if err := commitUpdateAndRetry(context.Background(), db, item, "new desc", "u1"); err != nil {
		t.Fatalf("commitUpdateAndRetry: %v", err)
	}
	if db.begins != 1 || tx.commits != 1 || tx.calls != 1 {
		t.Fatalf("begins=%d commits=%d events=%d", db.begins, tx.commits, tx.calls)
	}
	want := []string{updateDescriptionSQL, forceIntakeSQL, insertIntakeTransitionSQL}
	for i, w := range want {
		if tx.sqls[i] != w {
			t.Errorf("statement %d = %q, want %q", i, tx.sqls[i], w)
		}
	}
	if tx.topic != bus.TopicIntake || tx.key != item.ID.String() || tx.aggregate != item.ID {
		t.Errorf("event %s/%s/%s", tx.topic, tx.key, tx.aggregate)
	}
	var body map[string]any
	_ = json.Unmarshal(tx.payload, &body)
	if body["description"] != "new desc" || body["source"] != "dashboard-retry" || body["product_id"] != product.String() {
		t.Errorf("payload = %v", body)
	}

	tx = &outboxTx{err: errors.New("boom")}
	if err := commitUpdateAndRetry(context.Background(), &txBeginnerFake{tx: tx}, item, "d", "u"); err == nil || tx.rollbacks != 1 || tx.commits != 0 {
		t.Errorf("enqueue failure: err=%v commits=%d rollbacks=%d", err, tx.commits, tx.rollbacks)
	}
}

// ─── ApproveWorkItem / human-review prompt answer ────────────────────────────

func humanReviewItem(t *testing.T) (*models.WorkItem, []*models.WorkItemArtifact) {
	item := &models.WorkItem{ID: uuid.New(), State: models.WorkItemStateHumanReview, Title: "Add widget", Description: "AC"}
	return item, []*models.WorkItemArtifact{reviewArtifact(t, map[string]string{"decision": "approve", "branch": "agent/w/t", "head_sha": fwdHeadSHA})}
}

func TestApproveWorkItem_TransitionAndQAReadyShareOneTx(t *testing.T) {
	item, reviews := humanReviewItem(t)
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypeReviewComments] = reviews
	rec := post(a.h.ApproveWorkItem, "/api/work-items/approve?id="+item.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateQA, bus.TopicQAReady, item.ID.String(), item.ID)
	if body["head_sha"] != fwdHeadSHA || body["branch"] != "agent/w/t" || body["title"] != "Add widget" {
		t.Errorf("payload = %v", body)
	}
}

func TestApproveWorkItem_InvalidTransitionIs400AndEnqueuesNothing(t *testing.T) {
	item, _ := humanReviewItem(t)
	a := newApprovalHarness(item)
	a.g.transErr = errors.New("invalid transition from released to qa")
	rec := post(a.h.ApproveWorkItem, "/api/work-items/approve?id="+item.ID.String(), "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if a.tx.calls != 0 || a.tx.commits != 0 || a.tx.rollbacks != 1 {
		t.Errorf("events=%d commits=%d rollbacks=%d", a.tx.calls, a.tx.commits, a.tx.rollbacks)
	}
}

func TestApproveWorkItem_EnqueueFailureIs500(t *testing.T) {
	item, _ := humanReviewItem(t)
	a := newApprovalHarness(item)
	a.tx.err = errors.New("outbox insert failed")
	rec := post(a.h.ApproveWorkItem, "/api/work-items/approve?id="+item.ID.String(), "")
	if rec.Code != http.StatusInternalServerError || a.tx.commits != 0 {
		t.Errorf("status = %d commits=%d, want 500 and no commit", rec.Code, a.tx.commits)
	}
}

func TestHumanReviewPromptAnswer_TransitionAndQAReadyShareOneTx(t *testing.T) {
	item, reviews := humanReviewItem(t)
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypeReviewComments] = reviews
	if err := a.h.HandleHumanReviewPromptAnswer(context.Background(), item.ID, "approve", "", "u"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateQA, bus.TopicQAReady, item.ID.String(), item.ID)
	if body["head_sha"] != fwdHeadSHA {
		t.Errorf("payload = %v", body)
	}
}

// ─── PRD approval ────────────────────────────────────────────────────────────

func TestApprovePRD_TransitionAndPRDReadyShareOneTx(t *testing.T) {
	product := uuid.New()
	item := &models.WorkItem{ID: uuid.New(), State: models.WorkItemStatePRDApproval, ProductID: &product}
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypePRD] = []*models.WorkItemArtifact{prdArtifact(t)}
	rec := post(a.h.ApprovePRD, "/api/work-items/approve-prd?id="+item.ID.String(), `{"guidance":"  go  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateResearch, bus.TopicPRDReady, item.ID.String(), item.ID)
	if body["prd_content"] != "the PRD" || body["user_guidance"] != "go" || body["product_id"] != product.String() {
		t.Errorf("payload = %v", body)
	}
}

func TestPRDPromptAnswer_AwaitingUserTransitionAndPRDReadyShareOneTx(t *testing.T) {
	item := &models.WorkItem{ID: uuid.New(), State: models.WorkItemStateAwaitingUser}
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypePRD] = []*models.WorkItemArtifact{prdArtifact(t)}
	if err := a.h.HandlePRDPromptAnswer(context.Background(), item.ID, "approve", "focus on X", "u"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateResearch, bus.TopicPRDReady, item.ID.String(), item.ID)
	if body["user_guidance"] != "focus on X" {
		t.Errorf("payload = %v", body)
	}
}

func TestPRDPromptAnswer_EnqueueFailureReturnsError(t *testing.T) {
	item := &models.WorkItem{ID: uuid.New(), State: models.WorkItemStatePRDApproval}
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypePRD] = []*models.WorkItemArtifact{prdArtifact(t)}
	a.tx.err = errors.New("outbox insert failed")
	if err := a.h.HandlePRDPromptAnswer(context.Background(), item.ID, "approve", "", "u"); err == nil {
		t.Fatal("answer succeeded with a failed enqueue")
	}
	if a.tx.commits != 0 || a.tx.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", a.tx.commits, a.tx.rollbacks)
	}
}

// ─── Blocked prompt retry ────────────────────────────────────────────────────

func TestBlockedPromptAnswer_SubtaskRetryEnqueuesImplementationTasks(t *testing.T) {
	item := blockedSubtask()
	a := newApprovalHarness(item)
	if err := a.h.HandleBlockedPromptAnswer(context.Background(), item.ID, "retry", "", "u"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateImplementation, bus.TopicImplementationTasks, item.ID.String(), *item.ParentID)
	if body["work_id"] != item.ParentID.String() || body["repository_id"] != item.RepositoryID.String() {
		t.Errorf("payload = %v", body)
	}
}

// A non-subtask blocked-prompt retry moves the item to research AND enqueues
// the prd.ready research-coordinator starts on, in one transaction.
func TestBlockedPromptAnswer_NonSubtaskRetryEnqueuesPRDReady(t *testing.T) {
	product := uuid.New()
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateAwaitingUser, ProductID: &product}
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypePRD] = []*models.WorkItemArtifact{prdArtifact(t)}
	if err := a.h.HandleBlockedPromptAnswer(context.Background(), item.ID, "retry", " try the other API ", "u"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	body := a.assertOneCommittedEvent(t, models.WorkItemStateResearch, bus.TopicPRDReady, item.ID.String(), item.ID)
	if body["work_id"] != item.ID.String() || body["prd_content"] != "the PRD" || body["work_type"] != "feature" ||
		body["product_id"] != product.String() || body["user_guidance"] != "try the other API" || body["retry"] != true {
		t.Errorf("payload = %v", body)
	}
}

func TestBlockedPromptAnswer_NonSubtaskRetryWithoutPRDChangesNothing(t *testing.T) {
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateAwaitingUser}
	a := newApprovalHarness(item)
	err := a.h.HandleBlockedPromptAnswer(context.Background(), item.ID, "retry", "", "u")
	if err == nil || !strings.Contains(err.Error(), "no PRD") {
		t.Fatalf("err = %v, want a no-PRD error", err)
	}
	if a.db.begins != 0 || len(a.g.transitions) != 0 || a.tx.calls != 0 {
		t.Errorf("begins=%d transitions=%d events=%d, want nothing", a.db.begins, len(a.g.transitions), a.tx.calls)
	}
}

func TestBlockedPromptAnswer_NonSubtaskRetryEnqueueFailureRollsBack(t *testing.T) {
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateAwaitingUser}
	a := newApprovalHarness(item)
	a.g.artifacts[models.ArtifactTypePRD] = []*models.WorkItemArtifact{prdArtifact(t)}
	a.tx.err = errors.New("outbox insert failed")
	if err := a.h.HandleBlockedPromptAnswer(context.Background(), item.ID, "retry", "", "u"); err == nil {
		t.Fatal("answer succeeded with a failed enqueue")
	}
	if a.tx.commits != 0 || a.tx.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", a.tx.commits, a.tx.rollbacks)
	}
}

// ─── Gated pr.ready ──────────────────────────────────────────────────────────

func TestCommitGatedPRReady_OneTxOneEvent(t *testing.T) {
	tx := &outboxTx{}
	db := &txBeginnerFake{tx: tx}
	pc := storedChange()
	if err := commitGatedPRReady(context.Background(), db, pc, "prod-1", 42, "https://github.com/o/r/pull/42", time.Now()); err != nil {
		t.Fatalf("commitGatedPRReady: %v", err)
	}
	if db.begins != 1 || tx.commits != 1 || tx.calls != 1 || len(tx.sqls) != 1 {
		t.Fatalf("begins=%d commits=%d events=%d statements=%d", db.begins, tx.commits, tx.calls, len(tx.sqls))
	}
	if tx.topic != bus.TopicPRReady || tx.key != fwdWorkID || tx.aggregate != uuid.MustParse(fwdWorkID) {
		t.Errorf("event %s/%s/%s", tx.topic, tx.key, tx.aggregate)
	}

	tx = &outboxTx{err: errors.New("boom")}
	if err := commitGatedPRReady(context.Background(), &txBeginnerFake{tx: tx}, pc, "", 0, "", time.Now()); err == nil || tx.rollbacks != 1 {
		t.Errorf("failure: err=%v rollbacks=%d", err, tx.rollbacks)
	}
	bad := pc
	bad.WorkItemID = "not-a-uuid"
	if err := commitGatedPRReady(context.Background(), &txBeginnerFake{tx: &outboxTx{}}, bad, "", 0, "", time.Now()); err == nil {
		t.Error("unparseable work item id accepted")
	}
}

// ─── Source guards ───────────────────────────────────────────────────────────

func dashboardSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), " "), " ")
	}
	return out
}

// stateChangeTopics are the pipeline topics an approval announces. They must
// reach Kafka only through the outbox. The dashboard's remaining direct
// publishes are chat streaming (chat.message, chat.proposal.resolved), the
// codex extraction request, and user_prompt.answered/revoked, none of which
// accompanies a work-item state change.
var stateChangeTopics = []string{
	"TopicIntake", "TopicPRDReady", "TopicResearchTasks", "TopicImplementationTasks",
	"TopicPRReady", "TopicPRGated", "TopicQAReady", "TopicPRMerged", "TopicWorkBlocked",
	"TopicTechnicalBriefReady", "TopicUserPromptRequested",
}

func TestDashboard_NoDirectPublishOfStateChangeTopics(t *testing.T) {
	directPublish := regexp.MustCompile(`\.Publish\([^;\n]*bus\.(\w+)`)
	for name, code := range dashboardSources(t) {
		for _, m := range directPublish.FindAllStringSubmatch(code, -1) {
			for _, topic := range stateChangeTopics {
				if m[1] == topic {
					t.Errorf("%s publishes bus.%s directly; it must be enqueued with its state change", name, topic)
				}
			}
		}
		for _, needle := range []string{"publishQAReady(", "publishGatedPRReady(", "PublishRequested("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s still calls %s", name, needle)
			}
		}
	}
}

// Both pr-gate approval paths enqueue through commitGatedPRReady.
func TestDashboard_GatedApprovalsUseOutbox(t *testing.T) {
	src := dashboardSources(t)
	all := src["handlers.go"] + src["handlers_pr_gate.go"]
	if n := strings.Count(all, "commitGatedPRReady("); n != 2 {
		t.Errorf("commitGatedPRReady called %d time(s) in the approval paths, want 2 (ApprovePendingChange, HandlePRGatePromptAnswer)", n)
	}
}

func TestDashboard_StartsOneNamedDispatcherAndWaits(t *testing.T) {
	code := dashboardSources(t)["main.go"]
	if strings.Count(code, `outbox.StartForService(`) != 1 || !strings.Contains(code, `"dashboard-backend")`) {
		t.Error("main.go must start exactly one outbox dispatcher with outbox.StartForService(..., \"dashboard-backend\")")
	}
	if !strings.Contains(code, "outboxDispatcher.Wait()") {
		t.Error("main.go does not wait for the outbox dispatcher on shutdown")
	}
}
