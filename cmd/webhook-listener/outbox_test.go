package main

import (
	"bytes"
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

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── Fakes ───────────────────────────────────────────────────────────────────

// fakeTx records every statement. Any pgx.Tx method it does not implement
// panics through the nil embedded interface, which is what a test wants.
type fakeTx struct {
	pgx.Tx
	db *fakeDB
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.db.txSQL = append(t.db.txSQL, sql)
	t.db.txArgs = append(t.db.txArgs, args)
	if t.db.failTxExecAt == len(t.db.txSQL) {
		return pgconn.CommandTag{}, errors.New("tx exec failed")
	}
	return pgconn.CommandTag{}, nil
}

func (t *fakeTx) Commit(context.Context) error   { t.db.commits++; return nil }
func (t *fakeTx) Rollback(context.Context) error { t.db.rollbacks++; return nil }

// errRow is a pgx.Row that always reports no rows.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// boolRow scans one bool.
type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*bool); ok {
		*p = r.v
		return nil
	}
	return errors.New("boolRow: unexpected destination")
}

// idRow scans one uuid.
type idRow struct{ id uuid.UUID }

func (r idRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*uuid.UUID); ok {
		*p = r.id
		return nil
	}
	return errors.New("idRow: unexpected destination")
}

// fakeDB is the webhookDB the handlers use: transactions for the outbox, plus
// the pooled reads (repository lookups) and the dead-letter write.
type fakeDB struct {
	repoID       uuid.UUID // returned by every repository lookup; Nil = no rows
	watched      bool      // codex_watched_branches answer
	repoLookups  int
	beginErr     error
	failTxExecAt int

	txSQL     []string
	txArgs    [][]any
	commits   int
	rollbacks int

	poolSQL     []string
	poolExecErr error
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return &fakeTx{db: d}, nil
}

func (d *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "codex_watched_branches") {
		if d.watched {
			return boolRow{v: true}
		}
		return errRow{err: pgx.ErrNoRows}
	}
	d.repoLookups++
	if d.repoID == uuid.Nil {
		return errRow{err: pgx.ErrNoRows}
	}
	return idRow{id: d.repoID}
}

func (d *fakeDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	d.poolSQL = append(d.poolSQL, sql)
	return pgconn.CommandTag{}, d.poolExecErr
}

// outboxRows decodes the outbox inserts recorded on the fake transaction.
type outboxRow struct {
	id            uuid.UUID
	aggregateType string
	aggregateID   uuid.UUID
	topic         string
	key           string
	payload       map[string]any
	headers       map[string]string
}

func (d *fakeDB) outboxRows(t *testing.T) []outboxRow {
	t.Helper()
	var rows []outboxRow
	for i, sql := range d.txSQL {
		if !strings.Contains(sql, "INSERT INTO outbox_events") {
			continue
		}
		a := d.txArgs[i]
		r := outboxRow{
			id:            a[0].(uuid.UUID),
			aggregateType: a[1].(string),
			aggregateID:   a[2].(uuid.UUID),
			topic:         a[3].(string),
			key:           a[4].(string),
		}
		if err := json.Unmarshal(a[5].([]byte), &r.payload); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		if err := json.Unmarshal(a[6].([]byte), &r.headers); err != nil {
			t.Fatalf("decode outbox headers: %v", err)
		}
		rows = append(rows, r)
	}
	return rows
}

// ─── Source scans ────────────────────────────────────────────────────────────

func serviceSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
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
		// The primary checkout uses core.autocrlf=true: normalise so a scan
		// behaves the same on CRLF sources.
		out[name] = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	if len(out) == 0 {
		t.Fatal("found no sources to scan")
	}
	return out
}

var (
	lineComment  = regexp.MustCompile(`(?m)//.*$`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

func stripComments(src string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(src, " "), " ")
}

// TestWebhookListener_NeverPublishesDirectly: webhooks are not replayable, so
// every event the listener accepts must reach Kafka through the outbox. The
// telemetry allowlist for this service is empty — it emits nothing that may
// be lost.
func TestWebhookListener_NeverPublishesDirectly(t *testing.T) {
	for name, src := range serviceSources(t) {
		code := stripComments(src)
		for _, needle := range []string{".Publish(", ".PublishMessage(", "publishJSON("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — webhook events must be enqueued in the outbox", name, needle)
			}
		}
	}
}

