package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── C++ regex patterns ───────────────────────────────────────────────────────

var (
	// #include <system> or #include "local"
	reCPPInclude = regexp.MustCompile(
		`(?m)^[ \t]*#include\s+([<"][^>"]+[>"])`,
	)

	// Namespace declaration: namespace Name {
	reCPPNamespace = regexp.MustCompile(
		`(?m)^([ \t]*)(?:inline\s+)?namespace\s+(\w+)\s*\{`,
	)

	// Class/struct declaration with optional inheritance:
	//   [template<...>] [class|struct] Name [: [public|private|protected] Base, ...] {
	reCPPClass = regexp.MustCompile(
		`(?m)^([ \t]*)(?:template\s*<[^>]*>\s*)?(class|struct)\s+(\w+)` +
			`(?:\s*:\s*((?:(?:public|private|protected)\s+[\w:<>]+(?:\s*,\s*)?)+))?\s*\{`,
	)

	// Template declaration alone (for functions)
	reCPPTemplate = regexp.MustCompile(
		`(?m)^([ \t]*)template\s*<([^>]*)>`,
	)

	// Enum declaration (scoped and unscoped):
	//   enum [class|struct] Name [: Type] {
	reCPPEnum = regexp.MustCompile(
		`(?m)^([ \t]*)enum\s+(class\s+|struct\s+)?(\w+)(?:\s*:\s*(\w+))?\s*\{`,
	)

	// Typedef / using alias:
	//   typedef OldType NewName;
	//   using NewName = OldType;
	reCPPTypedef = regexp.MustCompile(
		`(?m)^([ \t]*)typedef\s+([\w\s\*\<\>\[\],]+)\s+(\w+)\s*;`,
	)
	reCPPUsing = regexp.MustCompile(
		`(?m)^([ \t]*)using\s+(\w+)\s*=\s*([\w\s\*\<\>\[\],\:]+)\s*;`,
	)

	// using namespace X;
	reCPPUsingNamespace = regexp.MustCompile(
		`(?m)^[ \t]*using\s+namespace\s+([\w:]+)\s*;`,
	)

	// Function / method declaration or definition:
	//   [virtual] [static] [inline] [constexpr] ReturnType [ClassName::]Name(params) [const] [override] [final] [noexcept] [{|;]
	// We use a permissive pattern and filter out obvious non-matches.
	reCPPFunc = regexp.MustCompile(
		`(?m)^([ \t]*)(virtual\s+|static\s+|inline\s+|constexpr\s+|explicit\s+|friend\s+)*` +
			`((?:const\s+)?(?:unsigned\s+)?(?:long\s+)*[\w:<>\*&\[\]]+(?:\s*[\*&]+)?)\s+` +
			`((?:[\w]+::)*~?(?:operator[^\(]+|\w+))\s*` +
			`(<[^>]*>)?\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`((?:\s*const)?(?:\s*noexcept(?:\([^)]*\))?)?(?:\s*override)?(?:\s*final)?(?:\s*=\s*(?:0|default|delete))?)` +
			`\s*(?:\{|;)`,
	)

	// Constructor / destructor (no return type):
	//   ClassName::ClassName(params) or ~ClassName(params)
	reCPPConstructor = regexp.MustCompile(
		`(?m)^([ \t]*)(explicit\s+)?(~?[\w]+)\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s*:\s*[^\{]+)?\s*\{`,
	)

	// Forward declaration: class Name; or struct Name;
	reCPPForward = regexp.MustCompile(
		`(?m)^([ \t]*)(class|struct)\s+(\w+)\s*;`,
	)

	// Doxygen comment: /** ... */ or /// ...
	reCPPDoxyBlock = regexp.MustCompile(`(?s)/\*\*.*?\*/`)
	reCPPDoxyLine  = regexp.MustCompile(`(?m)((?:[ \t]*///[^\n]*\n)+)`)

	// Preprocessor #define (constants and macros)
	reCPPDefine = regexp.MustCompile(
		`(?m)^[ \t]*#define\s+([A-Z][A-Z0-9_]*)\s+(.+)`,
	)
)

// analyzeCPP performs deep static analysis of a C++ source file.
func analyzeCPP(filePath, content string) *FileAnalysis {
	fa := &FileAnalysis{
		FilePath:  filePath,
		Language:  "C++",
		LineCount: countLines(content),
	}

	// ── Doc comment index ─────────────────────────────────────────────────────
	doxyByEnd := buildCPPDoxyIndex(content)

	// ── #include ──────────────────────────────────────────────────────────────
	for _, m := range reCPPInclude.FindAllStringSubmatchIndex(content, -1) {
		raw := content[m[2]:m[3]]
		isSystem := strings.HasPrefix(raw, "<")
		mod := strings.Trim(raw, "<>\"")
		alias := "local"
		if isSystem {
			alias = "system"
		}
		fa.Imports = append(fa.Imports, Import{
			Module: mod,
			Alias:  alias,
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── using namespace ───────────────────────────────────────────────────────
	for _, m := range reCPPUsingNamespace.FindAllStringSubmatchIndex(content, -1) {
		ns := content[m[2]:m[3]]
		fa.Imports = append(fa.Imports, Import{
			Module: ns,
			Alias:  "namespace",
			Line:   lineNumber(content, m[0]),
		})
	}

	// ── Namespaces ────────────────────────────────────────────────────────────
	for _, m := range reCPPNamespace.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[4]:m[5]]
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindNamespace,
			Name:      name,
			Signature: "namespace " + name,
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Classes & Structs ─────────────────────────────────────────────────────
	// reCPPClass groups: 1=indent  2=keyword(class|struct)  3=name  4=bases
	for _, m := range reCPPClass.FindAllStringSubmatchIndex(content, -1) {
		keyword := extractGroup(content, m, 2) // "class" or "struct"
		name := extractGroup(content, m, 3)
		basesRaw := strings.TrimSpace(extractGroup(content, m, 4))

		var bases []string
		var extends string
		if basesRaw != "" {
			// Strip access specifiers
			cleanBases := regexp.MustCompile(`\b(?:public|private|protected)\b`).ReplaceAllString(basesRaw, "")
			for _, b := range strings.Split(cleanBases, ",") {
				b = strings.TrimSpace(b)
				if b != "" {
					bases = append(bases, b)
				}
			}
			if len(bases) > 0 {
				extends = bases[0]
			}
		}

		kind := KindClass
		if keyword == "struct" {
			kind = KindStruct
		}

		sig := keyword + " " + name
		if extends != "" {
			sig += " : " + basesRaw
		}

		ln := lineNumber(content, m[0])
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       kind,
			Name:       name,
			Signature:  sig,
			Extends:    extends,
			Implements: bases,
			Exported:   true,
			Line:       ln,
			DocComment: nearestCPPDoxy(doxyByEnd, m[0]),
		})
	}

	// ── Enums ─────────────────────────────────────────────────────────────────
	// reCPPEnum groups: 1=indent  2=scoped-marker  3=name  4=underlying
	for _, m := range reCPPEnum.FindAllStringSubmatchIndex(content, -1) {
		scoped := extractGroup(content, m, 2) != ""
		name := extractGroup(content, m, 3)
		underlying := extractGroup(content, m, 4)
		sig := "enum "
		if scoped {
			sig += "class "
		}
		sig += name
		if underlying != "" {
			sig += " : " + underlying
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindEnum,
			Name:      name,
			Signature: sig,
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Typedefs ──────────────────────────────────────────────────────────────
	// reCPPTypedef groups: 1=indent  2=underlying  3=name
	for _, m := range reCPPTypedef.FindAllStringSubmatchIndex(content, -1) {
		underlying := strings.TrimSpace(extractGroup(content, m, 2))
		name := extractGroup(content, m, 3)
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindType,
			Name:       name,
			Signature:  "typedef " + underlying + " " + name,
			ReturnType: underlying,
			Exported:   true,
			Line:       lineNumber(content, m[0]),
		})
	}

	// ── using aliases ─────────────────────────────────────────────────────────
	// reCPPUsing groups: 1=indent  2=name  3=underlying
	for _, m := range reCPPUsing.FindAllStringSubmatchIndex(content, -1) {
		name := extractGroup(content, m, 2)
		underlying := strings.TrimSpace(extractGroup(content, m, 3))
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindType,
			Name:       name,
			Signature:  "using " + name + " = " + underlying,
			ReturnType: underlying,
			Exported:   true,
			Line:       lineNumber(content, m[0]),
		})
	}

	// ── #define constants (UPPER_CASE) ────────────────────────────────────────
	for _, m := range reCPPDefine.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		val := strings.TrimSpace(content[m[4]:m[5]])
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindConstant,
			Name:      name,
			Signature: "#define " + name + " " + val,
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Forward declarations ──────────────────────────────────────────────────
	// reCPPForward groups: 1=indent  2=keyword  3=name
	for _, m := range reCPPForward.FindAllStringSubmatchIndex(content, -1) {
		keyword := extractGroup(content, m, 2)
		name := extractGroup(content, m, 3)
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindType,
			Name:      name,
			Signature: keyword + " " + name + "; // forward declaration",
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Functions & Methods ───────────────────────────────────────────────────
	// Build class ranges for parent assignment.
	type cppClassEntry struct {
		name string
		line int
	}
	var classEntries []cppClassEntry
	for _, m := range reCPPClass.FindAllStringSubmatchIndex(content, -1) {
		classEntries = append(classEntries, cppClassEntry{
			name: extractGroup(content, m, 3),
			line: lineNumber(content, m[0]),
		})
	}

	skipWords := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true,
		"catch": true, "return": true, "delete": true, "new": true,
		"sizeof": true, "alignof": true, "decltype": true,
		"void": true, "int": true, "bool": true, "char": true,
	}

	// reCPPFunc groups: 1=indent  2=modifiers  3=returnType  4=name  5=generics  6=params  7=qualifiers
	for _, m := range reCPPFunc.FindAllStringSubmatchIndex(content, -1) {
		modifiers := strings.TrimSpace(extractGroup(content, m, 2))
		returnType := strings.TrimSpace(extractGroup(content, m, 3))
		nameWithScope := extractGroup(content, m, 4)
		params := strings.TrimSpace(extractGroup(content, m, 6))
		qualifiers := strings.TrimSpace(extractGroup(content, m, 7))

		if nameWithScope == "" || returnType == "" {
			continue
		}
		if skipWords[nameWithScope] || skipWords[returnType] {
			continue
		}
		// Extract simple name and optional class scope
		parent := ""
		name := nameWithScope
		if idx := strings.LastIndex(nameWithScope, "::"); idx >= 0 {
			parent = strings.TrimSuffix(nameWithScope[:idx], "::")
			name = nameWithScope[idx+2:]
		}

		// Guess parent from class entries if not scoped
		if parent == "" {
			ln := lineNumber(content, m[0])
			for _, ce := range classEntries {
				if ce.line < ln {
					parent = ce.name
				}
			}
		}

		kind := KindFunction
		if parent != "" {
			kind = KindMethod
		}

		sig := buildCPPFuncSig(modifiers, returnType, nameWithScope, params, qualifiers)

		ln := lineNumber(content, m[0])
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       kind,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Parent:     parent,
			Exported:   true,
			Line:       ln,
			DocComment: nearestCPPDoxy(doxyByEnd, m[0]),
		})
	}

	return fa
}

