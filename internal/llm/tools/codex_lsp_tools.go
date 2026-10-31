package tools

import (
	"encoding/json"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// This file defines the six semantically-named tools that replaced the
// original monolithic `codex_lsp(method, …)` tool. Each wraps one LSP
// method (textDocument/references, textDocument/definition, etc.) behind
// a purpose-first name and description so small models reach for them
// organically from the question (rather than having to understand LSP
// semantics to dispatch). All six share the same lsp.Manager and Client
// cache — the dispatch happens in executor.go.
//
// Why the split: models' training data contains millions of direct
// function calls like `find_references(x)` / `goto_definition(…)` and
// approximately zero `proxy(method="references", …)` indirections. The
// monolithic tool was never used organically by gemma3:27b even when
// `method=workspace_symbol` was the exact right call.

// CodexFindReferences — "who uses this?" symbol lookup. Accepts either
// a free-form symbol name (resolved via workspace/symbol first) or an
// exact file + cursor position.
func CodexFindReferences() adapter.Tool {
	return adapter.Tool{
		Name: "codex_find_references",
		Description: "Find every file in the codebase that uses a given symbol (function, class, model, component, constant, type). " +
			"Use this whenever the question is \"how is X used?\", \"what uses this Model?\", \"where is this called from?\". " +
			"Prefer this over codex_grep for symbol lookups — it understands TypeScript/Go types and won't return string matches in comments. " +
			"Example: to answer \"how does this app use the Location Prisma model\", call codex_find_references(symbol=\"Location\"). " +
			"Example: for an exact cursor, codex_find_references(path=\"components/Foo.tsx\", line=12, character=7).",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"symbol":{"type":"string","description":"Symbol name to resolve via workspace/symbol first (e.g. \"Location\", \"renderMap\"). Either symbol OR path+line+character is required."},` +
			`"path":{"type":"string","description":"Repo-relative path of the file containing the symbol declaration. Use with line+character instead of symbol."},` +
			`"line":{"type":"integer","description":"1-based line number of the symbol (used with path+character)."},` +
			`"character":{"type":"integer","description":"0-based UTF-16 character offset within the line."},` +
			`"repo":{"type":"string","description":"Optional repo name when a product has multiple repositories."}` +
			`}}`),
	}
}

// CodexGotoDefinition — "where is X defined?" Jump to declaration.
func CodexGotoDefinition() adapter.Tool {
	return adapter.Tool{
		Name: "codex_goto_definition",
		Description: "Jump to where a symbol is defined. Use when the question is \"where is X defined?\" or \"show me the source of Y\". " +
			"Returns the file + range of the declaration. Parser-accurate — handles re-exports, barrel files, and TypeScript types correctly where grep would guess. " +
			"Example: to find where the createUser function is declared, codex_goto_definition(path=\"app/api/users/route.ts\", line=8, character=14) on the call site.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Repo-relative path of the file referencing the symbol."},` +
			`"line":{"type":"integer","description":"1-based line number."},` +
			`"character":{"type":"integer","description":"0-based UTF-16 character offset within the line."},` +
			`"repo":{"type":"string","description":"Optional repo name when a product has multiple repositories."}` +
			`},"required":["path","line","character"]}`),
	}
}

// CodexWorkspaceSymbols — "find me any symbol named like Y" across the
// whole project. The fastest way to answer "does this codebase have a
// Location model?" / "is there a helper for X?".
func CodexWorkspaceSymbols() adapter.Tool {
	return adapter.Tool{
		Name: "codex_workspace_symbols",
		Description: "Search the whole project for any symbol (class, function, variable, type, interface) matching the query. " +
			"Use this FIRST for \"does the codebase have an X?\" / \"find me the Y model / Z component\" — it returns every symbol name, file, and kind across the repo. " +
			"Prefer this over codex_grep when you're hunting a declaration by name. " +
			"Example: to answer \"does this repository have a Location model?\", call codex_workspace_symbols(query=\"Location\") — you get every class, interface, and type that matches, with their file paths.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"Symbol name or substring to match across the entire workspace (case-insensitive depending on server)."},` +
			`"limit":{"type":"integer","description":"Optional cap on results returned (servers typically return many)."},` +
			`"repo":{"type":"string","description":"Optional repo name when a product has multiple repositories."}` +
			`},"required":["query"]}`),
	}
}

// CodexDocumentSymbols — "what does this file export?" Outline of a
// single file.
func CodexDocumentSymbols() adapter.Tool {
	return adapter.Tool{
		Name: "codex_document_symbols",
		Description: "List every symbol declared in a single file (functions, classes, exports, types). " +
			"Use when you want an outline of a file without reading the whole thing — \"what does this module export?\" / \"what's in Foo.tsx?\". " +
			"Faster and more structured than codex_read_file when you only need the shape of the file. " +
			"Example: codex_document_symbols(path=\"lib/auth.ts\") returns every exported function and type in the file with line numbers.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Repo-relative path to the file."},` +
			`"repo":{"type":"string","description":"Optional repo name when a product has multiple repositories."}` +
			`},"required":["path"]}`),
	}
}

// CodexTypeInfo — "what's the type of X?" LSP hover at a cursor.
func CodexTypeInfo() adapter.Tool {
	return adapter.Tool{
		Name: "codex_type_info",
		Description: "Look up the inferred type, signature, or hover documentation for a symbol at a cursor position. " +
			"Use for \"what does this function return?\" / \"what's the shape of this variable?\" / \"what are the args of Y?\" — the language server knows, regex doesn't. " +
			"Example: codex_type_info(path=\"app/page.tsx\", line=42, character=10) on a variable name returns its inferred TypeScript type and any JSDoc.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Repo-relative path."},` +
			`"line":{"type":"integer","description":"1-based line number."},` +
			`"character":{"type":"integer","description":"0-based UTF-16 character offset within the line."},` +
			`"repo":{"type":"string","description":"Optional repo name when a product has multiple repositories."}` +
			`},"required":["path","line","character"]}`),
	}
}

// CodexDiagnostics — "does this file typecheck?" Authoritative compiler
// answer after an edit.
func CodexDiagnostics() adapter.Tool {
	return adapter.Tool{
		Name: "codex_diagnostics",
		Description: "Get the language server's errors and warnings for a file. This is the authoritative \"does this typecheck / compile?\" answer — " +
			"call it AFTER you've written or edited code to verify your change holds up. Returns every diagnostic (severity, line, message) the server has published for the file. " +
			"Example: codex_diagnostics(path=\"app/api/users/route.ts\") after an edit — empty list means clean, non-empty means fix before moving on.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Repo-relative path."},` +
			`"repo":{"type":"string","description":"Optional repo name when a product has multiple repositories."}` +
			`},"required":["path"]}`),
	}
}

// CodexLSPTools returns the six LSP-backed symbol tools. Callers include
// these alongside the other codex_* tools in the agent toolset.
func CodexLSPTools() []adapter.Tool {
	return []adapter.Tool{
		CodexFindReferences(),
		CodexGotoDefinition(),
		CodexWorkspaceSymbols(),
		CodexDocumentSymbols(),
		CodexTypeInfo(),
		CodexDiagnostics(),
	}
}