func TestWebhookListener_StartsOneDispatcherAndWaitsForIt(t *testing.T) {
	starts, waits := 0, 0
	for _, src := range serviceSources(t) {
		code := stripComments(src)
		starts += strings.Count(code, "outbox.StartForService(")
		starts += strings.Count(code, "outbox.Start(")
		starts += strings.Count(code, "outbox.StartWithOptions(")
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 {
		t.Errorf("found %d dispatcher start(s), want exactly 1", starts)
	}
	if waits != 1 {
		t.Errorf("found %d dispatcher.Wait() call(s), want exactly 1 on the shutdown path", waits)
	}
	found := false
	for _, src := range serviceSources(t) {
		if strings.Contains(stripComments(src), `outbox.StartForService(ctx, pool, kb, log.Default(), agentID)`) {
			found = true
		}
	}
	if !found {
		t.Error("main does not start the dispatcher as outbox.StartForService(ctx, pool, kb, log.Default(), agentID)")
	}
}

// TestWebhookListener_DrainsBeforeCancelling pins the shutdown order: the
// handlers enqueue on the service context, so srv.Shutdown (the graceful
// drain) must run before cancel(), and dispatcher.Wait() after it.
func TestWebhookListener_DrainsBeforeCancelling(t *testing.T) {
	code := stripComments(serviceSources(t)["main.go"])
	tail := code[strings.Index(code, `"webhook-listener: shutting down..."`):]
	shutdown := strings.Index(tail, "srv.Shutdown(")
	cancelAt := strings.Index(tail, "cancel()")
	wait := strings.Index(tail, "dispatcher.Wait()")
	if shutdown < 0 || cancelAt < 0 || wait < 0 || !(shutdown < cancelAt && cancelAt < wait) {
		t.Errorf("shutdown order: srv.Shutdown@%d cancel()@%d dispatcher.Wait()@%d; want Shutdown, then cancel, then Wait", shutdown, cancelAt, wait)
	}
}

// ─── Aggregates ──────────────────────────────────────────────────────────────

func TestSyntheticRepositoryID_IsStablePerRepository(t *testing.T) {
	a := syntheticRepositoryID("Acme/Widgets")
	if a == uuid.Nil {
		t.Fatal("synthetic id is nil")
	}
	if b := syntheticRepositoryID("acme/widgets"); a != b {
		t.Errorf("synthetic id depends on case: %s vs %s", a, b)
	}
	if c := syntheticRepositoryID("acme/gadgets"); a == c {
		t.Error("two repositories share a synthetic id")
	}
	if syntheticJiraIssueID("ABC-1") == syntheticJiraIssueID("ABC-2") {
		t.Error("two jira issues share an aggregate")
	}
	if syntheticJiraIssueID("ABC-1") != syntheticJiraIssueID("ABC-1") {
		t.Error("jira aggregate is not stable")
	}
}

func TestRepositoryAggregateID_PrefersTheRepositoryRow(t *testing.T) {
	repo := uuid.New()
	if got, registered := repositoryAggregateID(context.Background(), &fakeDB{repoID: repo}, "acme/widgets"); got != repo || !registered {
		t.Errorf("aggregate = %s registered=%v, want the repository id %s", got, registered, repo)
	}
	if got, registered := repositoryAggregateID(context.Background(), &fakeDB{}, "acme/widgets"); got != syntheticRepositoryID("acme/widgets") || registered {
		t.Errorf("unregistered repository aggregate = %s registered=%v, want the synthetic id", got, registered)
	}
}

func TestRepositoryAggregateSQL_MatchesAllURLFormsCaseInsensitively(t *testing.T) {
	for _, want := range []string{
		"lower(github_url) = lower($1)",
		"lower(github_url) = lower('https://github.com/' || $1)",
		"lower(github_url) = lower('https://github.com/' || $1 || '.git')",
		"ORDER BY created_at, id",
	} {
		if !strings.Contains(repositoryAggregateSQL, want) {
			t.Errorf("repositoryAggregateSQL lacks %q", want)
		}
	}
}

// ─── Delivery-derived event ids ──────────────────────────────────────────────

func TestDeliveryEventIDs_SameDeliveryYieldsSameIDs(t *testing.T) {
	a, ok := githubEventID("d-1", bus.TopicPRMerged, 0)
	b, _ := githubEventID("d-1", bus.TopicPRMerged, 0)
	if !ok || a != b {
		t.Fatalf("same delivery gave %s and %s", a, b)
	}
	for _, other := range []uuid.UUID{
		mustID(githubEventID("d-2", bus.TopicPRMerged, 0)),
		mustID(githubEventID("d-1", bus.TopicExternalChangeDetected, 0)),
		mustID(githubEventID("d-1", bus.TopicPRMerged, 1)),
	} {
		if other == a {
			t.Error("a different delivery, topic or index produced the same id")
		}
	}
	if _, ok := githubEventID("", bus.TopicPRMerged, 0); ok {
		t.Error("an empty delivery id must fall back to a random event id")
	}
	j1, ok := jiraEventID(1700000000000, "ABC-1", 0)
	j2, _ := jiraEventID(1700000000000, "ABC-1", 0)
	if !ok || j1 != j2 || j1 == mustID(jiraEventID(1700000000000, "ABC-1", 1)) || j1 == mustID(jiraEventID(1700000000001, "ABC-1", 0)) {
		t.Error("jira ids are not derived from timestamp + key + index")
	}
	if _, ok := jiraEventID(0, "ABC-1", 0); ok {
		t.Error("a missing jira timestamp must fall back to a random event id")
	}
}

func mustID(id uuid.UUID, _ bool) uuid.UUID { return id }

func postGitHubDelivery(t *testing.T, db *fakeDB, event, delivery string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	signGitHub(req, raw, testWebhookSecret)
	rec := httptest.NewRecorder()
	githubWebhookHandler(context.Background(), db, envSecrets(testWebhookSecret))(rec, req)
	return rec
}

// A redelivered webhook carries the same X-GitHub-Delivery, so it produces
// byte-identical event ids; the outbox insert is ON CONFLICT (id) DO NOTHING,
// so the second enqueue is a no-op instead of a duplicate event.
func TestGitHub_RedeliveryEnqueuesTheSameEventIDs(t *testing.T) {
	repo := uuid.New()
	first := &fakeDB{repoID: repo, watched: true}
	second := &fakeDB{repoID: repo, watched: true}
	postGitHubDelivery(t, first, "push", "delivery-42", pushBody("refs/heads/main"))
	postGitHubDelivery(t, second, "push", "delivery-42", pushBody("refs/heads/main"))
	a, b := first.outboxRows(t), second.outboxRows(t)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("rows = %d and %d, want 2 each", len(a), len(b))
	}
	for i := range a {
		want := mustID(githubEventID("delivery-42", a[i].topic, i))
		if a[i].id != want || b[i].id != want {
			t.Errorf("event %d ids = %s / %s, want the delivery-derived %s", i, a[i].id, b[i].id, want)
		}
		if a[i].payload[outbox.FieldEventID] != want.String() || a[i].headers[outbox.HeaderEventID] != want.String() {
			t.Errorf("event %d payload/header event_id do not carry the derived id", i)
		}
	}
	for i, sql := range second.txSQL {
		if strings.Contains(sql, "INSERT INTO outbox_events") && !strings.Contains(sql, "ON CONFLICT (id) DO NOTHING") {
			t.Errorf("statement %d is not the conflict-ignoring insert, so a redelivery would abort its tx", i)
		}
	}

	// A different delivery of the same content is a different event.
	third := &fakeDB{repoID: repo, watched: true}
	postGitHubDelivery(t, third, "push", "delivery-43", pushBody("refs/heads/main"))
	if third.outboxRows(t)[0].id == a[0].id {
		t.Error("a new delivery reused the previous delivery's event id")
	}
}

