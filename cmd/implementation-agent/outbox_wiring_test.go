package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Source-scanning guards on the M1-04 conversion: every event this agent
// emits alongside a state write goes through the outbox, and only the
// telemetry topics (tool.executed, console.stream) are still published
// directly.

func agentSources(t *testing.T) map[string]string {
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
		out[name] = stripComments(string(b))
	}
	if len(out) == 0 {
		t.Fatal("found no sources to scan")
	}
	return out
}

var (
	lineComment  = regexp.MustCompile(`(?m)//.*$`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	publishCall  = regexp.MustCompile(`\.Publish\(\s*[^,]+,\s*([^,]+),`)
)

func stripComments(src string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(src, " "), " ")
}

// telemetryTopics are the only topics this agent may publish directly.
var telemetryTopics = map[string]bool{
	"bus.TopicToolExecuted":  true,
	"bus.TopicConsoleStream": true,
}

func TestImplementationAgent_PublishesOnlyTelemetryDirectly(t *testing.T) {
	seen := 0
	for name, code := range agentSources(t) {
		for _, m := range publishCall.FindAllStringSubmatch(code, -1) {
			seen++
			if topic := strings.TrimSpace(m[1]); !telemetryTopics[topic] {
				t.Errorf("%s publishes %s directly; only telemetry may bypass the outbox", name, topic)
			}
		}
		for _, needle := range []string{"publishJSON(", ".PublishMessage(", ".PublishRequested(", "guards.BlockWorkItem(", ".TransitionState(", "wg.CompleteAssignment(", ".CompleteAssignment(msgCtx"} {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — outcome writes and events must commit together through completionSink.Report", name, needle)
			}
		}
	}
	// The two telemetry publishes exist; a scan that matched nothing would
	// pass vacuously.
	if seen < 2 {
		t.Errorf("matched %d Publish calls, want at least the 2 telemetry publishes; the scan pattern is stale", seen)
	}
}

func TestImplementationAgent_ReportEnqueuesThroughTheOutbox(t *testing.T) {
	code := agentSources(t)["finalize.go"]
	for _, needle := range []string{
		"workgraph.WithTx(",
		"InsertTx(s.ctx, tx,",
		"PublishTx(s.ctx, tx, s.workID,",
		"guards.BlockWorkItemTx(s.ctx, tx, s.workID, s.workID,",
		"outbox.EnqueueJSON(s.ctx, tx, s.workID, bus.TopicImplementationComplete, s.taskID,",
		"CompleteAssignmentTx(s.ctx, tx,",
	} {
		if !strings.Contains(code, needle) {
			t.Errorf("finalize.go no longer contains %q; the outcome transaction lost a write", needle)
		}
	}
}

func TestImplementationAgent_StartsAndWaitsForOneDispatcher(t *testing.T) {
	starts, waits := 0, 0
	for _, code := range agentSources(t) {
		starts += strings.Count(code, "outbox.Start")
		waits += strings.Count(code, "dispatcher.Wait()")
	}
	main := agentSources(t)["main.go"]
	if starts != 1 || !strings.Contains(main, `outbox.StartForService(ctx, pool, kb, log.Default(), "implementation-agent")`) {
		t.Errorf("want exactly one outbox.StartForService(..., \"implementation-agent\") in main.go, found %d start call(s)", starts)
	}
	if waits != 1 {
		t.Errorf("found %d dispatcher.Wait() call(s), want 1 on the shutdown path", waits)
	}
}
