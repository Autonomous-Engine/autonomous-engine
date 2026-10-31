package main

import (
	"context"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/workgraph"
	"github.com/google/uuid"
)

// The brief's codex pin commits with the brief: work_item_codex_pins gets
// the product, branch, product_version and modules, and
// work_items.codex_version shows the product_version. A brief that fails to
// commit leaves no pin.
func TestIntegration_TechnicalBriefWritesTheCodexPin(t *testing.T) {
	pool := integrationPool(t)
	wg := workgraph.New(pool)
	ctx := context.Background()
	workID, productID := newWorkItem(t, pool, models.WorkItemStateResearch)
	pin := &codex.Pin{WorkItemID: workID, ProductID: productID, Branch: "main", ProductVersion: 4, Modules: []string{"billing", "api"}}

	artifact := &models.WorkItemArtifact{WorkItemID: workID, Type: models.ArtifactTypeTechnicalBrief, Content: []byte(`{"brief":"b"}`), AgentID: agentID}
	msg := technicalBriefReadyMessage{WorkID: workID.String(), ProductID: productID.String(), BriefContent: "b", Timestamp: time.Now().UTC().Format(time.RFC3339)}

	// A brief whose artifact cannot be written (its work item does not
	// exist) rolls back, pin included.
	bad := &models.WorkItemArtifact{WorkItemID: uuid.New(), Type: models.ArtifactTypeTechnicalBrief, Content: []byte(`{}`), AgentID: agentID}
	if err := commitTechnicalBrief(ctx, pool, wg, workID, bad, msg, uuid.Nil, uuid.Nil, pin); err == nil {
		t.Fatal("brief for a missing work item committed")
	}
	if _, found, err := codex.NewPinStore(pool).Get(ctx, workID); err != nil || found {
		t.Fatalf("pin after a failed brief: found=%v err=%v", found, err)
	}

	if err := commitTechnicalBrief(ctx, pool, wg, workID, artifact, msg, uuid.Nil, uuid.Nil, pin); err != nil {
		t.Fatalf("commitTechnicalBrief: %v", err)
	}
	got, found, err := codex.NewPinStore(pool).Get(ctx, workID)
	if err != nil || !found || got.ProductVersion != 4 || got.Branch != "main" || got.ProductID != productID || len(got.Modules) != 2 || got.Modules[0] != "api" {
		t.Fatalf("pin = %+v, found=%v, err=%v", got, found, err)
	}
	var display int
	if err := pool.QueryRow(ctx, `SELECT codex_version FROM work_items WHERE id = $1`, workID).Scan(&display); err != nil || display != 4 {
		t.Fatalf("work_items.codex_version = %d, %v; want 4", display, err)
	}

	// No pin (no product, no branch): the brief still commits.
	other, _ := newWorkItem(t, pool, models.WorkItemStateResearch)
	artifact2 := &models.WorkItemArtifact{WorkItemID: other, Type: models.ArtifactTypeTechnicalBrief, Content: []byte(`{"brief":"b"}`), AgentID: agentID}
	if err := commitTechnicalBrief(ctx, pool, wg, other, artifact2, technicalBriefReadyMessage{WorkID: other.String()}, uuid.Nil, uuid.Nil, nil); err != nil {
		t.Fatalf("brief without a pin: %v", err)
	}
}
