package verify

import (
	"os"
	"path/filepath"
	"testing"
)

// ─── parseTSConfigPaths tests ───────────────────────────────────────────────

func TestParseTSConfigPaths_AtAlias(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": {
				"@/*": ["./src/*"]
			}
		}
	}`)
	aliases := parseTSConfigPaths(data, "/project")
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(aliases))
	}
	if aliases[0].prefix != "@/" {
		t.Errorf("expected prefix \"@/\", got %q", aliases[0].prefix)
	}
	if len(aliases[0].targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(aliases[0].targets))
	}
	want := filepath.Join("/project", "src") + string(filepath.Separator)
	if aliases[0].targets[0] != want {
		t.Errorf("expected target %q, got %q", want, aliases[0].targets[0])
	}
}

func TestParseTSConfigPaths_TildeAlias(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"paths": {
				"~/*": ["./*"]
			}
		}
	}`)
	aliases := parseTSConfigPaths(data, "/app")
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(aliases))
	}
	if aliases[0].prefix != "~/" {
		t.Errorf("expected prefix \"~/\", got %q", aliases[0].prefix)
	}
}

func TestParseTSConfigPaths_MultipleAliases(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": {
				"@/*": ["./src/*"],
				"@lib/*": ["./lib/*"]
			}
		}
	}`)
	aliases := parseTSConfigPaths(data, "/project")
	if len(aliases) != 2 {
		t.Fatalf("expected 2 aliases, got %d", len(aliases))
	}
}

func TestParseTSConfigPaths_NoPaths(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"strict": true
		}
	}`)
	aliases := parseTSConfigPaths(data, "/project")
	if len(aliases) != 0 {
		t.Fatalf("expected 0 aliases, got %d", len(aliases))
	}
}

func TestParseTSConfigPaths_NoWildcard(t *testing.T) {
	// Exact-match patterns (no wildcard) should be skipped.
	data := []byte(`{
		"compilerOptions": {
			"paths": {
				"config": ["./src/config.ts"]
			}
		}
	}`)
	aliases := parseTSConfigPaths(data, "/project")
	if len(aliases) != 0 {
		t.Fatalf("expected 0 aliases (no wildcard), got %d", len(aliases))
	}
}

