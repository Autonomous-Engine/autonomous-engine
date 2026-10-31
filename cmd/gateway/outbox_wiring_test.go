package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/bus"
	"github.com/autonomousengine/autonomous-engine/internal/outbox"
	"github.com/google/uuid"
)

// These tests pin the M1-04 conversion of the gateway: intake creates the
// work item and enqueues the intake event in one transaction, and nothing in
// the service publishes to Kafka directly any more.

func gatewaySources(t *testing.T) map[string]string {
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
		out[name] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("found no gateway sources to scan")
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

// funcSource returns the source text of the named top-level function.
func funcSource(t *testing.T, file, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != name {
			continue
		}
		return stripComments(string(src[fset.Position(fd.Pos()).Offset:fset.Position(fd.End()).Offset]))
	}
	t.Fatalf("function %s not found in %s", name, file)
	return ""
}

func TestGateway_NeverPublishesDirectly(t *testing.T) {
	forbidden := []string{".Publish(", ".PublishMessage(", "publishJSON(", "PublishRequested("}
	for name, src := range gatewaySources(t) {
		code := stripComments(src)
		for _, needle := range forbidden {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — intake must reach Kafka only through the outbox", name, needle)
			}
		}
	}
}

func TestGateway_IntakeCommitsItemAndEventInOneTransaction(t *testing.T) {
	body := funcSource(t, "main.go", "commitIntake")
	if n := strings.Count(body, "workgraph.WithTx("); n != 1 {
		t.Fatalf("commitIntake has %d WithTx calls, want 1", n)
	}
	for _, want := range []string{
		"wg.CreateWorkItemTx(ctx, tx, item)",
		"wg.SetAgentDefinitionTx(ctx, tx, item.ID, *agentDefID)",
		"outbox.EnqueueJSON(ctx, tx, item.ID, bus.TopicIntake, item.ID.String(), msg)",
		// M4-06: the shadow columns commit with the item.
		"shadow.ApplyTx(ctx, tx, item.ID)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("commitIntake does not contain %q", want)
		}
	}
	// The pooled writes must be gone from the intake path.
	handler := funcSource(t, "main.go", "intakeHandler")
	for _, gone := range []string{"wg.CreateWorkItem(", "pool.Exec(", "UPDATE work_items"} {
		if strings.Contains(handler, gone) {
			t.Errorf("intakeHandler still calls %s outside the intake transaction", gone)
		}
	}
	if !strings.Contains(handler, "commitIntake(") {
		t.Error("intakeHandler does not commit through commitIntake")
	}
}

func TestGateway_StartsAndWaitsForItsDispatcher(t *testing.T) {
	code := stripComments(gatewaySources(t)["main.go"])
	if n := strings.Count(code, "outbox.StartForService("); n != 1 {
		t.Errorf("found %d outbox.StartForService calls, want 1", n)
	}
	if !strings.Contains(code, `"gateway")`) {
		t.Error("dispatcher is not named after the gateway service")
	}
	if strings.Contains(code, "outbox.Start(") || strings.Contains(code, "outbox.StartWithOptions(") {
		t.Error("use outbox.StartForService, not Start/StartWithOptions")
	}
	if n := strings.Count(code, "dispatcher.Wait()"); n != 1 {
		t.Errorf("found %d dispatcher.Wait() calls, want 1 on the shutdown path", n)
	}
	// The dispatcher must outlive the HTTP server so an in-flight intake's
	// event is still dispatched.
	if strings.Index(code, "srv.Shutdown(") > strings.Index(code, "dispatcher.Wait()") {
		t.Error("dispatcher.Wait() runs before srv.Shutdown; in-flight intakes would be left undelivered until restart")
	}
}

// TestIntakeEvent_PayloadUnchangedPlusEventID: the conversion changes how the
// message is delivered, not what it says.
func TestIntakeEvent_PayloadUnchangedPlusEventID(t *testing.T) {
	workID := uuid.New()
	msg := intakeMessage{
		WorkID:      workID.String(),
		ProductID:   uuid.New().String(),
		Description: "Add a phone number field",
		Source:      "api",
		Metadata:    map[string]interface{}{"k": "v"},
	}
	ev, err := outbox.NewEvent(outbox.AggregateTypeWorkItem, workID, bus.TopicIntake, workID.String(), msg)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if ev.Topic != "intake" || ev.Key != workID.String() || ev.AggregateID != workID {
		t.Errorf("event addressed %s/%s/%s, want intake/%s/%s", ev.Topic, ev.Key, ev.AggregateID, workID, workID)
	}
	var got map[string]any
	if err := json.Unmarshal(ev.Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got[outbox.FieldEventID] != ev.ID.String() {
		t.Errorf("event_id = %v, want %s", got[outbox.FieldEventID], ev.ID)
	}
	delete(got, outbox.FieldEventID)
	raw, _ := json.Marshal(msg)
	var want map[string]any
	_ = json.Unmarshal(raw, &want)
	if len(got) != len(want) {
		t.Errorf("payload has %d fields besides event_id, want %d", len(got), len(want))
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("payload lost field %q", k)
		}
	}
}
