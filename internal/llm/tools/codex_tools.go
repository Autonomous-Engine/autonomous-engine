package tools

import (
	"encoding/json"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// This file defines the "codex tools" — read-only tools backed by the
// project codex (Postgres) and the vector store (Qdrant). They turn the
// codex from "static prompt dump" into a knowledge graph the agent can
// actively query. The corresponding executor methods live in executor.go.
//
// Every schema here is re-sent on every LLM call of every agent that
// offers it (R1 measured ~5.2k tokens of schemas per research call), so
// descriptions state what the tool returns and its one common misuse, and
// nothing else. The research base set is held to 8,000 chars of JSON by
// TestResearchBaseToolSchemasFitTheBudget.

// CodexFindModule returns a tool definition for finding modules by name
// or purpose substring. This is the agent's primary entry point for
// "what's in this codebase?" — always try it before keyword grep.
func CodexFindModule() adapter.Tool {
	return adapter.Tool{
		Name: "codex_find_module",
		Description: "Find codex modules whose name or purpose contains a substring. Returns name, purpose, " +
			"interface, dependencies, dependents and risk. Use a short identifier (\"auth\", \"locations\"), " +
			"not prose: \"server routes\" matches nothing. To find a file by name, use codex_list_tree.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"name_or_pattern":{"type":"string","description":"Case-insensitive substring of a module name or purpose"}},"required":["name_or_pattern"]}`),
	}
}

// CodexDependentsOf returns a tool definition for one-hop reverse dependency
// lookup. Use instead of grep'ing for imports.
func CodexDependentsOf() adapter.Tool {
	return adapter.Tool{
		Name:        "codex_dependents_of",
		Description: "List the modules that import the named module (one hop). Use it for blast radius instead of grepping for imports.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"module_name":{"type":"string","description":"Exact module name from codex_find_module"}},"required":["module_name"]}`),
	}
}

// CodexDependenciesOf returns a tool definition for one-hop forward dependency
// lookup. Use instead of grep'ing for imports.
func CodexDependenciesOf() adapter.Tool {
	return adapter.Tool{
		Name:        "codex_dependencies_of",
		Description: "List the modules the named module imports (one hop).",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"module_name":{"type":"string","description":"Exact module name from codex_find_module"}},"required":["module_name"]}`),
	}
}

// CodexSimilarCode returns a tool definition for semantic similarity search
// over the indexed code chunks in Qdrant. Use for "is there already code
// that does X?" questions.
func CodexSimilarCode() adapter.Tool {
	return adapter.Tool{
		Name: "codex_similar_code",
		Description: "Semantic search over the indexed code: the top matches (file, function, line, score) " +
			"for a natural-language description. Use it when you do not know the symbol name.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","description":"What the code does, e.g. \"retry with exponential backoff\""},"limit":{"type":"integer","description":"Max results (default 5, max 20)"}},"required":["description"]}`),
	}
}

// CodexPatternsFor returns a tool definition for looking up established
// patterns and anti-patterns by category or keyword.
func CodexPatternsFor() adapter.Tool {
	return adapter.Tool{
		Name:        "codex_patterns_for",
		Description: "The project's established patterns and anti-patterns for a category or keyword (e.g. \"testing\", \"error_handling\", \"auth\").",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"category_or_keyword":{"type":"string","description":"Category or keyword"}},"required":["category_or_keyword"]}`),
	}
}

// CodexReadFile returns a tool definition for reading a slice of a file
// from the repo-cache. This is the fallback primitive when
// codex_similar_code points at a file and the model needs the actual
// source to proceed (e.g. implement against a function signature).
func CodexReadFile() adapter.Tool {
	return adapter.Tool{
		Name: "codex_read_file",
		Description: "Read a line range of a repository file; returns the lines and total_lines. " +
			"Default up to 400 lines (4,800 chars), up to 2000 with a range. Read files the graph or a search pointed at, not to explore.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Repo-relative path"},"start_line":{"type":"integer","description":"1-based first line (default 1)"},"end_line":{"type":"integer","description":"1-based last line, inclusive"},"repo":{"type":"string","description":"Repo name if the product has several"}},"required":["path"]}`),
	}
}

