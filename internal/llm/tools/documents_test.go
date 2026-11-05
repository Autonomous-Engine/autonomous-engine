package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

const testPRDDoc = `# Product Requirements Document: Custom Charset Mappings

## Overview
The parser decodes headers with a fixed charset table.

## Problem Statement
GB2312 headers need GB18030.
Users cannot override the table.

## Acceptance Criteria

### 1. API Implementation
- A CharsetMappings option exists.

### 2. Decoding Logic
- Lookups are case-insensitive.

## Open Questions
- None.
`

// A registered document is retrieved by section, by search or as a table
// of contents, so a task carries only what it needs and fetches the rest.
func TestReadDocumentSectionTOCAndSearch(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	ex.SetDocuments(map[string]string{"prd": testPRDDoc})
	ctx := context.Background()
	call := func(args string) adapter.ToolResult {
		return ex.Execute(ctx, adapter.ToolCall{ID: "d", Name: "read_document", Arguments: args})
	}

	// A section by (case-insensitive, substring) heading, subsections
	// included, ending at the next heading of the same or higher level.
	res := call(`{"name":"prd","section":"acceptance criteria"}`)
	if res.IsError || !strings.Contains(res.Content, "### 1. API Implementation") || !strings.Contains(res.Content, "Lookups are case-insensitive") || strings.Contains(res.Content, "Open Questions") {
		t.Fatalf("section = %+v", res)
	}
	res = call(`{"name":"prd","section":"Decoding Logic"}`)
	if res.IsError || !strings.Contains(res.Content, "case-insensitive") || strings.Contains(res.Content, "Open Questions") || strings.Contains(res.Content, "API Implementation") {
		t.Fatalf("subsection = %+v", res)
	}
	// The table of contents when nothing is asked for.
	res = call(`{"name":"prd"}`)
	if res.IsError || !strings.Contains(res.Content, "## Overview") || !strings.Contains(res.Content, "### 2. Decoding Logic") || strings.Contains(res.Content, "GB18030") {
		t.Fatalf("toc = %+v", res)
	}
	// Search returns matching lines with their section.
	res = call(`{"name":"prd","query":"gb18030"}`)
	if res.IsError || !strings.Contains(res.Content, "GB2312 headers need GB18030") || !strings.Contains(res.Content, "Problem Statement") {
		t.Fatalf("search = %+v", res)
	}
	res = call(`{"name":"prd","query":"zzz-nothing"}`)
	if res.IsError || !strings.Contains(res.Content, "no lines match") {
		t.Fatalf("empty search = %+v", res)
	}
	// Unknown document or section.
	if res = call(`{"name":"brief"}`); !res.IsError || !strings.Contains(res.Content, "prd") {
		t.Fatalf("unknown document = %+v, want an error naming the available documents", res)
	}
	if res = call(`{"name":"prd","section":"Budget"}`); !res.IsError || !strings.Contains(res.Content, "Overview") {
		t.Fatalf("unknown section = %+v, want an error listing the headings", res)
	}
	// Results are bounded.
	ex.SetDocuments(map[string]string{"big": "# Big\n\n## Body\n" + strings.Repeat("line of text\n", 1000)})
	res = call(`{"name":"big","section":"Body"}`)
	if res.IsError || len(res.Content) > documentResultMax+100 || !strings.Contains(res.Content, "truncated") {
		t.Fatalf("big section = %d chars, err=%v, want ≤ %d and a truncation note", len(res.Content), res.IsError, documentResultMax)
	}
}

func TestResearchToolsOfferReadDocument(t *testing.T) {
	has := func(ts []adapter.Tool) bool {
		for _, tl := range ts {
			if tl.Name == "read_document" {
				return true
			}
		}
		return false
	}
	if has(ResearchToolsFor("code_trace", ResearchToolOptions{})) {
		t.Fatal("read_document offered without Documents")
	}
	if !has(ResearchToolsFor("code_trace", ResearchToolOptions{Documents: true})) {
		t.Fatal("read_document not offered with Documents")
	}
}
