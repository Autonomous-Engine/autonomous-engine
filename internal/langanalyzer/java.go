package langanalyzer

import (
	"regexp"
	"strings"
)

// ─── Java regex patterns ──────────────────────────────────────────────────────

var (
	// Package declaration
	reJavaPackage = regexp.MustCompile(`(?m)^package\s+([\w\.]+)\s*;`)

	// Import statement: import [static] pkg.Name;
	reJavaImport = regexp.MustCompile(`(?m)^import\s+(static\s+)?([\w\.\*]+)\s*;`)

	// Annotation (decorator): @AnnotationName[(args)]
	reJavaAnnotation = regexp.MustCompile(`(?m)^([ \t]*)(@[\w\.]+(?:\s*\([^)]*\))?)\s*$`)

	// Class declaration (including abstract, final, inner):
	//   [access] [abstract|final|sealed] class Name[<T>] [extends B] [implements I,J] {
	reJavaClass = regexp.MustCompile(
		`(?m)^([ \t]*)(public|protected|private|package-private)?\s*` +
			`(abstract\s+|final\s+|sealed\s+|non-sealed\s+)?` +
			`(static\s+)?class\s+(\w+)(?:<[^>]*>)?` +
			`(?:\s+extends\s+([\w\.\<\>]+))?` +
			`(?:\s+implements\s+([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// Interface declaration
	reJavaInterface = regexp.MustCompile(
		`(?m)^([ \t]*)(public|protected|private)?\s*` +
			`(static\s+)?interface\s+(\w+)(?:<[^>]*>)?` +
			`(?:\s+extends\s+([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// Enum declaration
	reJavaEnum = regexp.MustCompile(
		`(?m)^([ \t]*)(public|protected|private)?\s*(static\s+)?enum\s+(\w+)` +
			`(?:\s+implements\s+([\w\s,\.\<\>]+?))?\s*\{`,
	)

	// Method declaration with full signature:
	//   [access] [static] [final] [abstract] [synchronized] [native] ReturnType name(params) [throws X]
	reJavaMethod = regexp.MustCompile(
		`(?m)^([ \t]+)(public|protected|private)?\s*` +
			`(static\s+)?(final\s+)?(abstract\s+)?(synchronized\s+)?(native\s+)?(default\s+)?` +
			`([\w\<\>\[\]\,\s\.]+?)\s+(\w+)\s*` +
			`(<[^>]*>)?\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s+throws\s+([\w\s,\.]+?))?\s*(?:\{|;)`,
	)

	// Constructor: [access] ClassName(params) [throws X] {
	reJavaConstructor = regexp.MustCompile(
		`(?m)^([ \t]+)(public|protected|private)?\s+(\w+)\s*\(([^)]*(?:\([^)]*\)[^)]*)*)\)` +
			`(?:\s+throws\s+([\w\s,\.]+?))?\s*\{`,
	)

	// Field declaration: [access] [static] [final] Type name [= ...];
	reJavaField = regexp.MustCompile(
		`(?m)^([ \t]+)(public|protected|private)\s+(static\s+)?(final\s+)?(transient\s+)?(volatile\s+)?` +
			`([\w\<\>\[\]\,\.]+)\s+(\w+)\s*(?:=|;)`,
	)

	// Javadoc comment
	reJavaDoc = regexp.MustCompile(`(?s)/\*\*.*?\*/`)
)

// analyzeJava performs deep static analysis of a Java source file.
func analyzeJava(filePath, content string) *FileAnalysis {
	fa := &FileAnalysis{
		FilePath:  filePath,
		Language:  "Java",
		LineCount: countLines(content),
	}

	// ── Package ───────────────────────────────────────────────────────────────
	if m := reJavaPackage.FindStringSubmatchIndex(content); m != nil {
		fa.Package = content[m[2]:m[3]]
	}

	// ── Javadoc index ─────────────────────────────────────────────────────────
	javadocByEnd := buildJavadocIndex(content)

	// ── Annotation (decorator) index ──────────────────────────────────────────
	annotByLine := buildJavaAnnotationIndex(content)

	// ── Imports ───────────────────────────────────────────────────────────────
	for _, m := range reJavaImport.FindAllStringSubmatchIndex(content, -1) {
		isStatic := m[2] >= 0 && content[m[2]:m[3]] != ""
		path := content[m[4]:m[5]]
		// Split pkg.Name into module=pkg and name=Name
		lastDot := strings.LastIndex(path, ".")
		mod := path
		name := ""
		if lastDot >= 0 {
			mod = path[:lastDot]
			name = path[lastDot+1:]
		}
		imp := Import{
			Module: mod,
			Line:   lineNumber(content, m[0]),
		}
		if name != "" && name != "*" {
			imp.Names = []string{name}
		}
		if isStatic {
			imp.Alias = "static"
		}
		fa.Imports = append(fa.Imports, imp)
	}

	// ── Classes ───────────────────────────────────────────────────────────────
	for _, m := range reJavaClass.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		abstract := extractGroup(content, m, 3)
		// static inner class
		name := extractGroup(content, m, 5)
		extends := strings.TrimSpace(extractGroup(content, m, 6))
		implementsRaw := extractGroup(content, m, 7)
		var implements []string
		if implementsRaw != "" {
			for _, s := range strings.Split(implementsRaw, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					implements = append(implements, s)
				}
			}
		}
		ln := lineNumber(content, m[0])

		sig := ""
		if access != "" {
			sig += access + " "
		}
		if strings.TrimSpace(abstract) != "" {
			sig += strings.TrimSpace(abstract) + " "
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
			Exported:   access == "public",
			Decorators: annotByLine[ln-1],
			Line:       ln,
			DocComment: nearestJavadoc(javadocByEnd, m[0]),
		})
	}

	// ── Interfaces ────────────────────────────────────────────────────────────
	for _, m := range reJavaInterface.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		name := extractGroup(content, m, 4)
		extendsRaw := extractGroup(content, m, 5)
		var extends []string
		for _, s := range strings.Split(extendsRaw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				extends = append(extends, s)
			}
		}
		ln := lineNumber(content, m[0])
		sig := access + " interface " + name
		if len(extends) > 0 {
			sig += " extends " + strings.Join(extends, ", ")
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindInterface,
			Name:       name,
			Signature:  sig,
			Exported:   access == "public",
			Decorators: annotByLine[ln-1],
			Line:       ln,
			DocComment: nearestJavadoc(javadocByEnd, m[0]),
		})
	}

	// ── Enums ─────────────────────────────────────────────────────────────────
	for _, m := range reJavaEnum.FindAllStringSubmatchIndex(content, -1) {
		access := extractGroup(content, m, 2)
		name := extractGroup(content, m, 4)
		ln := lineNumber(content, m[0])
		sig := access + " enum " + name
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindEnum,
			Name:       name,
			Signature:  sig,
			Exported:   access == "public",
			Decorators: annotByLine[ln-1],
			Line:       ln,
		})
	}

	// ── Methods ───────────────────────────────────────────────────────────────
	for _, m := range reJavaMethod.FindAllStringSubmatchIndex(content, -1) {
		// reJavaMethod groups (1-indexed):
		//  1=indent  2=access  3=static  4=final  5=abstract  6=synchronized
		//  7=native  8=default  9=returnType  10=name  11=generics  12=params  13=throws
		indent := extractGroup(content, m, 1)
		if indent == "" {
			continue // skip top-level false positives
		}
		access := extractGroup(content, m, 2)
		static := extractGroup(content, m, 3) != ""
		final := extractGroup(content, m, 4) != ""
		abstract := extractGroup(content, m, 5) != ""
		returnType := strings.TrimSpace(extractGroup(content, m, 9))
		name := extractGroup(content, m, 10)
		params := strings.TrimSpace(extractGroup(content, m, 12))
		throws := strings.TrimSpace(extractGroup(content, m, 13))

		if name == "" || returnType == "" {
			continue
		}
		// Skip keywords that match the pattern
		skip := map[string]bool{
			"if": true, "for": true, "while": true, "switch": true,
			"catch": true, "return": true, "new": true,
		}
		if skip[name] || skip[returnType] {
			continue
		}

		ln := lineNumber(content, m[0])
		sig := buildJavaMethodSig(access, static, final, abstract, returnType, name, params, throws)

		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindMethod,
			Name:       name,
			Signature:  sig,
			ReturnType: returnType,
			Parameters: splitParams(params),
			Exported:   access == "public",
			Decorators: annotByLine[ln-1],
			Line:       ln,
			DocComment: nearestJavadoc(javadocByEnd, m[0]),
		})
	}

	// ── Constructors ──────────────────────────────────────────────────────────
	for _, m := range reJavaConstructor.FindAllStringSubmatchIndex(content, -1) {
		// reJavaConstructor groups: 1=indent  2=access  3=name  4=params  5=throws
		indent := extractGroup(content, m, 1)
		if indent == "" {
			continue
		}
		access := extractGroup(content, m, 2)
		name := extractGroup(content, m, 3)
		params := strings.TrimSpace(extractGroup(content, m, 4))
		throws := strings.TrimSpace(extractGroup(content, m, 5))
		ln := lineNumber(content, m[0])
		sig := access + " " + name + "(" + params + ")"
		if throws != "" {
			sig += " throws " + throws
		}
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:       KindMethod,
			Name:       name,
			Signature:  sig,
			Parameters: splitParams(params),
			Exported:   access == "public",
			Decorators: annotByLine[ln-1],
			Line:       ln,
			DocComment: nearestJavadoc(javadocByEnd, m[0]),
		})
	}

	// ── Fields ────────────────────────────────────────────────────────────────
	for _, m := range reJavaField.FindAllStringSubmatchIndex(content, -1) {
		// reJavaField groups: 1=indent  2=access  3=static  4=final  5=transient  6=volatile  7=type  8=name
		access := extractGroup(content, m, 2)
		static := extractGroup(content, m, 3) != ""
		final := extractGroup(content, m, 4) != ""
		typ := strings.TrimSpace(extractGroup(content, m, 7))
		name := extractGroup(content, m, 8)
		if name == "" {
			continue
		}
		sig := access + " "
		if static {
			sig += "static "
		}
		if final {
			sig += "final "
		}
		sig += typ + " " + name
		fa.Symbols = append(fa.Symbols, Symbol{
			Kind:      KindProperty,
			Name:      name,
			Signature: sig,
			ReturnType: typ,
			Exported:  access == "public",
			Line:      lineNumber(content, m[0]),
		})
	}

	return fa
}

