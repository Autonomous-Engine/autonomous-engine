package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// jira-sync hosts the Jira ingester (internal/jira), which enqueues intake in
// the outbox; this service must therefore run exactly one dispatcher and wait
// for it on shutdown. It publishes nothing directly.
func TestJiraSync_StartsOneDispatcherAndNeverPublishesDirectly(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(string(b), " ")
	if n := strings.Count(code, `outbox.StartForService(ctx, pool, kb, log.Default(), "jira-sync")`); n != 1 {
		t.Errorf("dispatcher starts = %d, want 1", n)
	}
	if n := strings.Count(code, "dispatcher.Wait()"); n != 1 {
		t.Errorf("dispatcher waits = %d, want 1", n)
	}
	for _, needle := range []string{".Publish(", ".PublishMessage("} {
		if strings.Contains(code, needle) {
			t.Errorf("main.go calls %s", needle)
		}
	}
}
