package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── Go regex patterns ────────────────────────────────────────────────────────

var (
	// Package declaration: package foo
	reGoPackage = regexp.MustCompile(`(?m)^package\s+(\w+)`)

	// Function / method declaration.
	// Group 1: optional receiver  "(recv Type)"
	// Group 2: function name
	// Group 3: parameter list (inside outer parens)
	// Group 4: return type (single word, pointer, or named-return tuple)
	reGoFunc = regexp.MustCompile(
		`(?m)^func\s+(\(\s*\*?\w[\w\s\*,\[\]]*\)\s+)?(\w+)\s*` +
			`(<[^>]*>)?\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)\s*` +
			`((?:\([^)]*\)|\*?\[?\]?\w[\w\.\*\[\]]*(?:\s*,\s*\*?\[?\]?\w[\w\.\*\[\]]*)?))?`,
	)

	// Struct definition: type Foo struct {
	reGoStruct = regexp.MustCompile(`(?m)^type\s+(\w+)\s+struct\s*\{`)

	// Interface definition: type Foo interface {
	reGoInterface = regexp.MustCompile(`(?m)^type\s+(\w+)\s+interface\s*\{`)

	// Named type alias / definition: type Foo Bar  (catches everything not struct/interface)
	reGoTypeAlias = regexp.MustCompile(`(?m)^type\s+(\w+)(?:\s*\[.*?\])?\s+([\w\.\[\]\*]+.*)`)

	// Const block or standalone const: const ( X = ... ) or const X = ...
	reGoConstBlock = regexp.MustCompile(`(?s)const\s*\(([^)]*)\)`)
	reGoConstSingle = regexp.MustCompile(`(?m)^const\s+(\w+)\s+`)
	reGoConstLine   = regexp.MustCompile(`(?m)^\s*(\w+)\s*(?:\w[\w\.\[\]\*]*)?\s*=`)

	// Var declarations (exported)
	reGoVar = regexp.MustCompile(`(?m)^var\s+(\w+)\s+`)

	// Import block and single imports
	reGoImportBlock  = regexp.MustCompile(`(?s)import\s*\(([^)]*)\)`)
	reGoImportSingle = regexp.MustCompile(`(?m)^import\s+(?:(\w+)\s+)?"([^"]+)"`)
	reGoImportLine   = regexp.MustCompile(`(?m)^\s*(?:(\w+)\s+)?"([^"]+)"`)

	// Doc comments immediately preceding a declaration
	reGoDocComment = regexp.MustCompile(`(?m)((?:^\s*//[^\n]*\n)+)\s*(?:func|type|var|const)\s`)
)

