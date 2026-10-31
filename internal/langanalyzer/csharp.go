package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── C# regex patterns ────────────────────────────────────────────────────────

var (
	// Namespace declaration
	reCSharpNamespace = regexp.MustCompile(
		`(?m)^([ \t]*)(?:namespace)\s+([\w\.]+)\s*(?:\{|;)`,
	)

	// Using directive: using [static] [alias =] Name.Space;
	reCSharpUsing = regexp.MustCompile(
		`(?m)^[ \t]*using\s+(static\s+)?(?:(\w+)\s*=\s*)?([\w\.\<\>]+)\s*;`,
	)

	// Attribute (decorator): [HttpGet], [Authorize("policy")], etc.
	reCSharpAttribute = regexp.MustCompile(
		`(?m)^([ \t]*)\[([^\[\]]+)\]\s*$`,
	)

	// Class: [access] [static|abstract|sealed|partial] class Name[<T>] [: Base, IFace] {
	reCSharpClass = regexp.MustCompile(
		`(?m)^([ \t]*)(public|internal|protected|private)?\s*` +
			`(static\s+|abstract\s+|sealed\s+|partial\s+)*` +
			`class\s+(\w+)(?:<[^>]*>)?` +
			`(?:\s*:\s*([\w\s,\.\<\>]+?))?\s*(?:where\s+[^{]+)?\s*\{`,
	)

	// Interface: [access] interface IName[<T>] [: IBase] {
	reCSharpInterface = regexp.MustCompile(
		`(?m)^([ \t]*)(public|internal|protected|private)?\s*(partial\s+)?` +
			`interface\s+(\w+)(?:<[^>]*>)?` +
			`(?:\s*:\s*([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// Struct: [access] struct Name[<T>] [: IFace] {
	reCSharpStruct = regexp.MustCompile(
		`(?m)^([ \t]*)(public|internal|protected|private)?\s*(readonly\s+)?(partial\s+)?` +
			`struct\s+(\w+)(?:<[^>]*>)?` +
			`(?:\s*:\s*([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// Enum: [access] enum Name [: underlyingType] {
	reCSharpEnum = regexp.MustCompile(
		`(?m)^([ \t]*)(public|internal|protected|private)?\s*enum\s+(\w+)` +
			`(?:\s*:\s*(\w+))?\s*\{`,
	)

	// Method / constructor with full modifiers and return type:
	//   [access] [static|virtual|abstract|override|async|extern|new] ReturnType Name[<T>](params) [where ...]
	reCSharpMethod = regexp.MustCompile(
		`(?m)^([ \t]+)(public|internal|protected|private)?\s*` +
			`(static\s+|virtual\s+|abstract\s+|override\s+|sealed\s+|async\s+|extern\s+|new\s+|readonly\s+)*` +
			`([\w\<\>\[\]\,\?\s\.]+?)\s+(\w+)\s*` +
			`(?:<[^>]*>)?\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s*where\s+[^{]*)?\s*(?:\{|=>|;)`,
	)

	// Property: [access] [static|virtual|abstract|override] Type Name { [get;] [set;] }
	reCSharpProperty = regexp.MustCompile(
		`(?m)^([ \t]+)(public|internal|protected|private)?\s*` +
			`(static\s+|virtual\s+|abstract\s+|override\s+|readonly\s+)*` +
			`([\w\<\>\[\]\,\?\s\.]+?)\s+(\w+)\s*` +
			`\{[^}]*(?:get|set)[^}]*\}`,
	)

	// Event declaration
	reCSharpEvent = regexp.MustCompile(
		`(?m)^([ \t]+)(public|internal|protected|private)?\s*(static\s+)?` +
			`event\s+([\w\<\>]+)\s+(\w+)\s*;`,
	)

	// Delegate declaration
	reCSharpDelegate = regexp.MustCompile(
		`(?m)^([ \t]*)(public|internal|protected|private)?\s*delegate\s+` +
			`([\w\<\>\[\]\?]+)\s+(\w+)\s*(?:<[^>]*>)?\s*\(([^)]*)\)\s*;`,
	)

	// XML doc comment: /// <summary>...</summary>
	reCSharpXMLDoc = regexp.MustCompile(`(?m)((?:[ \t]*///[^\n]*\n)+)`)
)

// analyzeCSharp performs deep static analysis of a C# source file.
func analyzeCSharp(filePath, content string) *FileAnalysis {
	fa := &FileAnalysis{
		FilePath:  filePath,
		Language:  "C#",
		LineCount: countLines(content),
	}

	// ── XML doc index ─────────────────────────────────────────────────────────
	xmlDocByEnd := buildCSharpDocIndex(content)

	// ── Attribute (decorator) index ───────────────────────────────────────────
	attrByLine := buildCSharpAttributeIndex(content)

	// ── Namespace ─────────────────────────────────────────────────────────────
	if m := reCSharpNamespace.FindStringSubmatchIndex(content); m != nil {
		fa.Package = content[m[4]:m[5]]
	}
	for _, m := range reCSharpNamespace.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[4]:m[5]]
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindNamespace,
			Name:      name,
			Signature: "namespace " + name,
			Exported:  true,
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Using directives ──────────────────────────────────────────────────────
	for _, m := range reCSharpUsing.FindAllStringSubmatchIndex(content, -1) {
		isStatic := m[2] >= 0 && content[m[2]:m[3]] != ""
		alias := ""
		if m[4] >= 0 {
			alias = content[m[4]:m[5]]
		}
		ns := content[m[6]:m[7]]
		imp := Import{
			Module: ns,
			Alias:  alias,
			Line:   lineNumber(content, m[0]),
		}
		if isStatic {
			imp.Alias = "static " + imp.Alias
		}
		fa.Imports = append(fa.Imports, imp)
	}

	// ── Classes ───────────────────────────────────────────────────────────────
	// reCSharpClass groups: 1=indent  2=access  3=modifiers  4=name  5=bases
	for _, m := range reCSharpClass.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		modifiers := strings.TrimSpace(extractGroup(content, m, 3))
		name := extractGroup(content, m, 4)
		basesRaw := strings.TrimSpace(extractGroup(content, m, 5))

		var extends string
		var implements []string
		if basesRaw != "" {
			parts := strings.Split(basesRaw, ",")
			for i, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				if i == 0 && !strings.HasPrefix(p, "I") {
					// Heuristic: first entry that doesn't start with I is base class
					extends = p
				} else {
					implements = append(implements, p)
				}
			}
		}

		ln := lineNumber(content, m[0])
		sig := buildCSharpTypeSig(access, modifiers, "class", name, basesRaw)
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindClass,
			Name:       name,
			Signature:  sig,
			Extends:    extends,
			Implements: implements,
			Exported:   access == "public",
			Decorators: attrByLine[ln-1],
			Line:       ln,
			DocComment: nearestCSharpDoc(xmlDocByEnd, m[0]),
		})
	}

	// ── Interfaces ────────────────────────────────────────────────────────────
	// reCSharpInterface groups: 1=indent  2=access  3=partial  4=name  5=extends
	for _, m := range reCSharpInterface.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		name := extractGroup(content, m, 4)
		extendsRaw := strings.TrimSpace(extractGroup(content, m, 5))
		ln := lineNumber(content, m[0])
		sig := buildCSharpTypeSig(access, "", "interface", name, extendsRaw)
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindInterface,
			Name:       name,
			Signature:  sig,
			Exported:   access == "public",
			Decorators: attrByLine[ln-1],
			Line:       ln,
			DocComment: nearestCSharpDoc(xmlDocByEnd, m[0]),
		})
	}

	// ── Structs ───────────────────────────────────────────────────────────────
	// reCSharpStruct groups: 1=indent  2=access  3=readonly  4=partial  5=name  6=bases
	for _, m := range reCSharpStruct.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		name := extractGroup(content, m, 5)
		ln := lineNumber(content, m[0])
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindStruct,
			Name:       name,
			Signature:  buildCSharpTypeSig(access, "", "struct", name, ""),
			Exported:   access == "public",
			Decorators: attrByLine[ln-1],
			Line:       ln,
			DocComment: nearestCSharpDoc(xmlDocByEnd, m[0]),
		})
	}

	// ── Enums ─────────────────────────────────────────────────────────────────
	// reCSharpEnum groups: 1=indent  2=access  3=name  4=underlying
	for _, m := range reCSharpEnum.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		name := extractGroup(content, m, 3)
		underlying := extractGroup(content, m, 4)
		sig := access + " enum " + name
		if underlying != "" {
			sig += " : " + underlying
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindEnum,
			Name:      name,
			Signature: sig,
			Exported:  access == "public",
			Line:      lineNumber(content, m[0]),
		})
	}

	// ── Delegates ─────────────────────────────────────────────────────────────
	// reCSharpDelegate groups: 1=indent  2=access  3=returnType  4=name  5=params
	for _, m := range reCSharpDelegate.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		returnType := extractGroup(content, m, 3)
		name := extractGroup(content, m, 4)
		params := strings.TrimSpace(extractGroup(content, m, 5))
		sig := access + " delegate " + returnType + " " + name + "(" + params + ")"
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindType,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Exported:   access == "public",
			Line:       lineNumber(content, m[0]),
		})
	}

	// ── Events ────────────────────────────────────────────────────────────────
	// reCSharpEvent groups: 1=indent  2=access  3=static  4=type  5=name
	for _, m := range reCSharpEvent.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		typ := extractGroup(content, m, 4)
		name := extractGroup(content, m, 5)
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindProperty,
			Name:       name,
			Signature:  access + " event " + typ + " " + name,
			ReturnType: typ,
			Exported:   access == "public",
			Line:       lineNumber(content, m[0]),
		})
	}

	// ── Methods ───────────────────────────────────────────────────────────────
	// reCSharpMethod groups: 1=indent  2=access  3=modifiers  4=returnType  5=name  6=params
	for _, m := range reCSharpMethod.FindAllStringSubmatchIndex(content, -1) {
		indent := extractGroup(content, m, 1)
		if indent == "" {
			continue
		}
		access := extractGroup(content, m, 2)
		modifiers := strings.TrimSpace(extractGroup(content, m, 3))
		returnType := strings.TrimSpace(extractGroup(content, m, 4))
		name := extractGroup(content, m, 5)
		params := strings.TrimSpace(extractGroup(content, m, 6))

		if name == "" || returnType == "" {
			continue
		}
		// Avoid false positives on control flow keywords
		skip := map[string]bool{
			"if": true, "for": true, "while": true, "foreach": true,
			"switch": true, "catch": true, "using": true, "lock": true,
		}
		if skip[name] || skip[returnType] {
			continue
		}

		ln := lineNumber(content, m[0])
		sig := buildCSharpMethodSig(access, modifiers, returnType, name, params)

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindMethod,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Exported:   access == "public",
			Decorators: attrByLine[ln-1],
			Line:       ln,
			DocComment: nearestCSharpDoc(xmlDocByEnd, m[0]),
		})
	}

	// ── Properties ────────────────────────────────────────────────────────────
	// reCSharpProperty groups: 1=indent  2=access  3=modifiers  4=type  5=name
	for _, m := range reCSharpProperty.FindAllStringSubmatchIndex(content, -1) {
		indent := extractGroup(content, m, 1)
		if indent == "" {
			continue
		}
		access := extractGroup(content, m, 2)
		modifiers := strings.TrimSpace(extractGroup(content, m, 3))
		typ := strings.TrimSpace(extractGroup(content, m, 4))
		name := extractGroup(content, m, 5)
		if name == "" || typ == "" {
			continue
		}
		sig := buildCSharpMethodSig(access, modifiers, typ, name, "")
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindProperty,
			Name:       name,
			Signature:  sig + " { get; set; }",
			ReturnType: typ,
			Exported:   access == "public",
			Line:       lineNumber(content, m[0]),
		})
	}

	return fa
}

