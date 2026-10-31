package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/interactions"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeTx struct {
	pgx.Tx
	db *fakeBeginner
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.db.sqls = append(t.db.sqls, sql)
	t.db.args = append(t.db.args, args)
	if t.db.failAt == len(t.db.sqls) {
		return pgconn.CommandTag{}, errors.New("exec failed")
	}
	return pgconn.CommandTag{}, nil
}
func (t *fakeTx) Commit(context.Context) error   { t.db.commits++; return nil }
func (t *fakeTx) Rollback(context.Context) error { t.db.rollbacks++; return nil }

type fakeBeginner struct {
	failAt             int
	sqls               []string
	args               [][]any
	commits, rollbacks int
}

func (b *fakeBeginner) Begin(context.Context) (pgx.Tx, error) { return &fakeTx{db: b}, nil }

type outboxArgs struct {
	aggregateType string
	aggregateID   uuid.UUID
	topic, key    string
	payload       map[string]any
}

func (b *fakeBeginner) outboxRows(t *testing.T) []outboxArgs {
	t.Helper()
	var out []outboxArgs
	for i, sql := range b.sqls {
		if !strings.Contains(sql, "INSERT INTO outbox_events") {
			continue
		}
		a := b.args[i]
		r := outboxArgs{aggregateType: a[1].(string), aggregateID: a[2].(uuid.UUID), topic: a[3].(string), key: a[4].(string)}
		if err := json.Unmarshal(a[5].([]byte), &r.payload); err != nil {
			t.Fatal(err)
		}
		if r.payload[outbox.FieldEventID] != a[0].(uuid.UUID).String() {
			t.Errorf("payload event_id = %v, want the row id", r.payload[outbox.FieldEventID])
		}
		delete(r.payload, outbox.FieldEventID)
		out = append(out, r)
	}
	return out
}

func sources(t *testing.T) map[string]string {
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
		out[n] = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(string(b), " ")
	}
	return out
}

var directPublish = regexp.MustCompile(`\.Publish\([^,]+,\s*([A-Za-z0-9_."]+)`)

// indexerTelemetry is the knowledge-indexer's explicit allowlist of direct
// publishes: indexing progress and the onboarding prefill progress/drift
// notifications (telemetry per the M1-04e brief). codex.updated,
// codex.update.from_interaction and codex.extract.requested are enqueued.
var indexerTelemetry = map[string]bool{
	"bus.TopicIndexingProgress":               true,
	"bus.TopicOnboardingPrefillDriftDetected": true,
	"bus.TopicOnboardingPrefillCompleted":     true,
}

func TestKnowledgeIndexer_OnlyTelemetryIsPublishedDirectly(t *testing.T) {
	seen := map[string]int{}
	for name, code := range sources(t) {
		for _, m := range directPublish.FindAllStringSubmatch(code, -1) {
			seen[m[1]]++
			if !indexerTelemetry[m[1]] {
				t.Errorf("%s publishes %s directly; only telemetry may bypass the outbox", name, m[1])
			}
		}
		for _, needle := range []string{".PublishMessage(", "publishJSON(", "publishCodexUpdated("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s", name, needle)
			}
		}
	}
	for topic := range indexerTelemetry {
		if seen[topic] != 1 {
			t.Errorf("telemetry publish %s found %d times, want 1 (the scan must still see it)", topic, seen[topic])
		}
	}
}

func TestKnowledgeIndexer_StartsOneDispatcherAndWaitsForIt(t *testing.T) {
	starts, waits := 0, 0
	for _, code := range sources(t) {
		starts += strings.Count(code, "outbox.StartForService(ctx, pool, kb, log.Default(), serviceName)")
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || waits != 1 {
		t.Errorf("dispatcher starts=%d waits=%d, want 1 and 1", starts, waits)
	}
}

func TestEnqueueExtractRequest_RepositoryAggregate(t *testing.T) {
	b := &fakeBeginner{}
	repo := uuid.New()
	req := extractRequest{RepoID: repo, CommitSHA: "abc", Branch: "main", ParentSHA: "def"}
	if err := enqueueExtractRequest(context.Background(), b, req); err != nil {
		t.Fatalf("enqueueExtractRequest: %v", err)
	}
	r := b.outboxRows(t)[0]
	if r.topic != bus.TopicCodexExtractRequested || r.key != repo.String() {
		t.Errorf("topic/key = %s/%s", r.topic, r.key)
	}
	if r.aggregateType != aggregateTypeRepository || r.aggregateID != repo {
		t.Errorf("aggregate = %s/%s", r.aggregateType, r.aggregateID)
	}
	if r.payload["commit_sha"] != "abc" || r.payload["parent_sha"] != "def" || len(r.payload) != 4 {
		t.Errorf("payload = %v", r.payload)
	}
}

func TestOutboxCodexPromoter_ClassificationAndEventShareOneTx(t *testing.T) {
	b := &fakeBeginner{}
	p := &outboxCodexPromoter{db: b, store: interactions.NewStore(nil)}
	prompt, work, product, entry := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cls := interactions.ClassificationResult{ClassifiedAs: interactions.ClassifiedCodexPattern, CodexEntryID: &entry, CodexEntryTable: "codex_patterns"}
	payload := CodexUpdateFromInteraction{Source: "user_interaction", InteractionID: prompt, ProductID: product, CodexEntryID: entry, Section: "codex_patterns"}
	if err := p.Promote(context.Background(), prompt, work, cls, payload); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if b.commits != 1 || len(b.sqls) != 2 {
		t.Fatalf("commits=%d statements=%v", b.commits, b.sqls)
	}
	if !strings.Contains(b.sqls[0], "UPDATE user_interactions") {
		t.Errorf("first statement = %s, want the classification write-back", b.sqls[0])
	}
	r := b.outboxRows(t)[0]
	if r.topic != bus.TopicCodexUpdateFromInteraction || r.key != product.String() {
		t.Errorf("topic/key = %s/%s, want the product key as before", r.topic, r.key)
	}
	if r.aggregateType != outbox.AggregateTypeWorkItem || r.aggregateID != work {
		t.Errorf("aggregate = %s/%s, want work_item/%s", r.aggregateType, r.aggregateID, work)
	}
	if r.payload["codex_entry_id"] != entry.String() || r.payload["codex_section"] != "codex_patterns" {
		t.Errorf("payload = %v", r.payload)
	}

	// The enqueue failing rolls the back-link back: the row never points at
	// a codex entry whose creating event was lost.
	b = &fakeBeginner{failAt: 2}
	p = &outboxCodexPromoter{db: b, store: interactions.NewStore(nil)}
	if err := p.Promote(context.Background(), prompt, work, cls, payload); err == nil {
		t.Fatal("expected an error")
	}
	if b.commits != 0 || b.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d", b.commits, b.rollbacks)
	}

	// No work item: ordered on the product instead.
	b = &fakeBeginner{}
	p = &outboxCodexPromoter{db: b, store: interactions.NewStore(nil)}
	if err := p.Promote(context.Background(), prompt, uuid.Nil, cls, payload); err != nil {
		t.Fatal(err)
	}
	if r := b.outboxRows(t)[0]; r.aggregateType != aggregateTypeProduct || r.aggregateID != product {
		t.Errorf("work-item-less aggregate = %s/%s", r.aggregateType, r.aggregateID)
	}
}
