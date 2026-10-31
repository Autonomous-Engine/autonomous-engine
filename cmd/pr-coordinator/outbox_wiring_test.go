package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// coordinatorSources returns the non-test Go sources of this package.
func coordinatorSources(t *testing.T) map[string]string {
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

// TestCoordinator_NeverPublishesDirectly guards the M1-04b conversion. Every
// event pr-coordinator emits (pr.gated, pr.ready, the pr-gate prompt)
// announces a database write, so all of them go through the outbox; it has no
// telemetry topic to exempt. Re-adding a direct publish is one plausible line
// and reopens the "row committed, event lost" window.
func TestCoordinator_NeverPublishesDirectly(t *testing.T) {
	forbidden := []string{".Publish(", ".PublishMessage(", "publishJSON(", "PublishRequested("}
	for name, src := range coordinatorSources(t) {
		code := stripComments(src)
		for _, needle := range forbidden {
			if strings.Contains(code, needle) {
				t.Errorf("%s calls %s — pr-coordinator events must reach Kafka only through the outbox", name, needle)
			}
		}
	}
}

// The pending-row insert runs inside the gate's transaction, where a failed
// statement aborts the transaction; the pre-046 undefined-column retry cannot
// work there and was dropped (migration 046 is a deploy prerequisite).
func TestCoordinator_NoUndefinedColumnFallback(t *testing.T) {
	for name, src := range coordinatorSources(t) {
		code := stripComments(src)
		if name == "contracts.go" {
			// M2-06's one exemption: the contract-edge READ (on the pool,
			// outside every transaction) treats a pre-056 database as "no
			// edges". It is not a write fallback; cut that one function.
			start := strings.Index(code, "func isMissingContractSchema(")
			if start < 0 {
				t.Fatal("contracts.go: isMissingContractSchema not found")
			}
			end := start + strings.Index(code[start:], "\n}\n")
			code = code[:start] + code[end:]
		}
		if strings.Contains(code, "42703") || strings.Contains(code, "insertPendingPRChangeLegacySQL") {
			t.Errorf("%s still carries the pre-046 head_sha fallback", name)
		}
	}
}

// The handler must route both PR paths through the transactional helpers.
func TestCoordinator_HandlerUsesCommitHelpers(t *testing.T) {
	code := stripComments(coordinatorSources(t)["main.go"])
	for _, needle := range []string{"recordGatedPR(", "recordPRReady("} {
		if !strings.Contains(code, needle) {
			t.Errorf("main.go does not call %s", needle)
		}
	}
}

// The gate loop must stop once a failed gate commit has blocked the parent.
func TestCoordinator_GateLoopStopsAfterBlock(t *testing.T) {
	code := stripComments(coordinatorSources(t)["main.go"])
	if !regexp.MustCompile(`subtasks:\s*for _, c := range children`).MatchString(code) {
		t.Error("the per-repo PR loop is not labelled subtasks")
	}
	if !regexp.MustCompile(`if stop, rerr := recordGatedPR\([^\n]*\); rerr != nil \{[^}]*break subtasks\s*\} else if stop \{[^}]*break subtasks`).MatchString(code) {
		t.Error("the gate branch does not break out of the repo loop when recordGatedPR reports a retry or a block")
	}
	if strings.Contains(code, "commitGatedPR(") {
		t.Error("main.go calls commitGatedPR directly; use recordGatedPR so a failure stops the loop")
	}
}

// Auto mode: once a failed pr.ready commit has blocked the parent, no
// further GitHub PRs are opened for the remaining repositories.
func TestCoordinator_AutoLoopStopsAfterBlock(t *testing.T) {
	code := stripComments(coordinatorSources(t)["main.go"])
	if !regexp.MustCompile(`stop, rerr := recordPRReady\([^\n]*\)\n\s*if rerr != nil \{[^}]*break subtasks\s*\}\n[^\n]*\n\s*if stop \{[^}]*break subtasks`).MatchString(code) {
		t.Error("the auto-mode branch does not break out of the repo loop when recordPRReady reports a retry or a block")
	}
	if strings.Contains(code, "commitPRReady(") {
		t.Error("main.go calls commitPRReady directly; use recordPRReady so a failure stops the loop")
	}
}

func TestCoordinator_StartsOneNamedDispatcherAndWaits(t *testing.T) {
	starts, waits := 0, 0
	for _, src := range coordinatorSources(t) {
		code := stripComments(src)
		starts += strings.Count(code, `outbox.StartForService(`)
		if strings.Count(code, "outbox.Start(")+strings.Count(code, "outbox.StartWithOptions(") > 0 {
			t.Error("use outbox.StartForService so claimed_by names the service")
		}
		if strings.Contains(code, `"pr-coordinator")`) && strings.Contains(code, "dispatcher.Wait()") {
			waits++
		}
	}
	if starts != 1 {
		t.Errorf("found %d outbox.StartForService call(s), want exactly 1", starts)
	}
	if waits != 1 {
		t.Error("main does not start the dispatcher as \"pr-coordinator\" and Wait() for it on shutdown")
	}
}
