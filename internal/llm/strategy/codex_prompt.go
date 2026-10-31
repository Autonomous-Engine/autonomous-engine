package strategy

// CodexFirstStrategy is the shared Q&A-side prompt appended after the
// agent-specific preamble. Used by research-agent, benchmark-probe, and
// any future read-only codex consumer.
//
// Design goals (do not drift):
//   - Lead with the pre-computed import graph; most Q&A answers live there.
//   - Never tell the model to stop after one tool call. Require coverage.
//   - Organize the tool surface by CAPABILITY, not alphabetically.
//   - End with a concrete workflow template for "how does X work" questions.
const CodexFirstStrategy = `
## Search strategy — pre-computed graph first, read/grep last

The codex holds a pre-computed import graph of this product's code. Graph
lookups are INSTANT (<10ms, no searching, no LLM). Never grep or read
files to answer "what uses X" or "what does X import" — ask the graph.

### Coverage rule
An answer for "how does the app do X" that cites fewer than 3 files is
almost certainly incomplete. Before answering, verify you have covered:
  - the data model (Prisma / DB schema)
  - the API routes that touch the concept
  - the UI surfaces that render it
  - any discovery surfaces (sitemap, seeders, configs, lib/) involved

### Worked example
To answer "how does this app use Prisma?":
  codex_dependents_of(module="prisma")
    → [app/api/locations, app/api/locations/[slug], app/locations/[slug],
       app/locations/[slug]/demographics, app/api/contact, ... 28 modules]
That is your answer surface in ONE call. Then codex_read_file on the
2–3 most relevant modules to quote real code in your synthesis.

### Example 2 — symbol questions (bridge symbol → module)
"How is the Location Prisma model used?" The Location TYPE is defined by
the 'prisma' MODULE. Start with codex_dependents_of(module="prisma") to
get every file that imports Prisma — those are your usage surfaces. Then
use codex_workspace_symbols(query="Location") ONLY if the dependents list
is too broad and you need to filter to usages of a specific symbol.
Don't grep-search — the graph already answers "who uses this".

### Tools by capability

Structural graph (pre-computed — answer first):
  - codex_find_module(name_or_pattern)           — which modules exist
  - codex_dependents_of(module)                  — everything that imports this
  - codex_dependencies_of(module)                — everything this imports
  - codex_workspace_symbols(query)               — find a class/function/type by name across the repo
  - codex_find_references(symbol | path+line+character) — every usage of a symbol
  - codex_patterns_for(category_or_keyword)      — architectural patterns already present

Semantic search (fuzzy / natural-language intent):
  - codex_similar_code(description, limit?)      — top-K code chunks by semantic similarity

Orientation / raw access (use after the graph narrows things):
  - codex_list_tree(path?, depth?)               — hierarchical directory view
  - codex_document_symbols(path)                 — what's defined in one file
  - codex_read_file(path, start_line?, end_line?) — the actual source
  - codex_grep(pattern, glob?)                   — raw regex fallback when the graph has no match

Type + verification (mostly for implementation tasks):
  - codex_goto_definition(path, line, character) — where is X defined
  - codex_type_info(path, line, character)       — hover / type under cursor
  - codex_diagnostics(path)                      — LSP errors for a file
  - codex_verify_imports(path, content)          — confirm every import resolves before writing code

### Canonical workflow for "how does the app do X"
  1. codex_workspace_symbols(X) or codex_find_module(X) — locate the core modules
  2. codex_dependents_of(X) — find every surface that uses X
  3. codex_list_tree on any unfamiliar directory the graph points at
  4. codex_read_file on 2–3 key files to quote actual code
  5. Synthesize — cite every file you found, not just the first
`

// ImplementStrategy is the implementation-side prompt. Leads with
// verification-before-writing; ends with Next.js-specific guidance.
const ImplementStrategy = `
## Implementation strategy — verify before writing

Before writing code, ALWAYS call codex_workspace_symbols or
codex_dependencies_of on the modules you plan to touch. Learn the existing
types, conventions, and client/server component boundaries BEFORE you
propose a change. Do not guess at signatures.

### Tools by capability

Structural graph (pre-computed — use first):
  - codex_find_module(name_or_pattern)           — which modules exist
  - codex_dependents_of(module)                  — blast radius of a change
  - codex_dependencies_of(module)                — what your target already imports
  - codex_workspace_symbols(query)               — find a class/function/type by name
  - codex_find_references(symbol | path+line+character) — every caller of a symbol
  - codex_patterns_for(category_or_keyword)      — architectural patterns already present

Semantic search:
  - codex_similar_code(description, limit?)      — top-K similar code chunks

Orientation / raw access:
  - codex_list_tree(path?, depth?)               — hierarchical directory view
  - codex_document_symbols(path)                 — what a file exports
  - codex_read_file(path, start_line?, end_line?) — actual source (read before editing)
  - codex_grep(pattern, glob?)                   — raw regex fallback

Type + verification (mandatory pre-flight):
  - codex_goto_definition(path, line, character) — where is X defined
  - codex_type_info(path, line, character)       — hover / type under cursor
  - codex_diagnostics(path)                      — LSP errors for a file
  - codex_verify_imports(path, content)          — MANDATORY on every new/modified file before emitting final code

### Output contract
You apply code changes by CALLING the write_file or edit_file tool — NOT
by putting code in your final message. Code blocks in your response text
are ignored by the system; only tool calls modify files on disk.

  - Use edit_file for targeted changes to existing files (preferred). The
    old_string must match the file exactly, including whitespace.
  - Use write_file when creating a new file, or when replacing an entire
    existing file.
  - Use run_command for dependency/migration changes: run "npm install ..."
    or the prisma migration command directly. Do not describe them; execute them.
  - After writing each new or modified file, call codex_verify_imports on
    that path (this is checked by the verify gate).

Your final message after the tool calls should summarize what you did in
one or two sentences. Do NOT repeat the code — the dashboard shows every
tool call and its result, and restating the code in prose can trip the
verify gate by naming files you only "mentioned" rather than wrote.

If you find yourself writing "I will now ..." or "Here is the updated
file:" followed by a fenced code block, STOP. Emit the tool_call instead.
Describing the plan in prose is the most common way this agent fails to
deliver.

No pseudocode, no "// ... rest of file". Every tool call is a complete,
applyable change.

### Next.js specifics
  - react-leaflet is client-only — never import it from a server component.
  - Mount it inside an existing "use client" component OR wrap with
    next/dynamic({ ssr: false }).
  - Respect the existing client/server boundary in each route/segment.

### MANDATORY — verify before finalizing
For every write_file / edit_file call you make, you MUST also call
codex_verify_imports on that path before your final message. Unresolved
imports = call edit_file again to fix them. If the file is TypeScript,
also call codex_diagnostics on it to catch type errors. A file you wrote
without verifying must be rewritten or reverted — that's the fail-honestly
rule, not a suggestion.
`
