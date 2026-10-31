package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── Python regex patterns ────────────────────────────────────────────────────

var (
	// Function definition: [async] def name(params[: type]) [-> ReturnType]:
	rePyFunc = regexp.MustCompile(
		`(?m)^([ \t]*)(async\s+)?def\s+(\w+)\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s*->\s*([\w\[\]|\s,\.\?\*\'"]+?))?\s*:`,
	)

	// Class definition: class Name[(Base, ...)]:
	rePyClass = regexp.MustCompile(
		`(?m)^([ \t]*)class\s+(\w+)(?:\s*\(([^)]*)\))?\s*:`,
	)

	// Decorator: @something or @something(args)
	rePyDecorator = regexp.MustCompile(
		`(?m)^([ \t]*)(@[\w\.]+(?:\([^)]*\)?)?)\s*$`,
	)

	// import X
	rePyImport = regexp.MustCompile(
		`(?m)^([ \t]*)import\s+([\w\.,\s]+)`,
	)

	// from X import Y [as Z], ...
	rePyFromImport = regexp.MustCompile(
		`(?m)^([ \t]*)from\s+([\w\.]+)\s+import\s+((?:\*|\([\w\s,\n]+\)|[\w\s,]+))`,
	)

	// __all__ = [...]
	rePyAll = regexp.MustCompile(
		`(?m)^__all__\s*=\s*\[([^\]]*)\]`,
	)

	// Module-level UPPER_CASE constants: NAME = value  (at col 0)
	rePyConstant = regexp.MustCompile(
		`(?m)^([A-Z][A-Z0-9_]{2,})\s*(?::\s*[\w\[\]]+\s*)?=\s*`,
	)

	// Docstring immediately after def/class (triple-quote): """ ... """ or ''' ... '''
	rePyDocstring = regexp.MustCompile(
		`(?s)(?:"""(.*?)"""|'''(.*?)''')`,
	)
)

// analyzePython performs deep static analysis of a Python source file.
func analyzePython(filePath, content string) *FileAnalysis {
	fa := &FileAnalysis{
		FilePath:  filePath,
		Language:  "Python",
		LineCount: countLines(content),
	}

	lines := strings.Split(content, "\n")

	// ── Docstring index: map from line number → cleaned docstring ────────────
	docByLine := buildPyDocstringIndex(content)

	// ── Decorator index: map from decorated-declaration line → decorators ────
	decoratorsByLine := buildPyDecoratorIndex(content)

	// ── Module name (file stem used as package) ───────────────────────────────
	// Python has no package keyword; the module name is the file stem.

	// ── __all__ exports ───────────────────────────────────────────────────────
	for _, m := range rePyAll.FindAllStringSubmatchIndex(content, -1) {
		exports := splitPyList(content[m[2]:m[3]])
		for _, name := range exports {
			fa.Symbols = append(fa.Symbols, Symbol{
				Kind:      KindExport,
				Name:      name,
				Signature: "__all__ member: " + name,
				Exported:  true,
				Line:      lineNumber(content, m[0]),
			})
		}
	}

	// ── Module-level constants ────────────────────────────────────────────────
	for _, m := range rePyConstant.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		ln := lineNumber(content, m[0])
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindConstant,
			Name:      name,
			Signature: strings.TrimSpace(lines[ln-1]),
			Exported:  true,
			Line:      ln,
		})
	}

	// ── import statements ─────────────────────────────────────────────────────
	for _, m := range rePyImport.FindAllStringSubmatchIndex(content, -1) {
		mods := content[m[4]:m[5]]
		for _, mod := range strings.Split(mods, ",") {
			mod = strings.TrimSpace(mod)
			if mod == "" {
				continue
			}
			// Handle "import X as Y"
			alias := ""
			if idx := strings.Index(mod, " as "); idx >= 0 {
				alias = strings.TrimSpace(mod[idx+4:])
				mod = strings.TrimSpace(mod[:idx])
			}
			fa.Imports = append(fa.Imports, Import{
				Module: mod,
				Alias:  alias,
				Line:   lineNumber(content, m[0]),
			})
		}
	}

	// ── from X import Y, Z ────────────────────────────────────────────────────
	for _, m := range rePyFromImport.FindAllStringSubmatchIndex(content, -1) {
		mod := content[m[4]:m[5]]
		namesRaw := strings.Trim(strings.TrimSpace(content[m[6]:m[7]]), "()")
		names := splitPyImportNames(namesRaw)
		fa.Imports = append(fa.Imports, Import{
			Module: mod,
			Names:  names,
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Class definitions ─────────────────────────────────────────────────────
	for _, m := range rePyClass.FindAllStringSubmatchIndex(content, -1) {
		indent := content[m[2]:m[3]]
		name := content[m[4]:m[5]]
		bases := []string{}
		if m[6] >= 0 {
			for _, b := range strings.Split(content[m[6]:m[7]], ",") {
				b = strings.TrimSpace(b)
				if b != "" && b != "object" {
					bases = append(bases, b)
				}
			}
		}
		ln := lineNumber(content, m[0])

		sig := "class " + name
		extends := ""
		if len(bases) > 0 {
			sig += "(" + strings.Join(bases, ", ") + ")"
			extends = bases[0]
		}
		sig += ":"

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindClass,
			Name:       name,
			Signature:  sig,
			Extends:    extends,
			Implements: bases,
			Exported:   indent == "" && !strings.HasPrefix(name, "_"),
			Decorators: decoratorsByLine[ln-1],
			Line:       ln,
			DocComment: docByLine[ln+1],
		})
	}

	// ── Function definitions ──────────────────────────────────────────────────
	// Build class range map so we can assign Parent.
	type pyClassEntry struct {
		indent string
		name   string
		line   int
	}
	var classStack []pyClassEntry
	for _, m := range rePyClass.FindAllStringSubmatchIndex(content, -1) {
		classStack = append(classStack, pyClassEntry{
			indent: content[m[2]:m[3]],
			name:   content[m[4]:m[5]],
			line:   lineNumber(content, m[0]),
		})
	}

	for _, m := range rePyFunc.FindAllStringSubmatchIndex(content, -1) {
		indent := content[m[2]:m[3]]
		async := m[4] >= 0 && content[m[4]:m[5]] != ""
		name := content[m[6]:m[7]]
		params := ""
		if m[8] >= 0 {
			params = strings.TrimSpace(content[m[8]:m[9]])
		}
		returnType := ""
		if m[10] >= 0 {
			returnType = strings.TrimSpace(content[m[10]:m[11]])
		}

		ln := lineNumber(content, m[0])
		dec := decoratorsByLine[ln-1]

		// Determine parent class and kind
		parent := ""
		kind := KindFunction
		for _, ce := range classStack {
			// A method is indented one level deeper than its class
			if ce.line < ln && len(indent) > len(ce.indent) {
				parent = ce.name
				kind = KindMethod
			}
		}

		// Infer special method sub-type from decorators
		for _, d := range dec {
			switch {
			case d == "@property":
				kind = KindProperty
			case d == "@staticmethod":
				kind = KindMethod
			case d == "@classmethod":
				kind = KindMethod
			}
		}

		sig := ""
		if async {
			sig = "async "
		}
		sig += "def " + name + "(" + params + ")"
		if returnType != "" {
			sig += " -> " + returnType
		}
		sig += ":"

		exported := !strings.HasPrefix(name, "_") || name == "__init__"

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       kind,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Parent:     parent,
			Exported:   exported,
			Decorators: dec,
			Line:       ln,
			DocComment: docByLine[ln+1],
		})
	}

	return fa
}

