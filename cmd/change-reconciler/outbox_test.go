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

type enqueued struct {
	aggregateType string
	aggregateID   uuid.UUID
	topic, key    string
	payload       map[string]any
}

func (b *fakeBeginner) events(t *testing.T) []enqueued {
	t.Helper()
	var out []enqueued
	for i, sql := range b.sqls {
		if !strings.Contains(sql, "INSERT INTO outbox_events") {
			continue
		}
		a := b.args[i]
		e := enqueued{aggregateType: a[1].(string), aggregateID: a[2].(uuid.UUID), topic: a[3].(string), key: a[4].(string)}
		if err := json.Unmarshal(a[5].([]byte), &e.payload); err != nil {
			t.Fatal(err)
		}
		if e.payload[outbox.FieldEventID] != a[0].(uuid.UUID).String() {
			t.Errorf("payload event_id = %v, want the row id", e.payload[outbox.FieldEventID])
		}
		delete(e.payload, outbox.FieldEventID)
		out = append(out, e)
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

// TestChangeReconciler_NeverPublishesDirectly: the reconciler's event
// (codex.updated) goes through the outbox; its telemetry allowlist is empty.
func TestChangeReconciler_NeverPublishesDirectly(t *testing.T) {
	for name, code := range sources(t) {
		for _, needle := range []string{".Publish(", ".PublishMessage(", "publishJSON("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s; the reconciler's events must be enqueued in the outbox", name, needle)
			}
		}
	}
}

func TestChangeReconciler_StartsOneDispatcherAndWaitsForIt(t *testing.T) {
	starts, waits := 0, 0
	for _, code := range sources(t) {
		starts += strings.Count(code, "outbox.StartForService(ctx, pool, kb, log.Default(), agentID)")
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || waits != 1 {
		t.Errorf("dispatcher starts=%d waits=%d, want 1 and 1", starts, waits)
	}
}

func TestEnqueueCodexUpdated_ProductAggregateAndUnchangedMessage(t *testing.T) {
	b := &fakeBeginner{}
	product := uuid.New()
	msg := codexUpdatedMessage{Source: "github", CommitSHA: "abc", Timestamp: "now"}
	if err := enqueueCodexUpdated(context.Background(), b, &product, "github:main", msg); err != nil {
		t.Fatalf("enqueueCodexUpdated: %v", err)
	}
	if b.commits != 1 {
		t.Errorf("commits = %d", b.commits)
	}
	evs := b.events(t)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	e := evs[0]
	if e.topic != bus.TopicCodexUpdated || e.key != "github:main" {
		t.Errorf("topic/key = %s/%s", e.topic, e.key)
	}
	if e.aggregateType != aggregateTypeProduct || e.aggregateID != product {
		t.Errorf("aggregate = %s/%s, want product/%s", e.aggregateType, e.aggregateID, product)
	}
	// The message as before, plus the product it resolved to (F3).
	if len(e.payload) != 4 || e.payload["commit_sha"] != "abc" || e.payload["source"] != "github" || e.payload["product_id"] != product.String() {
		t.Errorf("payload = %v", e.payload)
	}

	// No product: the global codex aggregate.
	b = &fakeBeginner{}
	if err := enqueueCodexUpdated(context.Background(), b, nil, "k", msg); err != nil {
		t.Fatal(err)
	}
	e = b.events(t)[0]
	if e.aggregateType != aggregateTypeCodex || e.aggregateID != globalCodexAggregateID {
		t.Errorf("productless aggregate = %s/%s", e.aggregateType, e.aggregateID)
	}
	if _, ok := e.payload["product_id"]; ok || len(e.payload) != 3 {
		t.Errorf("productless payload = %v, want the original three fields", e.payload)
	}
}

// Codex coherence Part 1: the reconciler no longer blocks work. Its global
// 24 h blockAffectedWork (every item in research…review of every product)
// is gone; an external change reaches in-flight work only through indexing
// (external-change.detected / codex.updated → knowledge-indexer →
// codex.indexed → status-orchestrator's codex re-plan of the items whose
// pinned modules changed).
func TestChangeReconciler_NeverBlocksWork(t *testing.T) {
	for name, code := range sources(t) {
		for _, needle := range []string{"TopicWorkBlocked", "blockAffectedWork", "enqueueWorkBlocked", "escalationTimeout", "GetByState("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s still contains %s", name, needle)
			}
		}
	}
}