func TestGitHubPush_ResolvesTheRepositoryOnceForBothEvents(t *testing.T) {
	repo := uuid.New()
	db := &fakeDB{repoID: repo, watched: true}
	postGitHubDelivery(t, db, "push", "d-once", pushBody("refs/heads/main"))
	if db.repoLookups != 1 {
		t.Errorf("repository lookups = %d, want 1", db.repoLookups)
	}
	rows := db.outboxRows(t)
	if len(rows) != 2 || rows[0].aggregateID != repo || rows[1].aggregateID != repo || rows[1].key != repo.String() {
		t.Errorf("rows = %+v, want both events on repository %s", rows, repo)
	}

	// An unregistered repository cannot be watched: no extract request.
	db = &fakeDB{watched: true}
	postGitHubDelivery(t, db, "push", "d-unreg", pushBody("refs/heads/main"))
	if rows := db.outboxRows(t); len(rows) != 1 || rows[0].topic != bus.TopicExternalChangeDetected {
		t.Errorf("unregistered push rows = %+v, want only external-change", rows)
	}
}

// ─── Event construction ──────────────────────────────────────────────────────

func payloadWithoutEventID(t *testing.T, r outboxRow) map[string]any {
	t.Helper()
	if r.payload[outbox.FieldEventID] != r.id.String() {
		t.Errorf("payload event_id = %v, want the row id %s", r.payload[outbox.FieldEventID], r.id)
	}
	if r.headers[outbox.HeaderEventID] != r.id.String() {
		t.Errorf("header event_id = %v, want the row id %s", r.headers[outbox.HeaderEventID], r.id)
	}
	out := map[string]any{}
	for k, v := range r.payload {
		if k != outbox.FieldEventID {
			out[k] = v
		}
	}
	return out
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func postGitHub(t *testing.T, db *fakeDB, event string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", event)
	signGitHub(req, raw, testWebhookSecret)
	rec := httptest.NewRecorder()
	githubWebhookHandler(context.Background(), db, envSecrets(testWebhookSecret))(rec, req)
	return rec
}

func pushBody(ref string) map[string]any {
	return map[string]any{
		"ref":         ref,
		"after":       "abc123",
		"repository":  map[string]any{"full_name": "acme/widgets"},
		"pusher":      map[string]any{"name": "dev"},
		"head_commit": map[string]any{"id": "abc123", "message": "fix: a thing"},
	}
}

func TestGitHubPush_EnqueuesExternalChangeInOneShortTx(t *testing.T) {
	repo := uuid.New()
	db := &fakeDB{repoID: repo}
	rec := postGitHub(t, db, "push", pushBody("refs/heads/main"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if db.commits != 1 || db.rollbacks != 0 {
		t.Errorf("commits=%d rollbacks=%d, want one committed transaction", db.commits, db.rollbacks)
	}
	rows := db.outboxRows(t)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1 (the branch is not watched, so no extract request)", len(rows))
	}
	r := rows[0]
	if r.topic != bus.TopicExternalChangeDetected || r.key != "abc123" {
		t.Errorf("topic/key = %s/%s, want %s/abc123", r.topic, r.key, bus.TopicExternalChangeDetected)
	}
	if r.aggregateType != aggregateTypeRepository || r.aggregateID != repo {
		t.Errorf("aggregate = %s/%s, want repository/%s", r.aggregateType, r.aggregateID, repo)
	}
	got := payloadWithoutEventID(t, r)
	want := map[string]any{"source": "github", "branch": "main", "commit_sha": "abc123", "is_agent": false, "repository_id": repo.String()}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("payload %s = %v, want %v", k, got[k], v)
		}
	}
	// The zero event omits repository_id; a registered push adds it (F3).
	if len(got) != len(asMap(t, externalChangeEvent{}))+1 {
		t.Errorf("payload fields = %v; the wire format changed", got)
	}
}

