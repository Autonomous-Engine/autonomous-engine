// Command codex-smoke exercises the codex_* tools (codex_list_tree,
// codex_verify_imports, and the six LSP-backed wrappers — find_references,
// goto_definition, workspace_symbols, document_symbols, type_info,
// diagnostics) end-to-end against a repo on disk. It's a developer-facing
// smoke check, not a product feature — the benchmark-probe is the real
// evaluation.
//
// Usage (inside an agent container):
//
//	codex-smoke --repo /repo-cache/<product>/<repo>/ --tool list_tree
//	codex-smoke --repo <path> --tool verify_imports --file app/page.tsx
//	codex-smoke --repo <path> --tool lsp --method document_symbol --file app/page.tsx
//
// It side-steps the full codex/vector/kafka wiring by synthesising the
// minimum product + repository metadata in a shim codex. That's enough
// to confirm the tool code path works; the real agents carry the real
// dependencies.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/lsp"
	"github.com/autonomousengine/autonomous-engine/internal/llm/tools/verify"
)

func main() {
	repo := flag.String("repo", "", "absolute path to repo root")
	tool := flag.String("tool", "list_tree", "list_tree | verify_imports | lsp")
	file := flag.String("file", "", "repo-relative file path (for verify_imports / lsp)")
	method := flag.String("method", "document_symbol", "LSP method (for tool=lsp)")
	query := flag.String("query", "", "workspace_symbol query (for tool=lsp method=workspace_symbol)")
	line := flag.Int("line", 0, "1-based line (for lsp references/definition/hover)")
	character := flag.Int("character", 0, "0-based UTF-16 character (for lsp references/definition/hover)")
	flag.Parse()

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "--repo is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	switch *tool {
	case "list_tree":
		runListTree(*repo)
	case "verify_imports":
		runVerifyImports(*repo, *file)
	case "lsp":
		runLSP(ctx, *repo, *method, *file, *line, *character, *query)
	default:
		fmt.Fprintf(os.Stderr, "unknown --tool %q\n", *tool)
		os.Exit(2)
	}
}

// runListTree exercises the codex_list_tree traversal logic by walking
// the filesystem with the same excludes the tool uses and printing the
// first 5 lines.
func runListTree(repo string) {
	// Reuse the in-tree walker via executor by spawning a minimal fake.
	entries, err := os.ReadDir(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "readdir: %v\n", err)
		os.Exit(1)
	}
	lines := []string{"./"}
	skip := map[string]bool{".git": true, "node_modules": true, ".next": true, "dist": true, "build": true}
	for _, e := range entries {
		if skip[e.Name()] {
			continue
		}
		if e.IsDir() {
			lines = append(lines, "  "+e.Name()+"/")
		} else {
			lines = append(lines, "  "+e.Name())
		}
	}
	for i, l := range lines {
		if i >= 5 {
			break
		}
		fmt.Println(l)
	}
}

// runVerifyImports calls the real verify package so we're testing the
// actual code path — not a reimplementation.
func runVerifyImports(repo, file string) {
	if file == "" {
		// Use a synthetic snippet with one legit + one bogus import.
		snippet := "import React from 'react'\nimport { foo } from 'template/leaflet'\nimport { bar } from './does-not-exist'\n"
		res, err := verify.VerifyImports(repo, "app/probe.ts", snippet)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify: %v\n", err)
			os.Exit(1)
		}
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		// maybe repo-relative
		data, err = os.ReadFile(repo + "/" + file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read file: %v\n", err)
			os.Exit(1)
		}
	}
	res, err := verify.VerifyImports(repo, file, string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
}

// runLSP drives the LSP manager end to end.
func runLSP(ctx context.Context, repo, method, file string, line, character int, query string) {
	mgr := lsp.NewManager()
	defer mgr.Close()

	client, language, err := mgr.ClientForRepo(ctx, repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "language detected: %s\n", language)

	var params map[string]any
	methodName := method

	if method == "workspace_symbol" {
		raw, err := client.Request(ctx, "workspace/symbol", map[string]any{"query": query})
		if err != nil {
			fmt.Fprintf(os.Stderr, "workspace/symbol: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(raw))
		return
	}
	if file == "" {
		fmt.Fprintln(os.Stderr, "--file is required for non-workspace_symbol methods")
		os.Exit(2)
	}
	full := repo + "/" + file
	data, err := os.ReadFile(full)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	uri := lsp.PathToURI(full)
	if err := client.OpenDoc(uri, language, string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "didOpen: %v\n", err)
		os.Exit(1)
	}
	pos := lsp.Position{Line: line - 1, Character: character}
	switch method {
	case "references":
		methodName = "textDocument/references"
		params = map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     pos,
			"context":      map[string]any{"includeDeclaration": true},
		}
	case "definition":
		methodName = "textDocument/definition"
		params = map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     pos,
		}
	case "hover":
		methodName = "textDocument/hover"
		params = map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     pos,
		}
	case "document_symbol":
		methodName = "textDocument/documentSymbol"
		params = map[string]any{"textDocument": map[string]any{"uri": uri}}
	case "diagnostics":
		// Give the server ~2s to publish diagnostics after didOpen.
		deadline := time.Now().Add(2 * time.Second)
		var diags []lsp.Diagnostic
		for time.Now().Before(deadline) {
			diags = client.Diagnostics(uri)
			if len(diags) > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		out, _ := json.MarshalIndent(diags, "", "  ")
		fmt.Println(string(out))
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown method %q\n", method)
		os.Exit(2)
	}
	raw, err := client.Request(ctx, methodName, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request %s: %v\n", methodName, err)
		os.Exit(1)
	}
	// Pretty-print + trim to a sensible size.
	var pretty any
	if err := json.Unmarshal(raw, &pretty); err != nil {
		fmt.Println(string(raw))
		return
	}
	out, _ := json.MarshalIndent(pretty, "", "  ")
	s := string(out)
	if len(s) > 4000 {
		s = s[:4000] + "\n... [truncated]"
	}
	fmt.Println(s)
}
