package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── TypeScript-specific regex patterns ───────────────────────────────────────
// All JS patterns apply plus the additions below.

var (
	// TypeScript interface: [export] interface Name [extends A, B] {
	reTSInterface = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+)?interface\s+(\w+)` +
			`(?:<[^>]*>)?` +
			`(?:\s+extends\s+([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// TypeScript type alias: [export] type Name[<T>] = ...
	reTSTypeAlias = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+)?type\s+(\w+)(?:<[^>]*>)?\s*=\s*(.+)`,
	)

	// TypeScript enum: [export] [const] enum Name {
	reTSEnum = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+)?(const\s+)?enum\s+(\w+)\s*\{`,
	)

	// TypeScript namespace / module: [export] namespace Name {
	reTSNamespace = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+)?(?:namespace|module)\s+(\w+)\s*\{`,
	)

	// Decorators: @Decorator or @decorator(args)
	reTSDecorator = regexp.MustCompile(
		`(?m)^[ \t]*(@[\w\.]+(?:\([^)]*\))?)\s*$`,
	)

	// TypeScript function with typed params & return:
	// [export] [async] function name<T>(p: T): ReturnType
	reTSFunction = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+(?:default\s+)?)?(async\s+)?function(\s*\*)?\s+(\w+)` +
			`(?:<[^>]*>)?\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s*:\s*([\w\<\>\[\]\|\&\s\.\?]+?))?(?:\s*\{|\s*;)`,
	)

	// Arrow const with TS types: const name = (p: T): R =>
	reTSArrowConst = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+)?(const|let|var)\s+(\w+)` +
			`(?:\s*:\s*[\w\<\>\[\]\|\&\s\.\?]+?)?\s*=\s*(async\s+)?` +
			`(?:(?:<[^>]*>)?\s*\(([^)]*)\)|(\w+))\s*(?::\s*[\w\<\>\[\]\|\&\?\s]+)?\s*=>`,
	)

	// Class method with access modifier and TS types:
	// [access] [static] [async] [abstract] [get|set] name<T>(p: T): R {
	reTSMethod = regexp.MustCompile(
		`(?m)^([ \t]+)(public|private|protected|override)?\s*(static\s+)?(async\s+)?(abstract\s+)?` +
			`(\*\s*)?(get\s+|set\s+)?(#?\w+)` +
			`(?:<[^>]*>)?\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s*:\s*([\w\<\>\[\]\|\&\?\s\.]+?))?\s*(?:\{|;)`,
	)

	// Class property declaration: [access] [static] [readonly] name[?]: Type [= ...]
	reTSProperty = regexp.MustCompile(
		`(?m)^([ \t]+)(public|private|protected)?\s*(static\s+)?(readonly\s+)?` +
			`(#?\w+)\s*(?:\?|!)?\s*:\s*([\w\<\>\[\]\|\&\?\s\.]+?)\s*(?:=|;|\n)`,
	)

	// TS class: [export] [abstract] class Name<T> [extends B] [implements I, J] {
	reTSClass = regexp.MustCompile(
		`(?m)^([ \t]*)(export\s+(?:default\s+)?)?(abstract\s+)?class\s+(\w+)` +
			`(?:<[^>]*>)?` +
			`(?:\s+extends\s+([\w\.\<\>]+))?` +
			`(?:\s+implements\s+([\w\s,\.\<\>]+?))?\s*\{`,
	)
)