// analyzeGo performs deep static analysis of a Go source file.
func analyzeGo(filePath, content string) *FileAnalysis {
	fa := &FileAnalysis{
		FilePath:  filePath,
		Language:  "Go",
		LineCount: countLines(content),
	}

	// ── Package ───────────────────────────────────────────────────────────────
	if m := reGoPackage.FindStringSubmatchIndex(content); m != nil {
		fa.Package = content[m[2]:m[3]]
	}

	// ── Imports ───────────────────────────────────────────────────────────────
	// Grouped import blocks
	for _, blockMatch := range reGoImportBlock.FindAllStringSubmatchIndex(content, -1) {
		blockBody := content[blockMatch[2]:blockMatch[3]]
		baseOffset := blockMatch[2]
		for _, lm := range reGoImportLine.FindAllStringSubmatchIndex(blockBody, -1) {
			alias := ""
			path := ""
			if lm[2] >= 0 {
				alias = blockBody[lm[2]:lm[3]]
			}
			if lm[4] >= 0 {
				path = blockBody[lm[4]:lm[5]]
			}
			if path == "" {
				continue
			}
			imp := Import{
				Module: path,
				Alias:  alias,
				Line:   lineNumber(content, baseOffset+lm[0]),
			}
			fa.Imports = append(fa.Imports, imp)
		}
	}
	// Single-line imports
	for _, m := range reGoImportSingle.FindAllStringSubmatchIndex(content, -1) {
		alias := ""
		path := ""
		if m[2] >= 0 {
			alias = content[m[2]:m[3]]
		}
		if m[4] >= 0 {
			path = content[m[4]:m[5]]
		}
		if path == "" {
			continue
		}
		fa.Imports = append(fa.Imports, Import{
			Module: path,
			Alias:  alias,
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Doc comment index ──────────────────────────────────────────────────
	// Build a map from declaration start offset → doc comment text.
	docByOffset := map[int]string{}
	for _, dm := range reGoDocComment.FindAllStringSubmatchIndex(content, -1) {
		if dm[2] < 0 {
			continue
		}
		raw := content[dm[2]:dm[3]]
		lines := strings.Split(strings.TrimSpace(raw), "\n")
		cleaned := make([]string, 0, len(lines))
		for _, l := range lines {
			cleaned = append(cleaned, trimComment(strings.TrimSpace(l)))
		}
		// The declaration starts just after the comment block.
		docByOffset[dm[3]] = strings.Join(cleaned, " ")
	}

	// ── Structs ───────────────────────────────────────────────────────────────
	for _, m := range reGoStruct.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		sym := Symbol{
			Kind:       KindStruct,
			Name:       name,
			Signature:  "type " + name + " struct",
			Exported:   isExportedGoName(name),
			Line:       lineNumber(content, m[0]),
			DocComment: nearestDoc(docByOffset, m[0]),
		}
		fa.Symbols = append(fa.Symbols, sym)
	}

	// ── Interfaces ────────────────────────────────────────────────────────────
	for _, m := range reGoInterface.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		sym := Symbol{
			Kind:       KindInterface,
			Name:       name,
			Signature:  "type " + name + " interface",
			Exported:   isExportedGoName(name),
			Line:       lineNumber(content, m[0]),
			DocComment: nearestDoc(docByOffset, m[0]),
		}
		fa.Symbols = append(fa.Symbols, sym)
	}

	// ── Type aliases / named types ─────────────────────────────────────────
	for _, m := range reGoTypeAlias.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		underlying := strings.TrimSpace(content[m[4]:m[5]])
		// Skip if we already captured as struct/interface
		if strings.HasPrefix(underlying, "struct") || strings.HasPrefix(underlying, "interface") {
			continue
		}
		sym := Symbol{
			Kind:       KindType,
			Name:       name,
			Signature:  "type " + name + " " + underlying,
			ReturnType: underlying,
			Exported:   isExportedGoName(name),
			Line:       lineNumber(content, m[0]),
			DocComment: nearestDoc(docByOffset, m[0]),
		}
		fa.Symbols = append(fa.Symbols, sym)
	}

	// ── Constants ─────────────────────────────────────────────────────────────
	for _, blockMatch := range reGoConstBlock.FindAllStringSubmatchIndex(content, -1) {
		blockBody := content[blockMatch[2]:blockMatch[3]]
		baseOffset := blockMatch[2]
		for _, cm := range reGoConstLine.FindAllStringSubmatchIndex(blockBody, -1) {
			if cm[2] < 0 {
				continue
			}
			name := blockBody[cm[2]:cm[3]]
			if name == "_" {
				continue
			}
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind:      KindConstant,
				Name:      name,
				Signature: strings.TrimSpace(blockBody[cm[0]:cm[1]]),
				Exported:  isExportedGoName(name),
				Line:      lineNumber(content, baseOffset+cm[0]),
			})
		}
	}
	for _, m := range reGoConstSingle.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindConstant,
			Name:      name,
			Signature: strings.TrimSpace(content[m[0]:m[1]]),
			Exported:  isExportedGoName(name),
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Exported vars ─────────────────────────────────────────────────────────
	for _, m := range reGoVar.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if !isExportedGoName(name) {
			continue
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindVariable,
			Name:      name,
			Signature: strings.TrimSpace(content[m[0]:m[1]]),
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Functions & Methods ───────────────────────────────────────────────────
	for _, m := range reGoFunc.FindAllStringSubmatchIndex(content, -1) {
		if m[4] < 0 {
			continue
		}
		name := content[m[4]:m[5]]
		if name == "" {
			continue
		}

		receiver := ""
		if m[2] >= 0 {
			receiver = strings.TrimSpace(content[m[2]:m[3]])
		}

		params := ""
		if m[8] >= 0 {
			params = strings.TrimSpace(content[m[8]:m[9]])
		}

		returnType := ""
		if m[10] >= 0 {
			returnType = strings.TrimSpace(content[m[10]:m[11]])
		}

		sig := buildGoSignature(receiver, name, params, returnType)

		kind := KindFunction
		parent := ""
		if receiver != "" {
			kind = KindMethod
			// Extract the type name from "(r *Foo)" → "Foo"
			parent = extractGoReceiverType(receiver)
		}

		sym := Symbol{
			Kind:       kind,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Parent:     parent,
			Exported:   isExportedGoName(name),
			Line:       lineNumber(content, m[0]),
			DocComment: nearestDoc(docByOffset, m[0]),
		}
		fa.Symbols = append(fa.Symbols, sym)
	}

	return fa
}

// buildGoSignature reassembles a Go function signature from its parts.
func buildGoSignature(receiver, name, params, returnType string) string {
	var b strings.Builder
	b.WriteString("func ")
	if receiver != "" {
		b.WriteString(receiver)
		b.WriteByte(' ')
	}
	b.WriteString(name)
	b.WriteByte('(')
	b.WriteString(params)
	b.WriteByte(')')
	if returnType != "" {
		b.WriteByte(' ')
		b.WriteString(returnType)
	}
	return b.String()
}

// extractGoReceiverType strips the variable name and pointer from a Go
// receiver like "(s *Server)" → "Server".
func extractGoReceiverType(recv string) string {
	// Remove surrounding parens
	recv = strings.Trim(recv, "()")
	recv = strings.TrimSpace(recv)
	parts := strings.Fields(recv)
	if len(parts) == 0 {
		return ""
	}
	// Last part is the type, possibly with *
	t := parts[len(parts)-1]
	t = strings.TrimPrefix(t, "*")
	t = strings.TrimPrefix(t, "[")
	t = strings.TrimSuffix(t, "]")
	return t
}

// splitParams splits a parameter list string by commas (naive — good enough
// for regex-level analysis where we do not need to parse generic brackets).
func splitParams(params string) []string {
	if params == "" {
		return nil
	}
	parts := strings.Split(params, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// nearestDoc looks for a doc comment whose end offset is close to (but not
// past) the declaration at declOffset. We search within a 512-byte window.
func nearestDoc(docs map[int]string, declOffset int) string {
	best := ""
	bestDist := 1<<31 - 1
	for endOff, text := range docs {
		dist := declOffset - endOff
		if dist >= 0 && dist < 512 && dist < bestDist {
			bestDist = dist
			best = text
		}
	}
	return best
}
