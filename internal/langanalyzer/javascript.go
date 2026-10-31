package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── JavaScript regex patterns ────────────────────────────────────────────────

var (
	// Named function declarations: [export] [async] function [*] name(params)
	reJSFunction = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+(?:default\s+)?)?(async\s+)?function(\s*\*)?\s+(\w+)\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)`,
	)

	// Arrow / const function: [export] const name = [async] ([params]) =>
	// Also catches: const name = function(...)
	reJSArrowConst = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+)?(const|let|var)\s+(\w+)\s*=\s*(async\s+)?(?:function\s*\*?\s*\(([^)]*)\)|(?:\(([^)]*)\)|(\w+))\s*=>)`,
	)

	// Class declarations: [export] [abstract] class Name [extends Base] [implements ...] {
	reJSClass = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+(?:default\s+)?)?class\s+(\w+)` +
			`(?:\s+extends\s+([\w\.\<\>]+))?` +
			`(?:\s+implements\s+([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// Method inside a class body (indented):
	//   [static] [async] [get|set] [#]name(params) {
	//   [static] [async] * name(params) {  (generator)
	reJSMethod = regexp.MustCompile(
		`(?m)^([ \t]+)(static\s+)?(async\s+)?(\*\s*)?(get\s+|set\s+)?(#?\w+)\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)\s*(?::[^{]*)?\s*\{`,
	)

	// import { A, B } from 'module'
	reJSImportNamed = regexp.MustCompile(
		`(?m)^[ \t]*import\s+\{([^}]+)\}\s+from\s+['"]([^'"]+)['"]`,
	)

	// import DefaultExport from 'module'
	reJSImportDefault = regexp.MustCompile(
		`(?m)^[ \t]*import\s+(\w+)\s+from\s+['"]([^'"]+)['"]`,
	)

	// import * as Alias from 'module'
	reJSImportAll = regexp.MustCompile(
		`(?m)^[ \t]*import\s+\*\s+as\s+(\w+)\s+from\s+['"]([^'"]+)['"]`,
	)

	// import 'module'  (side-effect import)
	reJSSideEffectImport = regexp.MustCompile(
		`(?m)^[ \t]*import\s+['"]([^'"]+)['"]`,
	)

	// require('module') / require("module")
	reJSRequire = regexp.MustCompile(
		`(?m)(?:const|let|var)\s+(?:\{[^}]+\}|\w+)\s*=\s*require\(['"]([^'"]+)['"]\)`,
	)

	// export { A, B [as C] }
	reJSExportNamed = regexp.MustCompile(
		`(?m)^[ \t]*export\s+\{([^}]+)\}(?:\s+from\s+['"]([^'"]+)['"])?`,
	)

	// export default expr / export default function name / export default class name
	reJSExportDefault = regexp.MustCompile(
		`(?m)^[ \t]*export\s+default\s+(?:(?:async\s+)?function\s*\*?\s*(\w+)?|class\s+(\w+)?|(\w+))`,
	)

	// JSDoc comment: /** ... */
	reJSDoc = regexp.MustCompile(`(?s)/\*\*.*?\*/`)

	// module.exports = { ... } or module.exports.name = ...
	reJSModuleExports = regexp.MustCompile(
		`(?m)^[ \t]*module\.exports\s*=\s*`,
	)
)

// analyzeJavaScript performs deep static analysis of a JavaScript source file.
func analyzeJavaScript(filePath, content string) *FileAnalysis {
	fa := &FileAnalysis{
		FilePath:  filePath,
		Language:  "JavaScript",
		LineCount: countLines(content),
	}

	// Build JSDoc index: map from end-of-comment byte offset → cleaned comment.
	jsDocByEnd := buildJSDocIndex(content)

	// ── Named imports ─────────────────────────────────────────────────────────
	for _, m := range reJSImportNamed.FindAllStringSubmatchIndex(content, -1) {
		names := splitJSNames(content[m[2]:m[3]])
		mod := content[m[4]:m[5]]
		fa.Imports = append(fa.Imports, Import{
			Module: mod,
			Names:  names,
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Default imports ───────────────────────────────────────────────────────
	for _, m := range reJSImportDefault.FindAllStringSubmatchIndex(content, -1) {
		alias := content[m[2]:m[3]]
		mod := content[m[4]:m[5]]
		fa.Imports = append(fa.Imports, Import{
			Module: mod,
			Alias:  alias,
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Namespace imports ─────────────────────────────────────────────────────
	for _, m := range reJSImportAll.FindAllStringSubmatchIndex(content, -1) {
		alias := content[m[2]:m[3]]
		mod := content[m[4]:m[5]]
		fa.Imports = append(fa.Imports, Import{
			Module: mod,
			Alias:  alias + " (*)",
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Side-effect imports ───────────────────────────────────────────────────
	for _, m := range reJSSideEffectImport.FindAllStringSubmatchIndex(content, -1) {
		fa.Imports = append(fa.Imports, Import{
			Module: content[m[2]:m[3]],
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── require() calls ───────────────────────────────────────────────────────
	for _, m := range reJSRequire.FindAllStringSubmatchIndex(content, -1) {
		fa.Imports = append(fa.Imports, Import{
			Module: content[m[2]:m[3]],
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Named function declarations ───────────────────────────────────────────
	for _, m := range reJSFunction.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		async := m[6] >= 0 && content[m[6]:m[7]] != ""
		generator := m[8] >= 0 && content[m[8]:m[9]] != ""
		name := content[m[10]:m[11]]
		params := ""
		if m[12] >= 0 {
			params = strings.TrimSpace(content[m[12]:m[13]])
		}

		prefix := buildJSFuncPrefix(exported, async, generator)
		sig := prefix + "function " + name + "(" + params + ")"

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindFunction,
			Name:       name,
			Signature:  sig,
			Parameters: splitParams(params),
			Exported:   exported,
			Line:       lineNumber(content, m[0]),
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── Arrow / const function expressions ───────────────────────────────────
	for _, m := range reJSArrowConst.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		name := content[m[8]:m[9]]
		async := m[10] >= 0 && content[m[10]:m[11]] != ""

		// Try to find params from the various capture groups
		params := ""
		for _, gi := range []int{12, 14, 16} {
			if gi+1 < len(m) && m[gi] >= 0 {
				params = strings.TrimSpace(content[m[gi]:m[gi+1]])
				break
			}
		}

		sig := ""
		if async {
			sig = "async "
		}
		sig += "const " + name + " = "
		if async {
			sig += "async "
		}
		sig += "(" + params + ") =>"

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindFunction,
			Name:       name,
			Signature:  sig,
			Parameters: splitParams(params),
			Exported:   exported,
			Line:       lineNumber(content, m[0]),
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── Class declarations ────────────────────────────────────────────────────
	for _, m := range reJSClass.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		name := content[m[6]:m[7]]
		extends := ""
		if m[8] >= 0 {
			extends = strings.TrimSpace(content[m[8]:m[9]])
		}
		implements := []string(nil)
		if m[10] >= 0 {
			implements = splitJSNames(content[m[10]:m[11]])
		}

		sig := "class " + name
		if extends != "" {
			sig += " extends " + extends
		}

		sym := Symbol{
			Kind:       KindClass,
			Name:       name,
			Signature:  sig,
			Extends:    extends,
			Implements: implements,
			Exported:   exported,
			Line:       lineNumber(content, m[0]),
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		}
		fa.Symbols = append(fa.Symbols, sym)
	}

	// ── Methods inside class bodies ───────────────────────────────────────────
	// Find the enclosing class for each method by offset.
	classRanges := buildJSClassRanges(content, reJSClass)
	for _, m := range reJSMethod.FindAllStringSubmatchIndex(content, -1) {
		indent := content[m[2]:m[3]]
		// Only consider lines indented (i.e. inside a class/object body).
		if len(strings.TrimLeft(indent, " \t")) != 0 {
			continue
		}
		if len(indent) == 0 {
			continue
		}

		static := m[4] >= 0 && content[m[4]:m[5]] != ""
		async := m[6] >= 0 && content[m[6]:m[7]] != ""
		generator := m[8] >= 0 && content[m[8]:m[9]] != ""
		accessor := ""
		if m[10] >= 0 {
			accessor = strings.TrimSpace(content[m[10]:m[11]])
		}
		name := content[m[12]:m[13]]
		// Skip constructor captured here — handled separately if needed.
		if name == "constructor" || name == "if" || name == "for" || name == "while" || name == "switch" {
			continue
		}
		params := ""
		if m[14] >= 0 {
			params = strings.TrimSpace(content[m[14]:m[15]])
		}

		parent := findParentClass(classRanges, m[0])

		prefix := buildJSMethodPrefix(static, async, generator, accessor)
		sig := prefix + name + "(" + params + ")"

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindMethod,
			Name:       name,
			Signature:  sig,
			Parameters: splitParams(params),
			Parent:     parent,
			Exported:   true,
			Line:       lineNumber(content, m[0]),
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── Named exports ─────────────────────────────────────────────────────────
	for _, m := range reJSExportNamed.FindAllStringSubmatchIndex(content, -1) {
		names := splitJSNames(content[m[2]:m[3]])
		reexportMod := ""
		if m[4] >= 0 {
			reexportMod = content[m[4]:m[5]]
		}
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			sig := "export { " + n + " }"
			if reexportMod != "" {
				sig += " from '" + reexportMod + "'"
			}
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind:      KindExport,
				Name:      n,
				Signature: sig,
				Exported:  true,
				Line:      lineNumber(content, m[0]),
			})
		}
	}

	// ── Default export ────────────────────────────────────────────────────────
	for _, m := range reJSExportDefault.FindAllStringSubmatchIndex(content, -1) {
		name := ""
		for _, gi := range []int{2, 4, 6} {
			if gi < len(m) && m[gi] >= 0 {
				name = content[m[gi]:m[gi+1]]
				break
			}
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindExport,
			Name:      name,
			Signature: strings.TrimSpace(content[m[0]:m[1]]),
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	return fa
}

// ─── JS/TS shared helpers ─────────────────────────────────────────────────────

// buildJSDocIndex maps the byte offset of each JSDoc comment's closing */ to
// its cleaned text.
func buildJSDocIndex(content string) map[int]string {
	result := map[int]string{}
	for _, m := range reJSDoc.FindAllStringIndex(content, -1) {
		raw := content[m[0]:m[1]]
		// Strip /** ... */ and asterisk leaders
		raw = strings.TrimPrefix(raw, "/**")
		raw = strings.TrimSuffix(raw, "*/")
		lines := strings.Split(raw, "\n")
		cleaned := make([]string, 0, len(lines))
		for _, l := range lines {
			l = strings.TrimSpace(l)
			l = strings.TrimPrefix(l, "*")
			l = strings.TrimSpace(l)
			if l != "" {
				cleaned = append(cleaned, l)
			}
		}
		result[m[1]] = strings.Join(cleaned, " ")
	}
	return result
}

// nearestJSDoc finds the JSDoc comment whose closing offset is closest to
// (and before) declOffset, within a 5-line window (~400 bytes).
func nearestJSDoc(docs map[int]string, declOffset int) string {
	best := ""
	bestDist := 1<<31 - 1
	for endOff, text := range docs {
		dist := declOffset - endOff
		if dist >= 0 && dist < 400 && dist < bestDist {
			bestDist = dist
			best = text
		}
	}
	return best
}

// splitJSNames splits "A, B as C, D" style lists into individual name tokens.
func splitJSNames(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Handle "name as alias" → keep just the original name
		if idx := strings.Index(p, " as "); idx >= 0 {
			p = strings.TrimSpace(p[:idx])
		}
		result = append(result, p)
	}
	return result
}

// classRange holds the byte range of a class declaration.
type classRange struct {
	start int
	end   int // estimated — we use a heuristic closing brace search
	name  string
}

// buildJSClassRanges finds all class start positions.  We don't track real
// brace matching here; instead we record start offsets and sort them so that
// findParentClass can pick the most-recent enclosing class.
func buildJSClassRanges(content string, re *regexp.Regexp) []classRange {
	var ranges []classRange
	for _, m := range re.FindAllStringSubmatchIndex(content, -1) {
		name := ""
		if m[6] >= 0 {
			name = content[m[6]:m[7]]
		}
		ranges = append(ranges, classRange{start: m[0], end: len(content), name: name})
	}
	return ranges
}

// findParentClass returns the name of the innermost class whose start offset
// precedes methodOffset.
func findParentClass(ranges []classRange, methodOffset int) string {
	parent := ""
	for _, cr := range ranges {
		if cr.start < methodOffset {
			parent = cr.name
		}
	}
	return parent
}

// buildJSFuncPrefix assembles the keyword prefix for a function declaration.
func buildJSFuncPrefix(exported, async, generator bool) string {
	var b strings.Builder
	if exported {
		b.WriteString("export ")
	}
	if async {
		b.WriteString("async ")
	}
	if generator {
		b.WriteString("* ")
	}
	return b.String()
}

// buildJSMethodPrefix assembles the keyword prefix for a class method.
func buildJSMethodPrefix(static, async, generator bool, accessor string) string {
	var b strings.Builder
	if static {
		b.WriteString("static ")
	}
	if async {
		b.WriteString("async ")
	}
	if generator {
		b.WriteString("* ")
	}
	if accessor != "" {
		b.WriteString(accessor)
		b.WriteByte(' ')
	}
	return b.String()
}
