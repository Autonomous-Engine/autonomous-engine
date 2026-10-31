// Package jsresolve is the one place that knows which files a relative
// JavaScript/TypeScript module specifier can resolve to. The import
// verifier (internal/llm/tools/verify) and the JS impact graph
// (internal/checks) both use it, so a TypeScript ESM import written as
// "./x.js" resolves to x.ts everywhere or nowhere.
package jsresolve

import (
	"path"
	"strings"
)

// Extensions are appended to an extension-less (or unmatched) specifier,
// and tried as directory index files, in this order.
var Extensions = []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts", ".json", ".vue", ".svelte"}

// Candidates returns, in resolution order, the file paths a relative
// specifier may resolve to. base is the specifier already joined onto the
// importer's directory and cleaned, with '/' separators (an absolute path
// in slash form is fine). The order is: the file as written, extension
// appends, the TypeScript ESM source mapping (.js/.jsx → .ts/.tsx,
// .mjs → .mts, .cjs → .cts), then directory index files.
func Candidates(base string) []string {
	base = strings.TrimSuffix(base, "/")
	cands := make([]string, 0, 2*len(Extensions)+3)
	cands = append(cands, base)
	for _, e := range Extensions {
		cands = append(cands, base+e)
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	switch ext { // TypeScript ESM: import "./x.js" resolves to x.ts
	case ".js", ".jsx":
		cands = append(cands, stem+".ts", stem+".tsx")
	case ".mjs":
		cands = append(cands, stem+".mts")
	case ".cjs":
		cands = append(cands, stem+".cts")
	}
	for _, e := range Extensions {
		cands = append(cands, base+"/index"+e)
	}
	return cands
}

// Resolve returns the first candidate of base for which exists reports
// true. exists must report regular files only (not directories).
func Resolve(base string, exists func(string) bool) (string, bool) {
	for _, c := range Candidates(base) {
		if exists(c) {
			return c, true
		}
	}
	return "", false
}
