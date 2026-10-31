package strategy

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseProposedFiles_CodeFencesWithHeadingPaths(t *testing.T) {
	resp := "" +
		"Here are the changes I'll make.\n\n" +
		"**`app/api/locations/route.ts`**: add in-memory cache\n" +
		"```ts\nimport { NextResponse } from 'next/server';\nexport async function GET() {}\n```\n\n" +
		"**`lib/cache.ts`**: new cache helper\n" +
		"```ts\nexport class TTLCache<T> {}\n```\n\n" +
		"**`lib/types.ts`**: shared types\n" +
		"```ts\nexport type Loc = { id: string };\n```\n"

	got := ParseProposedFiles(resp)
	want := []string{
		"app/api/locations/route.ts",
		"lib/cache.ts",
		"lib/types.ts",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseProposedFiles code-fence case\n got: %v\nwant: %v", got, want)
	}
}

func TestParseProposedFiles_UnifiedDiffHeaders(t *testing.T) {
	resp := "" +
		"Patch below:\n\n" +
		"```diff\n" +
		"--- a/src/foo.ts\n" +
		"+++ b/src/foo.ts\n" +
		"@@ -1 +1 @@\n" +
		"-export const x = 1;\n" +
		"+export const x = 2;\n" +
		"--- /dev/null\n" +
		"+++ b/src/bar.ts\n" +
		"@@ -0,0 +1 @@\n" +
		"+export const y = 3;\n" +
		"```\n"

	got := ParseProposedFiles(resp)
	want := []string{"src/bar.ts", "src/foo.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseProposedFiles diff case\n got: %v\nwant: %v", got, want)
	}
}

func TestParseProposedFiles_InlineBacktickPaths(t *testing.T) {
	resp := "" +
		"I'll update `app/api/locations/route.ts` to add caching. " +
		"A new file `lib/cache.ts` will hold the TTL logic. " +
		"Some inline code like `const x = 1` should be ignored, as should " +
		"`someFunction()`. Also mention `package.json` for the new dep.\n"

	got := ParseProposedFiles(resp)
	want := []string{
		"app/api/locations/route.ts",
		"lib/cache.ts",
		"package.json",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseProposedFiles inline-bullet case\n got: %v\nwant: %v", got, want)
	}
}

func TestParseProposedFiles_EmptyResponse(t *testing.T) {
	if got := ParseProposedFiles(""); got != nil {
		t.Fatalf("empty input: got %v, want nil", got)
	}
	if got := ParseProposedFiles("  \n  \t  "); got != nil {
		t.Fatalf("whitespace input: got %v, want nil", got)
	}
}

func TestParseProposedFiles_NoCodeIgnored(t *testing.T) {
	// Pure prose answer — no paths, no code. Gate must be a no-op.
	resp := "The cache should expire after 60 seconds. This is a design answer, not code."
	if got := ParseProposedFiles(resp); len(got) != 0 {
		t.Fatalf("prose answer should yield no paths, got %v", got)
	}
}

func TestMissingVerifications_AllVerified(t *testing.T) {
	proposed := []string{"app/api/locations/route.ts", "lib/cache.ts"}
	calls := []ToolCall{
		{Name: "codex_find_module", Args: `{"name":"cache"}`},
		{Name: "codex_verify_imports", Args: `{"path":"app/api/locations/route.ts","content":"..."}`},
		{Name: "codex_verify_imports", Args: `{"path":"lib/cache.ts","content":"..."}`},
	}
	got := MissingVerifications(proposed, calls)
	if len(got) != 0 {
		t.Fatalf("all-verified: got %v, want []", got)
	}
}

func TestMissingVerifications_OneMissing(t *testing.T) {
	proposed := []string{"app/api/locations/route.ts", "lib/cache.ts", "lib/types.ts"}
	calls := []ToolCall{
		{Name: "codex_verify_imports", Args: `{"path":"app/api/locations/route.ts"}`},
		{Name: "codex_verify_imports", Args: `{"path":"lib/cache.ts"}`},
		// types.ts never verified
		{Name: "codex_diagnostics", Args: `{"path":"lib/types.ts"}`},
	}
	got := MissingVerifications(proposed, calls)
	want := []string{"lib/types.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("one-missing: got %v, want %v", got, want)
	}
}

func TestMissingVerifications_NoneVerified(t *testing.T) {
	proposed := []string{"a/b.ts", "c/d.ts"}
	calls := []ToolCall{
		{Name: "codex_find_module", Args: `{"name":"b"}`},
	}
	got := MissingVerifications(proposed, calls)
	want := []string{"a/b.ts", "c/d.ts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("none-verified: got %v, want %v", got, want)
	}
}

func TestMissingVerifications_EmptyProposed(t *testing.T) {
	if got := MissingVerifications(nil, []ToolCall{{Name: "codex_verify_imports"}}); got != nil {
		t.Fatalf("empty proposed should return nil, got %v", got)
	}
}

func TestForcedVerifyPrompt_Shape(t *testing.T) {
	missing := []string{"a/b.ts", "lib/cache.ts"}
	out := ForcedVerifyPrompt(missing)
	if out == "" {
		t.Fatal("expected non-empty prompt for missing paths")
	}
	// Must mention the tool by exact name so the model binds correctly.
	if !strings.Contains(out, "codex_verify_imports") {
		t.Errorf("prompt missing tool name: %q", out)
	}
	// Must list every missing path.
	for _, p := range missing {
		if !strings.Contains(out, p) {
			t.Errorf("prompt missing path %q: %q", p, out)
		}
	}
	// Non-optional phrasing — no "please" or "could you".
	lower := strings.ToLower(out)
	if strings.Contains(lower, "please") || strings.Contains(lower, "could you") {
		t.Errorf("prompt used softening language, expected imperative: %q", out)
	}
	// Gate must call itself what it is — parent agents correlate logs.
	if !strings.Contains(out, "GATE") {
		t.Errorf("prompt missing GATE marker: %q", out)
	}
}

func TestForcedVerifyPrompt_EmptyMissing(t *testing.T) {
	if got := ForcedVerifyPrompt(nil); got != "" {
		t.Errorf("empty missing: expected empty string, got %q", got)
	}
}
