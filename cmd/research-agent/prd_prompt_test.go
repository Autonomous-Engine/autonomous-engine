package main

import (
	"strings"
	"testing"
)

const prdWithCriteria = `# Product Requirements Document: Custom Charset Mappings

## Overview
The parser decodes headers with a fixed charset table.

## Problem Statement
GB2312 headers need GB18030.

## Goals
- Let callers override the table.

## Acceptance Criteria

### 1. API Implementation
- A CharsetMappings option exists.

### 2. Decoding Logic
- Lookups are case-insensitive.

## Open Questions
- None.
`

// The task message carried the whole PRD on every call. It carries the
// title, the acceptance criteria (what research answers) and the other
// headings; the rest is one read_document away.
func TestResearchUserMessageCarriesAcceptanceCriteriaAndTOC(t *testing.T) {
	task := testTask("code_trace")
	task.StartingContext = prdWithCriteria
	msg := buildResearchUserMessage(task, "/repo", "")
	for _, want := range []string{"Custom Charset Mappings", "### 1. API Implementation", "Lookups are case-insensitive", "read_document", "Overview", "Problem Statement", "Goals", "Open Questions"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q", want)
		}
	}
	for _, absent := range []string{"fixed charset table", "GB2312 headers need GB18030", "Let callers override"} {
		if strings.Contains(msg, absent) {
			t.Errorf("message still carries the other sections' text: %q", absent)
		}
	}
	if !strings.Contains(buildResearchSystemPrompt("code_trace"), "read_document") {
		t.Error("the strategy must name read_document")
	}
}

func TestPRDForTaskFallsBackWithoutCriteria(t *testing.T) {
	body := "# Title\n\n## Context\n" + strings.Repeat("context line\n", 400) + "\n## Notes\nnote\n"
	kept, headings := prdForTask(body)
	if len(kept) > prdFallbackChars+200 || !strings.HasPrefix(kept, "# Title") {
		t.Fatalf("fallback kept %d chars starting %q, want the first %d", len(kept), kept[:20], prdFallbackChars)
	}
	if len(headings) != 2 || headings[0] != "Context" || headings[1] != "Notes" {
		t.Fatalf("headings = %v", headings)
	}
	kept, headings = prdForTask(prdWithCriteria)
	if !strings.Contains(kept, "### 2. Decoding Logic") || strings.Contains(kept, "GB18030") {
		t.Fatalf("with criteria kept = %q", kept)
	}
	if strings.Join(headings, ",") != "Overview,Problem Statement,Goals,Open Questions" {
		t.Fatalf("other headings = %v", headings)
	}
}
