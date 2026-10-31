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
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
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

// directPublish matches a direct bus publish and captures its topic argument.
var directPublish = regexp.MustCompile(`\.Publish\([^,]+,\s*([A-Za-z0-9_."]+)`)

// onboardingTelemetry is the explicit allowlist of topics onboarding may still
// publish directly. The prefill topics are progress telemetry for the
// dashboard (the M1-04e brief); everything else is enqueued in the outbox.
var onboardingTelemetry = map[string]bool{
	"bus.TopicOnboardingPrefillProgress": true,
}

func TestOnboarding_OnlyTelemetryIsPublishedDirectly(t *testing.T) {
	seen := 0
	for name, code := range sources(t) {
		for _, m := range directPublish.FindAllStringSubmatch(code, -1) {
			seen++
			if !onboardingTelemetry[m[1]] {
				t.Errorf("%s publishes %s directly; only telemetry (%v) may bypass the outbox", name, m[1], onboardingTelemetry)
			}
		}
		for _, needle := range []string{".PublishMessage(", "publishJSON("} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s", name, needle)
			}
		}
	}
	// The allowlisted telemetry publish must still be found, or the scan
	// itself has stopped matching anything.
	if seen != 1 {
		t.Errorf("found %d direct publish(es), want exactly the 1 telemetry publish", seen)
	}
}

func TestOnboarding_StartsOneDispatcherAndWaitsForIt(t *testing.T) {
	starts, waits := 0, 0
	for _, code := range sources(t) {
		starts += strings.Count(code, `outbox.StartForService(ctx, pool, kb, log.Default(), "onboarding")`)
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	if starts != 1 || waits != 1 {
		t.Errorf("dispatcher starts=%d waits=%d, want 1 and 1", starts, waits)
	}
}

func TestEnqueueCodexUpdated_ProductAggregateKeyedBySession(t *testing.T) {
	b := &fakeBeginner{}
	product := uuid.New()
	payload := map[string]interface{}{"session_id": "s-1", "trigger": "onboarding_initial_scan", "product_id": product.String()}
	if err := enqueueCodexUpdated(context.Background(), b, aggregateTypeProduct, product, "s-1", payload); err != nil {
		t.Fatalf("enqueueCodexUpdated: %v", err)
	}
	if b.commits != 1 || len(b.sqls) != 1 {
		t.Fatalf("commits=%d statements=%v", b.commits, b.sqls)
	}
	a := b.args[0]
	if a[1] != aggregateTypeProduct || a[2] != product || a[3] != bus.TopicCodexUpdated || a[4] != "s-1" {
		t.Errorf("event = %v %v %v %v", a[1], a[2], a[3], a[4])
	}
	var got map[string]any
	if err := json.Unmarshal(a[5].([]byte), &got); err != nil {
		t.Fatal(err)
	}
	if got["trigger"] != "onboarding_initial_scan" || got["session_id"] != "s-1" || len(got) != 4 {
		t.Errorf("payload = %v", got)
	}
}

// Migration 060: the approved codex version announces itself on
// codex.indexed (written with the version); approve-codex no longer
// enqueues a codex.updated notification, which nothing acted on (the
// indexer skipped it as a trigger-less notification).
func TestApproveCodex_NoCodexUpdatedNotification(t *testing.T) {
	for name, code := range sources(t) {
		if strings.Contains(code, `"version_number": versionNum`) || strings.Contains(code, `"version_number": rec.`) {
			t.Errorf("%s still sends the approve-codex codex.updated notification", name)
		}
	}
}

func TestApprovalSections(t *testing.T) {
	got := approvalSections([]string{"risk_zones", "autonomous_scope", "risk_zones"}, true, true)
	want := []string{"section:autonomous_scope", "section:risk_zones", "module:main", "patterns"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	if got := approvalSections(nil, true, false); strings.Join(got, ",") != "module:main" {
		t.Fatalf("no draft = %v", got)
	}
	if got := approvalSections([]string{"k"}, false, false); strings.Join(got, ",") != "section:k" {
		t.Fatalf("module not written = %v", got)
	}
}

func TestAnalysisTriggerAggregate(t *testing.T) {
	session := uuid.New()
	product := uuid.New()
	if typ, id := analysisTriggerAggregate(session.String(), product.String()); typ != aggregateTypeProduct || id != product {
		t.Errorf("with product: %s/%s", typ, id)
	}
	if typ, id := analysisTriggerAggregate(session.String(), ""); typ != aggregateTypeOnboardingSession || id != session {
		t.Errorf("without product: %s/%s", typ, id)
	}
}

func TestCreateValidationItem_WorkItemAndIntakeShareOneTx(t *testing.T) {
	b := &fakeBeginner{}
	item := &models.WorkItem{ID: uuid.New(), Type: models.WorkItemTypeFeature, State: models.WorkItemStateIntake}
	payload := map[string]interface{}{"work_id": item.ID.String(), "source": "onboarding_validation"}
	if err := createValidationItem(context.Background(), b, workgraph.New(nil), item, payload); err != nil {
		t.Fatalf("createValidationItem: %v", err)
	}
	if b.commits != 1 || len(b.sqls) != 2 || !strings.Contains(b.sqls[0], "INSERT INTO work_items") {
		t.Fatalf("commits=%d statements=%v", b.commits, b.sqls)
	}
	a := b.args[1]
	if a[1] != outbox.AggregateTypeWorkItem || a[2] != item.ID || a[3] != bus.TopicIntake || a[4] != item.ID.String() {
		t.Errorf("event = %v %v %v %v", a[1], a[2], a[3], a[4])
	}

	b = &fakeBeginner{failAt: 2}
	if err := createValidationItem(context.Background(), b, workgraph.New(nil), item, payload); err == nil {
		t.Fatal("expected an error")
	}
	if b.commits != 0 || b.rollbacks != 1 {
		t.Errorf("a failed enqueue must roll back the work item: commits=%d rollbacks=%d", b.commits, b.rollbacks)
	}
}