// buildPyDocstringIndex maps each line number after a def/class to its
// cleaned docstring (if the very next non-empty line is a triple-quote string).
func buildPyDocstringIndex(content string) map[int]string {
	result := map[int]string{}
	for _, m := range rePyDocstring.FindAllStringSubmatchIndex(content, -1) {
		raw := ""
		if m[2] >= 0 {
			raw = content[m[2]:m[3]]
		} else if m[4] >= 0 {
			raw = content[m[4]:m[5]]
		}
		if raw == "" {
			continue
		}
		// Clean: collapse whitespace
		lines := strings.Split(raw, "\n")
		cleaned := make([]string, 0, len(lines))
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" {
				cleaned = append(cleaned, l)
			}
		}
		ln := lineNumber(content, m[0])
		result[ln] = strings.Join(cleaned, " ")
	}
	return result
}

// buildPyDecoratorIndex returns a map from the line number of the decorated
// declaration to the list of its decorators.
func buildPyDecoratorIndex(content string) map[int][]string {
	result := map[int][]string{}
	matches := rePyDecorator.FindAllStringSubmatchIndex(content, -1)
	for i, m := range matches {
		dec := content[m[4]:m[5]]
		ln := lineNumber(content, m[0])
		// Walk forward through consecutive decorators to find the final declaration.
		target := ln + 1
		for j := i + 1; j < len(matches); j++ {
			nextLn := lineNumber(content, matches[j][0])
			if nextLn == target {
				target = nextLn + 1
			} else {
				break
			}
		}
		result[target] = append(result[target], dec)
	}
	return result
}

// splitPyList splits a Python list literal body like `"a", "b", 'c'`.
func splitPyList(s string) []string {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), `"'`)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// splitPyImportNames splits "A, B as C, D" into ["A", "B", "D"].
func splitPyImportNames(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimSuffix(p, "\n")
		if idx := strings.Index(p, " as "); idx >= 0 {
			p = strings.TrimSpace(p[:idx])
		}
		if p != "" && p != "*" {
			result = append(result, p)
		}
	}
	return result
}
