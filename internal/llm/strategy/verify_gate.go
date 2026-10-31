package strategy

// verify_gate.go — structural enforcement of verify-before-emit.
//
// The ImplementStrategy prompt instructs the model to call
// codex_verify_imports on every proposed file before returning a final
// answer. Small / gemma-class models ignore prompt pressure reliably
// enough that we cannot rely on instruction-following alone. This module
// provides the plumbing for an out-of-band gate: when the model returns a
// natural-language response that proposes code for files it never
// verified, the agent injects a corrective user message and re-enters the
// tool loop. The gate is deterministic, dependency-free, and shared
// between the implementation-agent and the benchmark-probe so production
// behavior and benchmark behavior stay aligned.
//
// The parsing here is intentionally conservative: we'd rather miss a
// borderline file reference than force a spurious verify call. False
// negatives cost us enforcement on that particular file; false positives
// would make the gate flap on responses that never proposed code at all.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ToolCall is a lightweight record of a single tool invocation during a
// tool-loop turn. We deliberately don't reuse adapter.ToolCall so this
// file stays free of adapter coupling — it's pure string logic.
//
// Name is the tool identifier (e.g. "codex_verify_imports").
// Args is the raw JSON-encoded arguments payload, matching the shape the
// executor receives (adapter.ToolCall.Arguments). Args may be empty.
type ToolCall struct {
	Name string
	Args string
}

// verifyImportsTool is the single tool name the gate treats as a
// "verification" of a proposed file. codex_diagnostics is weaker
// (LSP-only, won't catch hallucinated package imports) so we require
// codex_verify_imports specifically — that's the tool named in the
// ImplementStrategy MANDATORY clause.
const verifyImportsTool = "codex_verify_imports"

// Tool names that count as "the model actually wrote the file." Any of
// these with a path matching a proposed file satisfies the "wrote it"
// half of the gate; codex_verify_imports is a separate check.
var writeTools = map[string]struct{}{
	"write_file": {},
	"edit_file":  {},
	// R11: the insertion tools write the file too.
	"insert_after":   {},
	"append_to_file": {},
}

// Pre-compiled matchers for ParseProposedFiles. Keep these conservative:
// the cost of missing one file is a weaker gate on that file; the cost
// of over-matching is spurious re-prompts that waste LLM turns.
var (
	// unifiedDiffHeaderRe matches "--- a/path" and "+++ b/path" lines that
	// prefix files inside a unified diff. We anchor to start-of-line and
	// accept either an a/ or b/ prefix or no prefix at all ("--- path").
	unifiedDiffHeaderRe = regexp.MustCompile(`(?m)^(?:---|\+\+\+)\s+(?:[ab]/)?([^\s\r\n]+)`)

	// bulletOrHeadingPathRe captures paths that appear inside backticks in
	// heading / bullet / bold contexts. Common LLM shapes:
	//   - `**path/to/file.tsx**`
	//   - `- \`path/to/file.tsx\`: description`
	//   - `## path/to/file.tsx`
	//   - `File: \`path/to/file.tsx\``
	// The path must contain a slash OR a recognized extension — bare names
	// like "api" don't match.
	bulletOrHeadingPathRe = regexp.MustCompile("`([^`\\n]{2,200})`")

	// fencedCodeBlockRe captures the header line of a fenced code block
	// along with the line immediately before it. That preceding line is
	// where the model conventionally names the file. We don't capture the
	// fence body because the body might contain unrelated paths.
	//
	// Multiline mode: `(?ms)` so `.` spans lines.
	// This regex is used with FindAllStringSubmatchIndex to locate fences;
	// we then walk backwards to find the nearest preceding line that looks
	// like a path claim. Fence language tag is in group 1; we use it only
	// as an additional filter (e.g. "ts", "tsx", "go").
	fencedCodeBlockStartRe = regexp.MustCompile("(?m)^```([a-zA-Z0-9_+\\-]*)\\s*$")

	// pathLikeRe is a post-filter for candidate strings extracted from
	// bullets / headings / context lines. It accepts strings that either
	// contain a path separator ("/") AND end in a plausible source-file
	// extension, OR match a well-known bare filename like "package.json".
	pathLikeRe = regexp.MustCompile(`^[A-Za-z0-9_./@\-\[\]]+\.(?:ts|tsx|js|jsx|mjs|cjs|go|py|rs|java|rb|php|css|scss|html|json|yaml|yml|toml|md|sql|prisma|sh|env)$`)

	// wellKnownBareFiles are filenames that the gate recognizes even
	// without a directory prefix. Add to this list only when a
	// plain-filename claim in LLM output reliably means "this file".
	wellKnownBareFiles = map[string]struct{}{
		"package.json":   {},
		"tsconfig.json":  {},
		"next.config.js": {},
		"next.config.ts": {},
		"schema.prisma":  {},
		"Dockerfile":     {},
	}
)

