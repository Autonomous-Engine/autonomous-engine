package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// chatSource returns every non-test .go file in the package, comments
// stripped and CRLF normalised (the primary checkout uses core.autocrlf=true),
// joined so a publish added in a new file is still seen.
func chatSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	files := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatal(err)
		}
		src := strings.ReplaceAll(string(b), "\r\n", "\n")
		all.WriteString(regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, " "))
		all.WriteString("\n")
		files++
	}
	if files == 0 {
		t.Fatal("found no sources to scan")
	}
	return all.String()
}

// chatTelemetry is the chat agent's explicit allowlist of direct publishes:
// chat.response is the streamed reply to the user (telemetry per common3).
// The state-changing path — create_work_item, reached when the user asks for
// a work item in chat — lives in internal/llm/tools and enqueues intake with
// the row (createWorkItemWithIntake).
var chatTelemetry = map[string]bool{"bus.TopicChatResponse": true}

func TestChatAgent_OnlyTelemetryIsPublishedDirectly(t *testing.T) {
	code := chatSource(t)
	matches := regexp.MustCompile(`\.Publish\([^,]+,\s*([A-Za-z0-9_."]+)`).FindAllStringSubmatch(code, -1)
	if len(matches) != 3 {
		t.Errorf("found %d direct publishes, want the 3 chat.response publishes", len(matches))
	}
	for _, m := range matches {
		if !chatTelemetry[m[1]] {
			t.Errorf("chat-agent publishes %s directly; only %v may bypass the outbox", m[1], chatTelemetry)
		}
	}
	if strings.Contains(code, ".PublishMessage(") {
		t.Error("chat-agent calls PublishMessage")
	}
}

func TestChatAgent_StartsOneDispatcherAndWaitsForIt(t *testing.T) {
	code := chatSource(t)
	if n := strings.Count(code, "outbox.StartForService(ctx, pool, kb, log.Default(), agentID)"); n != 1 {
		t.Errorf("dispatcher starts = %d, want 1", n)
	}
	if n := strings.Count(code, "dispatcher.Wait()"); n != 1 {
		t.Errorf("dispatcher waits = %d, want 1", n)
	}
}