// buildJavadocIndex maps javadoc comment end offsets to their cleaned text.
func buildJavadocIndex(content string) map[int]string {
	result := map[int]string{}
	for _, m := range reJavaDoc.FindAllStringIndex(content, -1) {
		raw := content[m[0]:m[1]]
		raw = strings.TrimPrefix(raw, "/**")
		raw = strings.TrimSuffix(raw, "*/")
		lns := strings.Split(raw, "\n")
		cleaned := make([]string, 0, len(lns))
		for _, l := range lns {
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

// nearestJavadoc finds the Javadoc comment closest to declOffset.
func nearestJavadoc(docs map[int]string, declOffset int) string {
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

// buildJavaAnnotationIndex maps the line of an annotated declaration to its annotations.
func buildJavaAnnotationIndex(content string) map[int][]string {
	result := map[int][]string{}
	matches := reJavaAnnotation.FindAllStringSubmatchIndex(content, -1)
	for _, m := range matches {
		ann := content[m[4]:m[5]]
		ln := lineNumber(content, m[0])
		result[ln+1] = append(result[ln+1], ann)
	}
	return result
}

// buildJavaMethodSig assembles a readable Java method signature.
func buildJavaMethodSig(access string, static, final, abstract bool, returnType, name, params, throws string) string {
	var b strings.Builder
	if access != "" {
		b.WriteString(access)
		b.WriteByte(' ')
	}
	if static {
		b.WriteString("static ")
	}
	if final {
		b.WriteString("final ")
	}
	if abstract {
		b.WriteString("abstract ")
	}
	b.WriteString(returnType)
	b.WriteByte(' ')
	b.WriteString(name)
	b.WriteByte('(')
	b.WriteString(params)
	b.WriteByte(')')
	if throws != "" {
		b.WriteString(" throws ")
		b.WriteString(throws)
	}
	return b.String()
}