func TestGitHubPush_WatchedBranchEnqueuesBothEventsTogether(t *testing.T) {
	repo := uuid.New()
	evs, err := pushEvents(githubPushPayload{
		Ref: "refs/heads/main", After: "abc123",
		HeadCommit: &githubCommit{Message: "[agent] change"},
	}, repo, true, true, "now", "")
	if err != nil {
		t.Fatalf("pushEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %d, want external-change + extract request", len(evs))
	}
	if evs[0].Topic != bus.TopicExternalChangeDetected || evs[1].Topic != bus.TopicCodexExtractRequested {
		t.Errorf("topics = %s, %s", evs[0].Topic, evs[1].Topic)
	}
	if evs[1].Key != repo.String() || evs[1].AggregateID != repo || evs[1].AggregateType != aggregateTypeRepository {
		t.Errorf("extract event key/aggregate = %s %s/%s", evs[1].Key, evs[1].AggregateType, evs[1].AggregateID)
	}
	var extract map[string]any
	if err := json.Unmarshal(evs[1].Payload, &extract); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"repo_id", "commit_sha", "branch", "parent_sha", "pushed_by", "pushed_at", "event_id"} {
		if _, ok := extract[k]; !ok {
			t.Errorf("extract payload lost %q: %v", k, extract)
		}
	}
	var change map[string]any
	_ = json.Unmarshal(evs[0].Payload, &change)
	if change["is_agent"] != true {
		t.Errorf("is_agent = %v, want true for an [agent] commit", change["is_agent"])
	}
	if change["repository_id"] != repo.String() {
		t.Errorf("repository_id = %v, want the registered repository %s", change["repository_id"], repo)
	}

	// A feature-branch push that is not watched produces nothing.
	none, err := pushEvents(githubPushPayload{Ref: "refs/heads/feature/x", After: "def"}, repo, true, false, "now", "")
	if err != nil || len(none) != 0 {
		t.Errorf("unwatched feature push = %d events, err %v; want none", len(none), err)
	}
}

