package main

import "testing"

// The PRD's H1 replaced the work item's title, so every item was renamed
// "Product Requirements Document: …" (F33). The generic prefix is dropped,
// and a heading that is only the generic phrase keeps the existing title.
func TestExtractTitleDropsTheGenericPRDPrefix(t *testing.T) {
	got := extractTitle("# Product Requirements Document: Custom Charset Mappings in Parser\n\n## Overview\n", "Add parser option")
	if got != "Custom Charset Mappings in Parser" {
		t.Fatalf("title = %q", got)
	}
	if got := extractTitle("# Product Requirements Document\n\n## Overview\n", "Add parser option"); got != "" {
		t.Fatalf("generic heading should yield no new title, got %q", got)
	}
	if got := extractTitle("# PRD: Add parser option for custom charset mappings\n", ""); got != "Add parser option for custom charset mappings" {
		t.Fatalf("PRD: prefix: %q", got)
	}
}