// ParseProposedFiles scans the model's natural-language response and
// returns the unique, sorted list of file paths it claims to create or
// modify. The parser recognizes three conventions:
//
//  1. Unified-diff headers: "--- a/<path>" and "+++ b/<path>"
//  2. Fenced code blocks preceded by a path-bearing context line:
//     **`src/foo.ts`**:\n```ts\n...
//  3. Bullet / heading lines with a backtick-quoted path that looks like
//     a source file (contains "/" and ends in a known extension, OR
//     matches a well-known bare filename like "package.json").
//
// Paths returned are verbatim from the response (no normalization beyond
// stripping a leading "./" and trimming whitespace).
func ParseProposedFiles(responseContent string) []string {
	if strings.TrimSpace(responseContent) == "" {
		return nil
	}

	seen := make(map[string]struct{})
	add := func(raw string) {
		p := normalizePath(raw)
		if p == "" {
			return
		}
		if !looksLikePath(p) {
			return
		}
		seen[p] = struct{}{}
	}

	// 1. Unified-diff headers. Strong signal — these always mean "this
	// file is being changed".
	for _, m := range unifiedDiffHeaderRe.FindAllStringSubmatch(responseContent, -1) {
		if len(m) >= 2 {
			// Skip /dev/null — it's the "file was deleted / added" marker.
			if strings.HasSuffix(m[1], "/dev/null") {
				continue
			}
			add(m[1])
		}
	}

	// 2. Fenced code blocks. Walk each fence start and look at the line
	// immediately preceding it for a backtick-quoted path. This captures
	// the common "**`path/to/file.tsx`**:\n```ts\n" shape without needing
	// to parse the fence body.
	lines := strings.Split(responseContent, "\n")
	for i, line := range lines {
		if !fencedCodeBlockStartRe.MatchString(strings.TrimRight(line, " \t\r")) {
			continue
		}
		// Look backwards up to 3 non-empty lines for a path-bearing context.
		for back := 1; back <= 3 && i-back >= 0; back++ {
			prev := strings.TrimSpace(lines[i-back])
			if prev == "" {
				continue
			}
			for _, m := range bulletOrHeadingPathRe.FindAllStringSubmatch(prev, -1) {
				add(m[1])
			}
			break
		}
	}

	// 3. General bullet / heading scan: every backtick-quoted candidate in
	// the entire response. The pathLikeRe filter + wellKnownBareFiles
	// set keep this from matching arbitrary inline code snippets.
	for _, m := range bulletOrHeadingPathRe.FindAllStringSubmatch(responseContent, -1) {
		add(m[1])
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// normalizePath strips leading "./" and surrounding whitespace. We keep
// the rest of the path verbatim — normalizing slashes or case would
// introduce false equivalences on case-insensitive filesystems.
func normalizePath(raw string) string {
	p := strings.TrimSpace(raw)
	p = strings.TrimPrefix(p, "./")
	// Strip trailing punctuation the model sometimes leaves on path
	// references ("see src/foo.ts."). We only trim characters that are
	// never part of a legitimate path.
	p = strings.TrimRight(p, ".,;:)\"'")
	return p
}

// looksLikePath gates candidate strings against pathLikeRe and the
// well-known-bare-files allowlist. This prevents the bullet-scan pass
// from matching every inline ``code`` snippet in the response.
func looksLikePath(p string) bool {
	if p == "" {
		return false
	}
	if _, ok := wellKnownBareFiles[p]; ok {
		return true
	}
	// Reject strings with spaces — real paths don't have them in this
	// codebase, and the LLM uses spaces inside backticks for things like
	// `const foo = 1`.
	if strings.ContainsAny(p, " \t") {
		return false
	}
	return pathLikeRe.MatchString(p)
}

// MissingVerifications returns the subset of proposedPaths that were NOT
// the target of a codex_verify_imports call in toolCalls. The match is
// lenient: the gate considers a file "verified" if its path appears as a
// substring of the stringified Args payload for any codex_verify_imports
// invocation. That's loose enough to handle both the documented
// {"path": "..."} shape and any minor arg-name drift without over-
// engineering a JSON parser here.
//
// The empty-input case returns nil (not the empty slice) so callers can
// distinguish "no paths proposed" from "all paths verified" if they
// want — both produce a len==0 result and neither should trigger a
// re-prompt, but callers logging gate activity may want to differentiate.
func MissingVerifications(proposedPaths []string, toolCalls []ToolCall) []string {
	if len(proposedPaths) == 0 {
		return nil
	}

	// Build the set of path fragments that have been verified. We collect
	// the raw Args strings of every codex_verify_imports call; a proposed
	// path counts as verified if it appears inside any of them.
	var verifiedBlobs []string
	for _, tc := range toolCalls {
		if tc.Name != verifyImportsTool {
			continue
		}
		verifiedBlobs = append(verifiedBlobs, tc.Args)
	}

	var missing []string
	for _, p := range proposedPaths {
		if !pathAppearsIn(p, verifiedBlobs) {
			missing = append(missing, p)
		}
	}
	return missing
}

// pathAppearsIn returns true if p (or its basename) shows up as a
// substring of any of the blobs. We try both forms because some models
// pass only the filename in verify calls even when the proposal names a
// full path.
func pathAppearsIn(p string, blobs []string) bool {
	if p == "" || len(blobs) == 0 {
		return false
	}
	base := p
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		base = p[idx+1:]
	}
	for _, b := range blobs {
		if b == "" {
			continue
		}
		if strings.Contains(b, p) {
			return true
		}
		// Only fall back to basename match if the basename is meaningful
		// (more than one segment of name). Bare "x.ts" is too weak.
		if base != p && len(base) >= 5 && strings.Contains(b, base) {
			return true
		}
	}
	return false
}

// MissingWrites returns the subset of proposedPaths that were NOT the
// target of a write_file or edit_file tool call. This catches the common
// failure mode where the model describes files in prose (often under a
// "Plan" heading) without actually calling a write tool — the gate then
// forces it toward write_file/edit_file instead of codex_verify_imports,
// which was the prior behaviour and let the model "satisfy" the gate by
// verifying files it had never written.
func MissingWrites(proposedPaths []string, toolCalls []ToolCall) []string {
	if len(proposedPaths) == 0 {
		return nil
	}
	var writtenBlobs []string
	for _, tc := range toolCalls {
		if _, ok := writeTools[tc.Name]; !ok {
			continue
		}
		writtenBlobs = append(writtenBlobs, tc.Args)
	}
	var missing []string
	for _, p := range proposedPaths {
		if !pathAppearsIn(p, writtenBlobs) {
			missing = append(missing, p)
		}
	}
	return missing
}

// ForcedWritePrompt is the re-prompt used when the model described files
// it never wrote. The older ForcedVerifyPrompt assumed the model had
// written the file and only forgot to verify — the worse failure is that
// the model is still in plan-mode. This message points the model at
// write_file / edit_file directly.
func ForcedWritePrompt(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("GATE: your answer is not accepted yet. You described code for the files below but never called write_file or edit_file on any of them. There is no code on disk for this task yet.\n\n")
	for _, p := range missing {
		fmt.Fprintf(&sb, "- %s\n", p)
	}
	sb.WriteString("\nFor EACH file above, emit a tool_call NOW:\n")
	sb.WriteString("  - write_file if the file does not yet exist (pass the full file content).\n")
	sb.WriteString("  - edit_file if the file already exists (pass old_string + new_string matching exactly).\n\n")
	sb.WriteString("Do NOT call codex_verify_imports yet — verification runs after the write. Do NOT restate your plan. Do NOT put the code in your assistant message. Put it in the tool_call arguments.\n")
	return sb.String()
}

// ForcedVerifyPrompt returns the user-message body that instructs the
// model to call codex_verify_imports on each missing path. The language
// is short, direct, and non-optional — the whole point of the gate is
// that "please" didn't work. The returned string is safe to drop
// directly into a ChatMessage with Role=user.
func ForcedVerifyPrompt(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("GATE: your answer is not accepted yet. You proposed code for the files below but did not run codex_verify_imports on them. Call codex_verify_imports on EACH of the following paths now, then return your final answer.\n\n")
	for _, p := range missing {
		fmt.Fprintf(&sb, "- %s\n", p)
	}
	sb.WriteString("\nPass the file path and its full proposed content to each codex_verify_imports call. This is enforced — your answer will not be accepted until every file above has been verified.\n\n")
	sb.WriteString("Do NOT restate your plan. Do NOT summarize what you are about to do. Emit the tool_call(s) directly.")
	return sb.String()
}