// buildCPPDoxyIndex maps doxygen comment end offsets to cleaned text.
func buildCPPDoxyIndex(content string) map[int]string {
	result := map[int]string{}
	// Block doxygen
	for _, m := range reCPPDoxyBlock.FindAllStringIndex(content, -1) {
		raw := content[m[0]:m[1]]
		raw = strings.TrimPrefix(raw, "/**")
		raw = strings.TrimSuffix(raw, "*/")
		lns := strings.Split(raw, "\n")
		cleaned := make([]string, 0, len(lns))
		for _, l := range lns {
			l = strings.TrimSpace(l)
			l = strings.TrimPrefix(l, "*")
			l = strings.TrimSpace(l)
			// Strip @param / @return tags for summary
			if strings.HasPrefix(l, "@") {
				continue
			}
			if l != "" {
				cleaned = append(cleaned, l)
			}
		}
		result[m[1]] = strings.Join(cleaned, " ")
	}
	// Line doxygen
	for _, m := range reCPPDoxyLine.FindAllStringSubmatchIndex(content, -1) {
		raw := content[m[2]:m[3]]
		lns := strings.Split(raw, "\n")
		cleaned := make([]string, 0, len(lns))
		for _, l := range lns {
			l = strings.TrimSpace(l)
			l = strings.TrimPrefix(l, "///")
			l = strings.TrimSpace(l)
			if l != "" {
				cleaned = append(cleaned, l)
			}
		}
		result[m[3]] = strings.Join(cleaned, " ")
	}
	return result
}

// nearestCPPDoxy finds the closest doxygen comment for a declaration.
func nearestCPPDoxy(docs map[int]string, declOffset int) string {
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

// buildCPPFuncSig assembles a readable C++ function signature.
func buildCPPFuncSig(modifiers, returnType, name, params, qualifiers string) string {
	var b strings.Builder
	if modifiers != "" {
		b.WriteString(modifiers)
		b.WriteByte(' ')
	}
	b.WriteString(returnType)
	b.WriteByte(' ')
	b.WriteString(name)
	b.WriteByte('(')
	b.WriteString(params)
	b.WriteByte(')')
	if qualifiers != "" {
		b.WriteByte(' ')
		b.WriteString(strings.TrimSpace(qualifiers))
	}
	return b.String()
}