func TestParseTSConfigPaths_BaseUrlSubdir(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": "src",
			"paths": {
				"@/*": ["./*"]
			}
		}
	}`)
	aliases := parseTSConfigPaths(data, "/project")
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(aliases))
	}
	want := filepath.Join("/project", "src") + string(filepath.Separator)
	if aliases[0].targets[0] != want {
		t.Errorf("expected target %q, got %q", want, aliases[0].targets[0])
	}
}

func TestParseTSConfigPaths_MalformedJSON(t *testing.T) {
	data := []byte(`not json`)
	aliases := parseTSConfigPaths(data, "/project")
	if aliases != nil {
		t.Fatalf("expected nil for malformed JSON, got %v", aliases)
	}
}

// ─── resolvePathAlias tests ─────────────────────────────────────────────────

func TestResolvePathAlias_NoAliases(t *testing.T) {
	_, matched := resolvePathAlias("/repo", "@/foo", nil)
	if matched {
		t.Error("expected no match when aliases is nil")
	}
}

func TestResolvePathAlias_NoMatch(t *testing.T) {
	aliases := []pathAlias{{prefix: "@/", targets: []string{"/repo/src/"}}}
	_, matched := resolvePathAlias("/repo", "react", aliases)
	if matched {
		t.Error("expected no match for 'react' against '@/' alias")
	}
}

func TestResolvePathAlias_MatchButFileNotFound(t *testing.T) {
	aliases := []pathAlias{{prefix: "@/", targets: []string{"/nonexistent/path/"}}}
	resolved, matched := resolvePathAlias("/repo", "@/components/button", aliases)
	if !matched {
		t.Fatal("expected alias to match")
	}
	if resolved {
		t.Error("expected resolved=false when target doesn't exist")
	}
}

// ─── End-to-end VerifyImports with alias resolution ─────────────────────────

func TestVerifyImports_AtAliasResolvesToFile(t *testing.T) {
	// Set up a temp repo with:
	//   tsconfig.json  (paths: {"@/*": ["./src/*"]})
	//   src/components/button.ts
	//   app/page.tsx   (imports @/components/button)
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "tsconfig.json"), `{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": { "@/*": ["./src/*"] }
		}
	}`)
	writeFile(t, filepath.Join(tmp, "package.json"), `{
		"dependencies": { "react": "^18.0.0" }
	}`)
	mkdirAll(t, filepath.Join(tmp, "src", "components"))
	writeFile(t, filepath.Join(tmp, "src", "components", "button.ts"), `export const Button = () => {}`)
	mkdirAll(t, filepath.Join(tmp, "app"))

	src := `import { Button } from "@/components/button"
import React from "react"
`
	result, err := VerifyImports(tmp, "app/page.tsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Checked != 2 {
		t.Errorf("expected 2 checked, got %d", result.Checked)
	}
	if len(result.Unresolved) != 0 {
		t.Errorf("expected 0 unresolved, got %d: %v", len(result.Unresolved), result.Unresolved)
	}
}

func TestVerifyImports_AtAliasResolvesToIndexFile(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "tsconfig.json"), `{
		"compilerOptions": {
			"paths": { "@/*": ["./*"] }
		}
	}`)
	writeFile(t, filepath.Join(tmp, "package.json"), `{}`)
	mkdirAll(t, filepath.Join(tmp, "components", "ui"))
	writeFile(t, filepath.Join(tmp, "components", "ui", "index.ts"), `export const Card = () => {}`)

	src := `import { Card } from "@/components/ui"
`
	result, err := VerifyImports(tmp, "app/page.tsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Unresolved) != 0 {
		t.Errorf("expected 0 unresolved, got %d: %v", len(result.Unresolved), result.Unresolved)
	}
}

func TestVerifyImports_AtAliasFileNotExist(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "tsconfig.json"), `{
		"compilerOptions": {
			"paths": { "@/*": ["./src/*"] }
		}
	}`)
	writeFile(t, filepath.Join(tmp, "package.json"), `{}`)
	mkdirAll(t, filepath.Join(tmp, "src"))
	// Note: we do NOT create src/components/missing.ts

	src := `import { Missing } from "@/components/missing"
`
	result, err := VerifyImports(tmp, "app/page.tsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Unresolved) != 1 {
		t.Fatalf("expected 1 unresolved, got %d", len(result.Unresolved))
	}
	if result.Unresolved[0].Import != "@/components/missing" {
		t.Errorf("unexpected import: %q", result.Unresolved[0].Import)
	}
	if result.Unresolved[0].Reason != "path alias resolved but target file does not exist" {
		t.Errorf("unexpected reason: %q", result.Unresolved[0].Reason)
	}
}

func TestVerifyImports_ScopedNpmPackageStillWorks(t *testing.T) {
	// Ensure that a real scoped package like @radix-ui/react-dialog is NOT
	// treated as a path alias when there is no matching alias config.
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "tsconfig.json"), `{
		"compilerOptions": {
			"paths": { "@/*": ["./src/*"] }
		}
	}`)
	writeFile(t, filepath.Join(tmp, "package.json"), `{
		"dependencies": { "@radix-ui/react-dialog": "^1.0.0" }
	}`)

	src := `import { Dialog } from "@radix-ui/react-dialog"
`
	result, err := VerifyImports(tmp, "app/page.tsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "@radix-ui/react-dialog" does NOT start with "@/" so it should NOT
	// match the "@/" alias — it falls through to package.json lookup.
	if len(result.Unresolved) != 0 {
		t.Errorf("expected 0 unresolved for scoped package, got %d: %v", len(result.Unresolved), result.Unresolved)
	}
}

func TestVerifyImports_NoTSConfigFallsThrough(t *testing.T) {
	// Without a tsconfig.json, @/foo should fall through to the package check
	// and be flagged as unresolved (old behavior).
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "package.json"), `{
		"dependencies": { "react": "^18.0.0" }
	}`)

	src := `import { Foo } from "@/components/foo"
`
	result, err := VerifyImports(tmp, "app/page.tsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Unresolved) != 1 {
		t.Fatalf("expected 1 unresolved, got %d", len(result.Unresolved))
	}
	// Should be the old "not in package.json" reason, not the alias reason.
	if result.Unresolved[0].Reason != "not in package.json, not a built-in" {
		t.Errorf("unexpected reason: %q", result.Unresolved[0].Reason)
	}
}

func TestVerifyImports_JsConfigPaths(t *testing.T) {
	// jsconfig.json should work the same as tsconfig.json.
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "jsconfig.json"), `{
		"compilerOptions": {
			"paths": { "@/*": ["./*"] }
		}
	}`)
	writeFile(t, filepath.Join(tmp, "package.json"), `{}`)
	mkdirAll(t, filepath.Join(tmp, "lib"))
	writeFile(t, filepath.Join(tmp, "lib", "utils.js"), `module.exports = {}`)

	src := `import { utils } from "@/lib/utils"
`
	result, err := VerifyImports(tmp, "app/page.jsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Unresolved) != 0 {
		t.Errorf("expected 0 unresolved, got %d: %v", len(result.Unresolved), result.Unresolved)
	}
}

func TestVerifyImports_RelativeImportUnchanged(t *testing.T) {
	// Ensure relative imports still work as before (regression check).
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "tsconfig.json"), `{
		"compilerOptions": {
			"paths": { "@/*": ["./src/*"] }
		}
	}`)
	writeFile(t, filepath.Join(tmp, "package.json"), `{}`)
	mkdirAll(t, filepath.Join(tmp, "app"))
	writeFile(t, filepath.Join(tmp, "app", "helper.ts"), `export const x = 1`)

	src := `import { x } from "./helper"
`
	result, err := VerifyImports(tmp, "app/page.tsx", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Unresolved) != 0 {
		t.Errorf("expected 0 unresolved for relative import, got %d: %v", len(result.Unresolved), result.Unresolved)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeFile %s: %v", path, err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdirAll %s: %v", path, err)
	}
}
