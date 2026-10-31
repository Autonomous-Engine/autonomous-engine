// Package langanalyzer provides deep, per-language static analysis using
// comprehensive regex patterns. It replaces the shallow regex extraction in
// the knowledge-indexer with thorough extraction of functions, types, imports,
// exports, decorators, and doc comments for Go, JavaScript, TypeScript, Python,
// Java, C#, and C++.
package langanalyzer

// SymbolKind represents the type of code symbol extracted.
type SymbolKind string

const (
	KindFunction  SymbolKind = "function"
	KindMethod    SymbolKind = "method"
	KindClass     SymbolKind = "class"
	KindInterface SymbolKind = "interface"
	KindStruct    SymbolKind = "struct"
	KindEnum      SymbolKind = "enum"
	KindType      SymbolKind = "type"
	KindConstant  SymbolKind = "constant"
	KindVariable  SymbolKind = "variable"
	KindImport    SymbolKind = "import"
	KindExport    SymbolKind = "export"
	KindDecorator SymbolKind = "decorator"
	KindNamespace SymbolKind = "namespace"
	KindProperty  SymbolKind = "property"
)

// Symbol represents a single extracted code symbol.
type Symbol struct {
	Kind       SymbolKind `json:"kind"`
	Name       string     `json:"name"`
	Signature  string     `json:"signature"`             // full signature for functions/methods
	ReturnType string     `json:"return_type,omitempty"` // declared return type
	Parameters []string   `json:"parameters,omitempty"`  // individual parameter strings
	Decorators []string   `json:"decorators,omitempty"`  // @Foo or [Foo] annotations
	Parent     string     `json:"parent,omitempty"`      // enclosing class/struct/namespace
	Extends    string     `json:"extends,omitempty"`     // base class
	Implements []string   `json:"implements,omitempty"`  // implemented interfaces
	Exported   bool       `json:"exported"`
	Line       int        `json:"line"`
	DocComment string     `json:"doc_comment,omitempty"` // leading doc comment text
}

// FileAnalysis is the result of analysing a single source file.
type FileAnalysis struct {
	FilePath  string   `json:"file_path"`
	Language  string   `json:"language"`
	Package   string   `json:"package"` // package / namespace / module name
	Symbols   []Symbol `json:"symbols"`
	Imports   []Import `json:"imports"`
	LineCount int      `json:"line_count"`
}

// Import represents a single import statement.
type Import struct {
	Module string   `json:"module"`           // package path or module name
	Names  []string `json:"names,omitempty"`  // specific names imported
	Alias  string   `json:"alias,omitempty"`  // local alias
	Line   int      `json:"line"`
}

// AnalyzeFile dispatches to the appropriate language analyser and returns a
// rich FileAnalysis for the given source content.
func AnalyzeFile(filePath, language, content string) *FileAnalysis {
	switch language {
	case "Go":
		return analyzeGo(filePath, content)
	case "JavaScript":
		return analyzeJavaScript(filePath, content)
	case "TypeScript":
		return analyzeTypeScript(filePath, content)
	case "Python":
		return analyzePython(filePath, content)
	case "Java":
		return analyzeJava(filePath, content)
	case "C#":
		return analyzeCSharp(filePath, content)
	case "C++":
		return analyzeCPP(filePath, content)
	default:
		lc := countLines(content)
		return &FileAnalysis{FilePath: filePath, Language: language, LineCount: lc}
	}
}

// countLines returns the number of newline-separated lines in s.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := 1
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}

// lineNumber returns the 1-based line number of byte offset idx in content.
func lineNumber(content string, idx int) int {
	line := 1
	for i := 0; i < idx && i < len(content); i++ {
		if content[i] == '\n' {
			line++
		}
	}
	return line
}

// trimComment strips leading // or # characters and whitespace from a single
// comment line.
func trimComment(s string) string {
	for len(s) > 0 && (s[0] == '/' || s[0] == '#' || s[0] == ' ' || s[0] == '\t' || s[0] == '*') {
		s = s[1:]
	}
	return s
}

// isExportedName returns true when the first rune of name is an upper-case
// ASCII letter (Go convention) or any non-empty name (for other languages
// where export is determined by keywords rather than capitalization).
func isExportedGoName(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}
