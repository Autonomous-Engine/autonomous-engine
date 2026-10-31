package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Migration 060: the subtask's codex_version is the parent's pinned
// product_version; without a pin it is the parent's recorded codex_version,
// and 0 stays 0 (unknown) instead of the old hard-coded 1.
func TestPlannedCodexVersion(t *testing.T) {
	pin := &codex.Pin{ProductVersion: 9}
	if got := plannedCodexVersion(pin, &models.WorkItem{CodexVersion: 4}); got != 9 {
		t.Errorf("pinned: %d, want 9", got)
	}
	if got := plannedCodexVersion(nil, &models.WorkItem{CodexVersion: 4}); got != 4 {
		t.Errorf("unpinned with a recorded version: %d, want 4", got)
	}
	if got := plannedCodexVersion(nil, &models.WorkItem{}); got != 0 {
		t.Errorf("unpinned, nothing recorded: %d, want 0", got)
	}
	if got := plannedCodexVersion(nil, nil); got != 0 {
		t.Errorf("no parent: %d, want 0", got)
	}
}

// The message carries the pin: product_version in codex_version (as
// before), the branch and the pinned modules; all additive.
func TestImplementationTaskMessage_CarriesTheCodexPin(t *testing.T) {
	var msg implementationTaskMessage
	applyCodexPin(&msg, &codex.Pin{Branch: "develop", ProductVersion: 5, Modules: []string{"api", "billing"}})
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["codex_version"] != float64(5) || m["codex_branch"] != "develop" || strings.Join(toStrings(m["codex_modules"]), ",") != "api,billing" {
		t.Fatalf("message = %s", b)
	}
	var unpinned implementationTaskMessage
	applyCodexPin(&unpinned, nil)
	b, _ = json.Marshal(unpinned)
	if strings.Contains(string(b), "codex_branch") || strings.Contains(string(b), "codex_modules") {
		t.Fatalf("an unpinned message carries pin fields: %s", b)
	}
}

func toStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// The plan copies the codex pin inside its transaction, after the subtask
// row exists; a read error of the parent's pin fails the delivery.
func TestPlanner_CopiesCodexPinInPlanTransaction(t *testing.T) {
	src := strings.ReplaceAll(stripComments(plannerSources(t)["main.go"]), "\r\n", "\n")
	commit := src[strings.Index(src, "func commitDecomposition("):]
	commit = commit[:strings.Index(commit, "\n}\n")]
	create := strings.Index(commit, "wg.CreateWorkItemTx(ctx, tx, ps.item)")
	cp := strings.Index(commit, "copyCodexPinTx(ctx, tx, parentWorkID, ps)")
	if create < 0 || cp < 0 || cp < create || cp > strings.Index(commit, "tx.Commit(ctx)") {
		t.Fatalf("copyCodexPinTx at %d, CreateWorkItemTx at %d: the copy must follow the subtask row inside the transaction", cp, create)
	}
	// The parent is re-pinned before any message is encoded.
	repin, encode := strings.Index(commit, "repinParentTx(ctx, tx, parentWorkID, planned)"), strings.Index(commit, "encodeStoredMessage(ps.msg)")
	if repin < 0 || encode < 0 || repin > encode {
		t.Fatalf("repinParentTx at %d, encodeStoredMessage at %d: the re-pin must precede the messages", repin, encode)
	}
	for _, want := range []string{"codexPin, codexPinErr := resolvePlanningCodexPin(msgCtx, cx, pool, workID)", "return fmt.Errorf(\"read codex pin for %s: %w\", workID, codexPinErr)"} {
		if !strings.Contains(src, want) {
			t.Errorf("planner does not contain %q", want)
		}
	}
	// The pin is resolved before the codex context the plan is built from.
	if strings.Index(src, "resolvePlanningCodexPin(msgCtx") > strings.Index(src, "cx.BuildContextForIntentBranch(msgCtx") {
		t.Error("the planning pin is read after the codex context")
	}
}

type fakeCurrent struct {
	v   int
	err error
}

func (f fakeCurrent) GetCurrentVersion(context.Context, uuid.UUID, string) (int, error) {
	return f.v, f.err
}

func TestResolvePlanningCodexPin_CurrentVersionErrorIsReturned(t *testing.T) {
	if _, err := resolvePlanningCodexPin(context.Background(), fakeCurrent{err: errors.New("db down")}, failingRowQuerier{}, uuid.New()); err == nil {
		t.Fatal("a read error was treated as no pin")
	}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errors.New("db down") }

type failingRowQuerier struct{ failingQuerier }

func (failingRowQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

func TestLoadParentCodexPin_ReadErrorIsReturned(t *testing.T) {
	if _, err := loadParentCodexPin(context.Background(), failingRowQuerier{}, uuid.New()); err == nil {
		t.Fatal("a read error was treated as no pin")
	}
}