func TestGitHubPush_UnregisteredRepositoryUsesTheSyntheticAggregate(t *testing.T) {
	db := &fakeDB{}
	if rec := postGitHub(t, db, "push", pushBody("refs/heads/master")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rows := db.outboxRows(t)
	if len(rows) != 1 || rows[0].aggregateID != syntheticRepositoryID("acme/widgets") {
		t.Fatalf("rows = %+v, want one event on the synthetic repository aggregate", rows)
	}
	// The synthetic id is no repository: the event names none (F3), so the
	// knowledge-indexer treats it as a notification.
	got := payloadWithoutEventID(t, rows[0])
	if _, ok := got["repository_id"]; ok {
		t.Errorf("unregistered push names repository_id %v", got["repository_id"])
	}
	if len(got) != len(asMap(t, externalChangeEvent{})) {
		t.Errorf("payload fields = %v; the wire format changed", got)
	}
}

func prBody(branch string, merged bool) map[string]any {
	return map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 42, "title": "Do it", "merged": merged,
			"head": map[string]any{"ref": branch},
			"base": map[string]any{"ref": "main"},
		},
		"repository": map[string]any{"full_name": "acme/widgets"},
	}
}

func TestGitHubPRMerged_EnqueuesPRMergedOnTheRepositoryAggregate(t *testing.T) {
	repo := uuid.New()
	work := uuid.New()
	db := &fakeDB{repoID: repo}
	if rec := postGitHub(t, db, "pull_request", prBody("agent/"+work.String()+"/abcd", true)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rows := db.outboxRows(t)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.topic != bus.TopicPRMerged || r.key != "pr-42" {
		t.Errorf("topic/key = %s/%s", r.topic, r.key)
	}
	if r.aggregateType != aggregateTypeRepository || r.aggregateID != repo {
		t.Errorf("aggregate = %s/%s, want repository/%s", r.aggregateType, r.aggregateID, repo)
	}
	got := payloadWithoutEventID(t, r)
	if got["work_id"] != work.String() || got["pr_number"] != float64(42) || got["is_agent"] != true {
		t.Errorf("payload = %v", got)
	}
	// The pre-M4-05 fields are unchanged; M4-05 adds only omitempty merge
	// identity fields (here: the repository; prBody carries no SHAs).
	if len(got) != len(asMap(t, prMergedEvent{}))+1 || got["repository"] != "acme/widgets" {
		t.Errorf("payload fields = %v; the wire format changed", got)
	}

	// Human branches and unmerged closes enqueue nothing and open no tx.
	for _, body := range []map[string]any{prBody("feature/x", true), prBody("agent/"+work.String(), false)} {
		db := &fakeDB{repoID: repo}
		postGitHub(t, db, "pull_request", body)
		if len(db.txSQL) != 0 || db.commits != 0 {
			t.Errorf("non-merge or human PR opened a transaction: %v", db.txSQL)
		}
	}
}

func TestGitHub_EnqueueFailureFallsBackToDeadLetter(t *testing.T) {
	db := &fakeDB{repoID: uuid.New(), failTxExecAt: 1}
	rec := postGitHub(t, db, "push", pushBody("refs/heads/main"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: the event is safe in the dead-letter table", rec.Code)
	}
	if db.commits != 0 || db.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d, want the failed tx rolled back", db.commits, db.rollbacks)
	}
	if len(db.poolSQL) != 1 || !strings.Contains(db.poolSQL[0], "webhook_dead_letters") {
		t.Errorf("pool statements = %v, want one dead-letter insert", db.poolSQL)
	}
}

func TestGitHub_EnqueueAndDeadLetterFailureIsReportedToTheSender(t *testing.T) {
	db := &fakeDB{repoID: uuid.New(), beginErr: errors.New("db down"), poolExecErr: errors.New("db down")}
	rec := postGitHub(t, db, "push", pushBody("refs/heads/main"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the delivery is recorded as failed and can be redelivered", rec.Code)
	}
}

func TestJiraWebhook_EnqueuesEveryChangelogItemInOneTx(t *testing.T) {
	body := map[string]any{
		"timestamp":    int64(1700000000123),
		"webhookEvent": "jira:issue_updated",
		"issue":        map[string]any{"key": "ABC-7"},
		"changelog": map[string]any{"items": []map[string]any{
			{"field": "priority", "fromString": "Low", "toString": "High"},
			{"field": "status", "fromString": "To Do", "toString": "Done"},
		}},
	}
	raw, _ := json.Marshal(body)
	db := &fakeDB{}
	rec := httptest.NewRecorder()
	jiraWebhookHandler(context.Background(), db)(rec, httptest.NewRequest(http.MethodPost, "/webhook/jira", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if db.commits != 1 {
		t.Errorf("commits = %d, want 1", db.commits)
	}
	rows := db.outboxRows(t)
	if len(rows) != 2 {
		t.Fatalf("outbox rows = %d, want 2", len(rows))
	}
	for i, field := range []string{"priority", "status"} {
		r := rows[i]
		if r.topic != bus.TopicExternalChangeDetected || r.key != "jira-ABC-7-"+field {
			t.Errorf("row %d topic/key = %s/%s", i, r.topic, r.key)
		}
		if r.aggregateType != aggregateTypeJiraIssue || r.aggregateID != syntheticJiraIssueID("ABC-7") {
			t.Errorf("row %d aggregate = %s/%s", i, r.aggregateType, r.aggregateID)
		}
		if want := mustID(jiraEventID(1700000000123, "ABC-7", i)); r.id != want {
			t.Errorf("row %d id = %s, want the webhook-derived %s", i, r.id, want)
		}
		if got := payloadWithoutEventID(t, r); got["field"] != field || got["issue_key"] != "ABC-7" {
			t.Errorf("row %d payload = %v", i, got)
		}
	}
}

// ─── Dead-letter retry ───────────────────────────────────────────────────────

func TestRetryDeadLetter_EnqueuesAndResolvesInOneTx(t *testing.T) {
	db := &fakeDB{}
	id := uuid.New()
	e := deadLetter{id: id.String(), source: "github", eventType: "pull_request", payload: []byte(`{"a":1}`)}
	if err := retryDeadLetter(context.Background(), db, e); err != nil {
		t.Fatalf("retryDeadLetter: %v", err)
	}
	if db.commits != 1 {
		t.Errorf("commits = %d, want 1", db.commits)
	}
	if len(db.txSQL) != 2 || !strings.Contains(db.txSQL[1], "SET resolved = true") {
		t.Fatalf("tx statements = %v, want outbox insert then resolve", db.txSQL)
	}
	rows := db.outboxRows(t)
	if rows[0].topic != bus.TopicPRMerged || rows[0].key != id.String() {
		t.Errorf("topic/key = %s/%s", rows[0].topic, rows[0].key)
	}
	if rows[0].aggregateType != aggregateTypeDeadLetter || rows[0].aggregateID != id {
		t.Errorf("aggregate = %s/%s", rows[0].aggregateType, rows[0].aggregateID)
	}
	if rows[0].payload["a"] != float64(1) {
		t.Errorf("payload = %v, want the stored body", rows[0].payload)
	}

	// A failed resolve rolls the enqueue back too: no event without its
	// dead letter being marked, so the retry cannot fire twice.
	db = &fakeDB{failTxExecAt: 2}
	if err := retryDeadLetter(context.Background(), db, e); err == nil {
		t.Fatal("retryDeadLetter succeeded although the resolve failed")
	}
	if db.commits != 0 || db.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", db.commits, db.rollbacks)
	}
}
