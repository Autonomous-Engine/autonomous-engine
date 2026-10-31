package strategy

import (
	"strings"
	"testing"
)

// TestVerifyGate_Integration_TwoProposedOneVerified simulates the
// end-to-end flow: the model emits a natural-language response that
// proposes two files and runs verify_imports on one of them. The gate
// must identify the one that was NOT verified and produce a corrective
// prompt naming it.
func TestVerifyGate_Integration_TwoProposedOneVerified(t *testing.T) {
	response := "" +
		"I'll add caching.\n\n" +
		"**`app/api/locations/route.ts`**: wrap the GET with a TTL cache.\n" +
		"```ts\nimport { NextResponse } from 'next/server';\n```\n\n" +
		"**`lib/cache.ts`**: new module.\n" +
		"```ts\nexport class TTLCache<T> {}\n```\n"

	turnCalls := []ToolCall{
		{Name: "codex_find_module", Args: `{"name":"locations"}`},
		{Name: "codex_verify_imports", Args: `{"path":"app/api/locations/route.ts","content":"..."}`},
		// lib/cache.ts NEVER verified
	}

	proposed := ParseProposedFiles(response)
	if len(proposed) != 2 {
		t.Fatalf("expected 2 proposed files, got %d: %v", len(proposed), proposed)
	}

	missing := MissingVerifications(proposed, turnCalls)
	if len(missing) != 1 || missing[0] != "lib/cache.ts" {
		t.Fatalf("expected missing=[lib/cache.ts], got %v", missing)
	}

	corrective := ForcedVerifyPrompt(missing)
	if !strings.Contains(corrective, "lib/cache.ts") {
		t.Errorf("corrective prompt should name lib/cache.ts, got %q", corrective)
	}
	if !strings.Contains(corrective, "codex_verify_imports") {
		t.Errorf("corrective prompt should name the tool")
	}
}

// TestVerifyGate_Integration_NoProposedFiles simulates a response that
// never claims code for any file (a purely analytic / design answer).
// The gate must be a no-op.
func TestVerifyGate_Integration_NoProposedFiles(t *testing.T) {
	response := "" +
		"The cache should be a Map keyed by URL with a 60s TTL. " +
		"Invalidation on write is unnecessary because GET is the only path.\n"

	proposed := ParseProposedFiles(response)
	if len(proposed) != 0 {
		t.Fatalf("prose answer yielded %d proposed files, want 0: %v", len(proposed), proposed)
	}

	missing := MissingVerifications(proposed, nil)
	if len(missing) != 0 {
		t.Fatalf("expected no missing verifications, got %v", missing)
	}

	if out := ForcedVerifyPrompt(missing); out != "" {
		t.Errorf("empty missing should produce empty prompt, got %q", out)
	}
}

// TestVerifyGate_Integration_AllProposedVerified simulates the
// happy-path case: model proposes three files and verifies each. The
// gate must return no missing paths so the tool loop terminates cleanly.
func TestVerifyGate_Integration_AllProposedVerified(t *testing.T) {
	response := "" +
		"**`a/route.ts`**:\n```ts\nexport const x = 1;\n```\n\n" +
		"**`b/helper.ts`**:\n```ts\nexport const y = 2;\n```\n\n" +
		"**`c/types.ts`**:\n```ts\nexport type Z = number;\n```\n"

	turnCalls := []ToolCall{
		{Name: "codex_verify_imports", Args: `{"path":"a/route.ts"}`},
		{Name: "codex_verify_imports", Args: `{"path":"b/helper.ts"}`},
		{Name: "codex_verify_imports", Args: `{"path":"c/types.ts"}`},
	}

	proposed := ParseProposedFiles(response)
	if len(proposed) != 3 {
		t.Fatalf("expected 3 proposed files, got %d: %v", len(proposed), proposed)
	}

	missing := MissingVerifications(proposed, turnCalls)
	if len(missing) != 0 {
		t.Fatalf("all verified: expected no missing, got %v", missing)
	}
}
