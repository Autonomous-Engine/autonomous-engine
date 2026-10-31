package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/zones"
	"github.com/google/uuid"
)

func planningZones(repo uuid.UUID) []zones.Zone {
	reason := "quarter close"
	return []zones.Zone{
		{RepositoryID: repo, Name: "safe", Patterns: []string{"docs/**"}, Policy: zones.PolicyAutonomous, Status: models.ZoneStatusActive},
		{RepositoryID: repo, Name: "payments", Patterns: []string{"payments/**"}, Policy: zones.PolicyHumanReview, Status: models.ZoneStatusActive},
		{RepositoryID: repo, Name: "ledger", Patterns: []string{"ledger/**"}, Policy: zones.PolicyEscalate, Status: models.ZoneStatusFrozen, FrozenReason: &reason},
		// Another repository's zone with the same glob must not apply.
		{RepositoryID: uuid.New(), Name: "other", Patterns: []string{"payments/**"}, Policy: zones.PolicyEscalate, Status: models.ZoneStatusFrozen},
	}
}

// Tasks carry the zones (names, policies) of the files they will change,
// only from their own repository's zones.
func TestAttachZonesPutsZonesOnTasks(t *testing.T) {
	repo := uuid.New()
	planned := []plannedSubtask{{msg: implementationTaskMessage{TaskID: "t1", RepositoryID: repo.String(), FilesToChange: []string{"payments/charge.go", "docs/a.md", "src/x.go"}}}}
	frozen := attachZones(planned, planningZones(repo), nil)
	if len(frozen) != 0 {
		t.Fatalf("frozen = %+v, want none", frozen)
	}
	got := planned[0].msg.Zones
	want := []zones.Ref{
		{Name: "payments", Policy: zones.PolicyHumanReview, Status: "active", Files: []string{"payments/charge.go"}},
		{Name: "safe", Policy: zones.PolicyAutonomous, Status: "active", Files: []string{"docs/a.md"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zones = %+v\nwant %+v", got, want)
	}
	raw, err := json.Marshal(planned[0].msg)
	if err != nil || !strings.Contains(string(raw), `"zones":[{"name":"payments","policy":"human_review"`) {
		t.Fatalf("message JSON = %s, %v", raw, err)
	}
	// A task in no zone carries none (and the key is omitted).
	none := []plannedSubtask{{msg: implementationTaskMessage{RepositoryID: repo.String(), FilesToChange: []string{"src/x.go"}}}}
	attachZones(none, planningZones(repo), nil)
	if raw, _ := json.Marshal(none[0].msg); strings.Contains(string(raw), `"zones"`) {
		t.Fatalf("zone-less message encodes zones: %s", raw)
	}
}

// Planned paths are matched relative to the repository: an absolute path
// under the clone is made relative (so a clone at /srv/infra/app does not
// put every file in infra/**), an absolute path elsewhere never matches.
func TestAttachZonesAnchorsToTheClone(t *testing.T) {
	repo := uuid.New()
	zs := []zones.Zone{{RepositoryID: repo, Name: "infra", Patterns: []string{"infra/**"}, Policy: zones.PolicyEscalate, Status: models.ZoneStatusFrozen}}
	roots := map[string]string{repo.String(): "/srv/infra/app"}
	planned := []plannedSubtask{{msg: implementationTaskMessage{RepositoryID: repo.String(), FilesToChange: []string{"/srv/infra/app/x.go", "/elsewhere/infra/y.tf"}}}}
	if frozen := attachZones(planned, zs, roots); len(frozen) != 0 || planned[0].msg.Zones != nil {
		t.Fatalf("frozen = %+v zones = %+v, want none", frozen, planned[0].msg.Zones)
	}
	planned = []plannedSubtask{{msg: implementationTaskMessage{RepositoryID: repo.String(), FilesToChange: []string{"/srv/infra/app/infra/main.tf"}}}}
	if frozen := attachZones(planned, zs, roots); len(frozen) != 1 || frozen[0].Files[0] != "infra/main.tf" {
		t.Fatalf("frozen = %+v, want infra with the relative file", frozen)
	}
}

// A task whose files fall in a frozen zone is not planned: the item
// escalates through the planner's block path (work.blocked → escalation
// prompt) with "zone X is frozen: reason", and nothing is committed.
func TestFrozenZoneEscalatesWithoutTasks(t *testing.T) {
	repo := uuid.New()
	planned := []plannedSubtask{
		{repoName: "api", msg: implementationTaskMessage{TaskID: "t1", RepositoryID: repo.String(), FilesToChange: []string{"ledger/post.go", "docs/a.md"}}},
	}
	frozen := attachZones(planned, planningZones(repo), nil)
	if len(frozen) != 1 || frozen[0].Zone.Name != "ledger" {
		t.Fatalf("frozen = %+v, want the ledger zone", frozen)
	}
	reason := frozenZoneReason(frozen)
	for _, want := range []string{"zone ledger is frozen: quarter close", "ledger/post.go", "No subtask was created", "Retry"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}

	tx := &decompTx{state: models.WorkItemStatePlanning}
	workID := uuid.New()
	if dup := escalateBlock(context.Background(), decompDB{tx}, workID, blockFrozenZone, reason, func(string) error { t.Fatal("direct block"); return nil }); dup {
		t.Fatal("duplicate")
	}
	var blocked string
	for i, sql := range tx.sqls {
		if strings.Contains(sql, "INSERT INTO outbox_events") {
			if tx.args[i][3] == bus.TopicImplementationTasks {
				t.Fatal("an implementation.tasks event was enqueued for a frozen zone")
			}
			if tx.args[i][3] == bus.TopicWorkBlocked {
				var body map[string]any
				if err := json.Unmarshal(tx.args[i][5].([]byte), &body); err != nil {
					t.Fatal(err)
				}
				blocked, _ = body["reason"].(string)
			}
		}
		if strings.Contains(sql, "INSERT INTO work_items") || strings.Contains(sql, "INSERT INTO plan_tasks") {
			t.Fatalf("a subtask or plan task was written: %s", sql)
		}
	}
	if !strings.Contains(blocked, "zone ledger is frozen: quarter close") {
		t.Fatalf("work.blocked reason = %q", blocked)
	}

	// In the handler the frozen-zone block comes before the plan commits.
	src := stripComments(plannerSources(t)["main.go"])
	iBlock := strings.Index(src, "escalateBlock(msgCtx, idempotency.Wrap(msgCtx, pool), workID, blockFrozenZone")
	iCommit := strings.Index(src, "commitDecomposition(msgCtx, idempotency.Wrap(msgCtx, pool)")
	if iBlock < 0 || iCommit < 0 || iBlock > iCommit {
		t.Fatalf("frozen-zone block at %d, commit at %d: the block must precede the commit", iBlock, iCommit)
	}
}

// The planner prompt lists the product's zones with their policies, so the
// model avoids restricted areas when it can.
func TestZonesPromptText(t *testing.T) {
	repo := uuid.New()
	text := zonesPromptText(planningZones(repo)[:3], map[string]string{repo.String(): "api"})
	for _, want := range []string{
		"- safe (api): autonomous — docs/**",
		"- payments (api): human_review — payments/**",
		"- ledger (api): escalate, FROZEN (quarter close) — ledger/**",
		"Prefer files outside escalate and frozen zones",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("prompt text lacks %q:\n%s", want, text)
		}
	}
	if zonesPromptText(nil, nil) != "" {
		t.Fatal("no zones must give no prompt section")
	}
}