// CodexGrep returns a tool definition for regex search across the
// indexed repository. This is the escape hatch when codex_similar_code
// has no good match — raw grep over the repo-cache.
func CodexGrep() adapter.Tool {
	return adapter.Tool{
		Name: "codex_grep",
		Description: "Regex search (Go syntax) of file CONTENTS; returns file, line and text. It does not match " +
			"file names: use codex_list_tree for that. Skips node_modules, .git, .next, dist, build.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Go regex, e.g. 'func\\s+Handle\\w+'"},"glob":{"type":"string","description":"Path glob, e.g. '**/*.tsx'"},"max_hits":{"type":"integer","description":"Default 50, cap 200"},"case_insensitive":{"type":"boolean"},"repo":{"type":"string","description":"Repo name if the product has several"}},"required":["pattern"]}`),
	}
}

// The language-server-backed structural tools (codex_find_references,
// codex_goto_definition, codex_workspace_symbols, codex_document_symbols,
// codex_type_info, codex_diagnostics) live in codex_lsp_tools.go — they
// are semantically-named wrappers over one shared LSP client, registered
// by CodexLSPTools().

// CodexListTree returns a tool definition for a hierarchical directory listing.
// Use when the agent needs to orient itself in a new area of the code.
func CodexListTree() adapter.Tool {
	return adapter.Tool{
		Name: "codex_list_tree",
		Description: "Indented directory tree up to depth levels (default 2, cap 5); skips node_modules, .git, " +
			".next, dist, build. Use it to orient and to find files by name.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Repo-relative directory (default '.')"},` +
			`"depth":{"type":"integer"},` +
			`"exclude":{"type":"array","items":{"type":"string"},"description":"Extra globs to skip"},` +
			`"repo":{"type":"string","description":"Repo name if the product has several"}` +
			`}}`),
	}
}


// CodexVerifyImports returns a tool definition for fast pre-LSP import checking.
// Catches hallucinated import specs (e.g. "template/leaflet") in <200ms.
func CodexVerifyImports() adapter.Tool {
	return adapter.Tool{
		Name: "codex_verify_imports",
		Description: "Check that every import in a file's content resolves (package.json packages, relative " +
			"paths, Node built-ins); returns the unresolved ones. Call it on each file you wrote or edited.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"Repo-relative path of the file"},` +
			`"content":{"type":"string","description":"The file body"},` +
			`"repo":{"type":"string","description":"Repo name if the product has several"}` +
			`},"required":["path","content"]}`),
	}
}

// CodexContractEdges returns a tool definition for querying producer/consumer
// contract edges from the codex_contract_edges table.
func CodexContractEdges() adapter.Tool {
	return adapter.Tool{
		Name: "codex_contract_edges",
		Description: "Query producer/consumer contract edges from the codex. Returns edges " +
			"(API routes, event topics, shared types, etc.) that link a producer module to its " +
			"consumers, or vice versa. PREFER THIS when assessing cross-service blast radius — " +
			"it tells you which files produce or consume a named contract without manual grep.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"contract_name":{"type":"string","description":"Name of the contract to look up (e.g. an API route, event topic, or shared type name)"},` +
			`"direction":{"type":"string","enum":["producer","consumer","both"],"description":"Filter edges by role: 'producer' = files that define the contract, 'consumer' = files that consume it, 'both' (default) = all edges"}` +
			`},"required":["contract_name"]}`),
	}
}

// CodexTools returns the codex-backed query tools. They are read-only
// (the write-side LSP diagnostics are still read-only from the repo's
// perspective) and safe to include in any agent's toolset. When the
// executor is not wired up with the corresponding dependencies, calling
// a tool returns a clear "fall back to grep" error.
func CodexTools() []adapter.Tool {
	tools := []adapter.Tool{
		CodexFindModule(),
		CodexDependentsOf(),
		CodexDependenciesOf(),
		CodexSimilarCode(),
		CodexPatternsFor(),
		CodexReadFile(),
		CodexGrep(),
		CodexListTree(),
		CodexVerifyImports(),
		CodexContractEdges(),
	}
	// Six semantically-named LSP wrappers (defined in codex_lsp_tools.go).
	tools = append(tools, CodexLSPTools()...)
	return tools
}