// analyzeTypeScript performs deep static analysis of a TypeScript source file.
// It reuses the JS analyser then augments with TS-only constructs.
func analyzeTypeScript(filePath, content string) *FileAnalysis {
	// Start with the JS base analysis (imports, exports, basic classes, JSDoc).
	fa := analyzeJavaScript(filePath, content)
	fa.Language = "TypeScript"

	// Rebuild JSDoc index (already done in JS analyser but we need it here too).
	jsDocByEnd := buildJSDocIndex(content)

	// Build a decorator index: map from the line number of the decorated
	// declaration (approx) → list of decorator strings.
	decoratorsByLine := buildTSDecoratorIndex(content)

	// ── TS Interfaces ─────────────────────────────────────────────────────────
	for _, m := range reTSInterface.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		name := content[m[6]:m[7]]
		extends := ""
		if m[8] >= 0 {
			extends = strings.TrimSpace(content[m[8]:m[9]])
		}
		ln := lineNumber(content, m[0])
		sig := "interface " + name
		if extends != "" {
			sig += " extends " + extends
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindInterface,
			Name:       name,
			Signature:  sig,
			Extends:    extends,
			Exported:   exported,
			Decorators: decoratorsByLine[ln-1],
			Line:       ln,
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── TS Type aliases ───────────────────────────────────────────────────────
	for _, m := range reTSTypeAlias.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		name := content[m[6]:m[7]]
		rhs := ""
		if m[8] >= 0 {
			rhs = strings.TrimSpace(content[m[8]:m[9]])
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindType,
			Name:       name,
			Signature:  "type " + name + " = " + rhs,
			ReturnType: rhs,
			Exported:   exported,
			Line:       lineNumber(content, m[0]),
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── TS Enums ──────────────────────────────────────────────────────────────
	for _, m := range reTSEnum.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		isConst := m[6] >= 0 && content[m[6]:m[7]] != ""
		name := content[m[8]:m[9]]
		sig := ""
		if isConst {
			sig = "const "
		}
		sig += "enum " + name
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindEnum,
			Name:      name,
			Signature: sig,
			Exported:  exported,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── TS Namespaces ─────────────────────────────────────────────────────────
	for _, m := range reTSNamespace.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		name := content[m[6]:m[7]]
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindNamespace,
			Name:      name,
			Signature: "namespace " + name,
			Exported:  exported,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── TS Classes (override JS result with richer info) ──────────────────────
	// Remove JS-captured class symbols and re-capture with TS class regex.
	fa.Symbols = removeSameKind(fa.Symbols, KindClass)
	for _, m := range reTSClass.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		abstract := m[6] >= 0 && content[m[6]:m[7]] != ""
		name := content[m[8]:m[9]]
		extends := ""
		if m[10] >= 0 {
			extends = strings.TrimSpace(content[m[10]:m[11]])
		}
		var implements []string
		if m[12] >= 0 {
			implements = splitJSNames(content[m[12]:m[13]])
		}
		ln := lineNumber(content, m[0])
		sig := ""
		if abstract {
			sig = "abstract "
		}
		sig += "class " + name
		if extends != "" {
			sig += " extends " + extends
		}
		if len(implements) > 0 {
			sig += " implements " + strings.Join(implements, ", ")
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindClass,
			Name:       name,
			Signature:  sig,
			Extends:    extends,
			Implements: implements,
			Exported:   exported,
			Decorators: decoratorsByLine[ln-1],
			Line:       ln,
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── TS Functions (typed, override JS) ─────────────────────────────────────
	fa.Symbols = removeSameKind(fa.Symbols, KindFunction)
	for _, m := range reTSFunction.FindAllStringSubmatchIndex(content, -1) {
		exported := m[4] >= 0 && content[m[4]:m[5]] != ""
		async := m[6] >= 0 && content[m[6]:m[7]] != ""
		generator := m[8] >= 0 && content[m[8]:m[9]] != ""
		name := content[m[10]:m[11]]
		params := ""
		if m[12] >= 0 {
			params = strings.TrimSpace(content[m[12]:m[13]])
		}
		returnType := ""
		if m[14] >= 0 {
			returnType = strings.TrimSpace(content[m[14]:m[15]])
		}
		ln := lineNumber(content, m[0])
		prefix := buildJSFuncPrefix(exported, async, generator)
		sig := prefix + "function " + name + "(" + params + ")"
		if returnType != "" {
			sig += ": " + returnType
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindFunction,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Exported:   exported,
			Decorators: decoratorsByLine[ln-1],
			Line:       ln,
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── TS Methods (typed, override JS) ───────────────────────────────────────
	// reTSMethod groups: 1=indent  2=access  3=static  4=async  5=abstract  6=generator  7=accessor  8=name  9=params  10=returnType
	fa.Symbols = removeSameKind(fa.Symbols, KindMethod)
	classRanges := buildJSClassRanges(content, reTSClass)
	for _, m := range reTSMethod.FindAllStringSubmatchIndex(content, -1) {
		indent := content[m[2]:m[3]] // group 1
		if len(indent) == 0 {
			continue
		}
		access := extractGroup(content, m, 2)
		static := extractGroup(content, m, 3) != ""
		async := extractGroup(content, m, 4) != ""
		abstract := extractGroup(content, m, 5) != ""
		generator := extractGroup(content, m, 6) != ""
		accessor := strings.TrimSpace(extractGroup(content, m, 7))
		name := extractGroup(content, m, 8)
		if name == "" || name == "if" || name == "for" || name == "while" || name == "switch" {
			continue
		}
		params := extractGroup(content, m, 9)
		returnType := strings.TrimSpace(extractGroup(content, m, 10))
		parent := findParentClass(classRanges, m[0])
		ln := lineNumber(content, m[0])

		sig := buildTSMethodSig(access, static, async, abstract, generator, accessor, name, params, returnType)

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindMethod,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Parent:     parent,
			Exported:   access != "private",
			Decorators: decoratorsByLine[ln-1],
			Line:       ln,
			DocComment: nearestJSDoc(jsDocByEnd, m[0]),
		})
	}

	// ── TS Class Properties ───────────────────────────────────────────────────
	// reTSProperty groups: 1=indent  2=access  3=static  4=readonly  5=name  6=type
	for _, m := range reTSProperty.FindAllStringSubmatchIndex(content, -1) {
		indent := content[m[2]:m[3]] // group 1
		if len(indent) == 0 {
			continue
		}
		access := extractGroup(content, m, 2)
		static := extractGroup(content, m, 3) != ""
		readonly := extractGroup(content, m, 4) != ""
		name := extractGroup(content, m, 5)
		typ := strings.TrimSpace(extractGroup(content, m, 6))
		if name == "" {
			continue
		}
		parent := findParentClass(classRanges, m[0])
		sig := ""
		if access != "" {
			sig += access + " "
		}
		if static {
			sig += "static "
		}
		if readonly {
			sig += "readonly "
		}
		sig += name + ": " + typ
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindProperty,
			Name:       name,
			Signature:  sig,
			ReturnType: typ,
			Parent:     parent,
			Exported:   access != "private",
			Line:       lineNumber(content, m[0]),
		})
	}

	return fa
}

// buildTSDecoratorIndex returns a map from line number → list of decorators
// that appear on the preceding lines.
func buildTSDecoratorIndex(content string) map[int][]string {
	result := map[int][]string{}
	matches := reTSDecorator.FindAllStringSubmatchIndex(content, -1)
	for _, m := range matches {
		dec := content[m[2]:m[3]]
		ln := lineNumber(content, m[0])
		// Associate with the next line (where the declaration starts).
		result[ln+1] = append(result[ln+1], dec)
	}
	return result
}

// buildTSMethodSig assembles a readable TypeScript method signature.
func buildTSMethodSig(access string, static, async, abstract, generator bool, accessor, name, params, returnType string) string {
	var b strings.Builder
	if access != "" {
		b.WriteString(access)
		b.WriteByte(' ')
	}
	if static {
		b.WriteString("static ")
	}
	if async {
		b.WriteString("async ")
	}
	if abstract {
		b.WriteString("abstract ")
	}
	if generator {
		b.WriteString("* ")
	}
	if accessor != "" {
		b.WriteString(accessor)
		b.WriteByte(' ')
	}
	b.WriteString(name)
	b.WriteByte('(')
	b.WriteString(params)
	b.WriteByte(')')
	if returnType != "" {
		b.WriteString(": ")
		b.WriteString(returnType)
	}
	return b.String()
}

// removeSameKind filters out all symbols of the given kind.
func removeSameKind(syms []Symbol, kind SymbolKind) []Symbol {
	result := syms[:0]
	for _, s := range syms {
		if s.Kind != kind {
			result = append(result, s)
		}
	}
	return result
}

// extractGroup safely extracts a submatch group from an index slice.
func extractGroup(content string, m []int, groupIdx int) string {
	lo := groupIdx * 2
	hi := lo + 1
	if lo >= len(m) || m[lo] < 0 {
		return ""
	}
	return content[m[lo]:m[hi]]
}