// buildCSharpDocIndex maps XML doc comment end offsets to their summary text.
func buildCSharpDocIndex(content string) map[int]string {
	result := map[int]string{}
	for _, m := range reCSharpXMLDoc.FindAllStringSubmatchIndex(content, -1) {
		raw := content[m[2]:m[3]]
		lns := strings.Split(raw, "\n")
		cleaned := make([]string, 0, len(lns))
		for _, l := range lns {
			l = strings.TrimSpace(l)
			l = strings.TrimPrefix(l, "///")
			l = strings.TrimSpace(l)
			// Strip XML tags
			l = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(l, "")
			if l != "" {
				cleaned = append(cleaned, l)
			}
		}
		result[m[3]] = strings.Join(cleaned, " ")
	}
	return result
}

// nearestCSharpDoc finds the closest XML doc comment for a declaration.
func nearestCSharpDoc(docs map[int]string, declOffset int) string {
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

// buildCSharpAttributeIndex maps the line of a decorated declaration to its attributes.
func buildCSharpAttributeIndex(content string) map[int][]string {
	result := map[int][]string{}
	for _, m := range reCSharpAttribute.FindAllStringSubmatchIndex(content, -1) {
		attr := "[" + content[m[4]:m[5]] + "]"
		ln := lineNumber(content, m[0])
		result[ln+1] = append(result[ln+1], attr)
	}
	return result
}

// buildCSharpTypeSig assembles a C# type declaration signature.
func buildCSharpTypeSig(access, modifiers, kind, name, bases string) string {
	var b strings.Builder
	if access != "" {
		b.WriteString(access)
		b.WriteByte(' ')
	}
	if modifiers != "" {
		b.WriteString(modifiers)
		b.WriteByte(' ')
	}
	b.WriteString(kind)
	b.WriteByte(' ')
	b.WriteString(name)
	if bases != "" {
		b.WriteString(" : ")
		b.WriteString(bases)
	}
	return b.String()
}

// buildCSharpMethodSig assembles a C# method signature.
func buildCSharpMethodSig(access, modifiers, returnType, name, params string) string {
	var b strings.Builder
	if access != "" {
		b.WriteString(access)
		b.WriteByte(' ')
	}
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
	return b.String()
}
