package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// R10 W7: a cheap syntax check before an edit or write is reported as
// done. A live run (attempt 10) replaced the end of `if (isMainModule) { … }`
// without its closing brace (esbuild "Unexpected end of file", tsc TS1005
// at 312:1) and duplicated a <script> block leaving the first unclosed;
// the pre-commit tests caught it 11 calls and 50k tokens later and the
// repair loop could not fix it. The check runs on the new content before
// it is written; an edit is refused only when it BREAKS a file that
// parsed before (a file already broken is not blocked), and a checker
// that is missing or slow never blocks (it is logged once).

// syntaxCheckTimeout bounds one external checker run.
const syntaxCheckTimeout = 5 * time.Second

// syntaxCheckMaxBytes: larger files are not checked.
const syntaxCheckMaxBytes = 512 << 10

// syntaxError is one parse failure: where, and what the checker said.
type syntaxError struct {
	checker string
	line    int // 1-based; 0 unknown
	col     int
	msg     string
}

var (
	syntaxLogOnce sync.Map // checker -> logged
	errNoChecker  = errors.New("checker unavailable")
)

func logCheckerOnce(key, msg string) {
	if _, loaded := syntaxLogOnce.LoadOrStore(key, true); !loaded {
		log.Printf("tool-executor: syntax check: %s", msg)
	}
}

// syntaxRejection is the edit_file / write_file result for content that
// would break relPath, or "" when it parses (or parsed no better before,
// or cannot be checked).
func (e *Executor) syntaxRejection(relPath string, oldContent []byte, oldExists bool, newContent []byte) string {
	if len(newContent) > syntaxCheckMaxBytes {
		logCheckerOnce("size", fmt.Sprintf("files over %d KB are not syntax-checked (%s)", syntaxCheckMaxBytes>>10, relPath))
		return ""
	}
	if looksBinary(newContent[:min(len(newContent), 8000)]) {
		logCheckerOnce("binary", "binary content is not syntax-checked ("+relPath+")")
		return ""
	}
	// A new file has no "parsed before" to compare with: only the
	// in-process checkers may refuse it (an external checker run on a
	// temp copy can misjudge it, e.g. node --check on ESM).
	after := e.checkSyntax(relPath, newContent, !oldExists)
	if after == nil {
		return ""
	}
	if oldExists && e.checkSyntax(relPath, oldContent, false) != nil {
		return "" // already unparsable before this edit: not this edit's doing
	}
	var b strings.Builder
	fmt.Fprintf(&b, "edit rejected: it would leave %s unparsable: %s", relPath, after.msg)
	if after.line > 0 {
		fmt.Fprintf(&b, " at %d:%d", after.line, after.col)
	}
	fmt.Fprintf(&b, " (%s).", after.checker)
	if ctx := linesAround(string(newContent), after.line, 1); ctx != "" {
		b.WriteString(" The result would have been:\n")
		b.WriteString(ctx)
	}
	b.WriteString("\nThe file was not changed. Fix the edit so the file stays complete (every brace, tag and string closed) and send it again.")
	return b.String()
}

// linesAround renders lines line-k..line+k of s, numbered, the line marked.
func linesAround(s string, line, k int) string {
	if line <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if line > len(lines) {
		line = len(lines)
	}
	var b strings.Builder
	for i := max(1, line-k); i <= min(len(lines), line+k); i++ {
		mark := "  "
		if i == line {
			mark = "> "
		}
		fmt.Fprintf(&b, "%s%4d: %s\n", mark, i, truncateRunes(lines[i-1], 200))
	}
	return strings.TrimRight(b.String(), "\n")
}

// checkSyntax runs the cheapest available checker for relPath's type;
// nil when the content parses or no checker applies. For a new file only
// the in-process checkers run (JSON on strict names only).
func (e *Executor) checkSyntax(relPath string, content []byte, newFile bool) *syntaxError {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".json":
		if relaxedJSON(relPath) || (newFile && !strictJSON(relPath)) {
			return nil
		}
		return checkJSON(content)
	case ".go":
		return checkGo(relPath, content)
	case ".ts", ".mts", ".cts":
		if !newFile {
			if se, err := e.checkEsbuild(".ts", content); !errors.Is(err, errNoChecker) {
				return se
			}
		}
		return checkBalance("brace balance", string(content), 0)
	case ".tsx":
		if newFile {
			return nil
		}
		se, _ := e.checkEsbuild(ext, content)
		return se
	case ".js", ".mjs", ".cjs":
		if !newFile {
			if se, err := checkNode(ext, content); !errors.Is(err, errNoChecker) {
				return se
			}
		}
		return checkBalance("brace balance", string(content), 0)
	case ".jsx":
		// JSX text may hold apostrophes the balance scan would read as
		// strings: only esbuild checks it, and never a new file.
		if newFile {
			return nil
		}
		se, _ := e.checkEsbuild(ext, content)
		return se
	case ".py":
		se, _ := checkPython(content)
		return se
	case ".html", ".htm":
		// R17: inline scripts go through esbuild / node --check like a .js
		// file (a new file: the balance scan only).
		if newFile {
			return checkHTML(string(content))
		}
		return checkHTMLWith(string(content), e.checkInlineScript)
	}
	return nil
}

// relaxedJSON names JSON files whose dialect allows comments and trailing
// commas (JSONC): never checked strictly.
func relaxedJSON(relPath string) bool {
	p := strings.ToLower(filepath.ToSlash(relPath))
	base := path.Base(p)
	return strings.HasSuffix(base, ".jsonc") || strings.HasPrefix(base, "tsconfig") || strings.HasPrefix(base, "jsconfig") ||
		strings.HasPrefix(p, ".vscode/") || strings.Contains(p, "/.vscode/") ||
		base == "devcontainer.json" || base == ".devcontainer.json"
}

// strictJSON names JSON files that must be strict JSON (a new one is
// checked too): manifests and lock files.
func strictJSON(relPath string) bool {
	base := strings.ToLower(path.Base(filepath.ToSlash(relPath)))
	switch base {
	case "package.json", "package-lock.json", "npm-shrinkwrap.json", "composer.json":
		return true
	}
	return strings.HasSuffix(base, "-lock.json") || strings.HasSuffix(base, ".lock.json")
}

func checkJSON(content []byte) *syntaxError {
	if len(bytes.TrimSpace(content)) == 0 || json.Valid(content) {
		return nil
	}
	var v any
	err := json.Unmarshal(content, &v)
	se := &syntaxError{checker: "json", msg: "invalid JSON"}
	var serr *json.SyntaxError
	if errors.As(err, &serr) {
		se.msg = serr.Error()
		se.line, se.col = lineColAt(string(content), int(serr.Offset))
	}
	return se
}

func checkGo(relPath string, content []byte) *syntaxError {
	_, err := parser.ParseFile(token.NewFileSet(), relPath, content, parser.AllErrors)
	if err == nil {
		return nil
	}
	se := &syntaxError{checker: "go/parser", msg: err.Error()}
	var list scanner.ErrorList
	if errors.As(err, &list) && len(list) > 0 {
		se.line, se.col, se.msg = list[0].Pos.Line, list[0].Pos.Column, list[0].Msg
	}
	return se
}

// esbuildErrRE matches esbuild's "<stdin>:303:0: ERROR: …" (log-level
// error, no color) or its pretty "✘ [ERROR] msg" + "<stdin>:303:0:" form.
var (
	esbuildLocRE = regexp.MustCompile(`<stdin>:(\d+):(\d+)`)
	esbuildMsgRE = regexp.MustCompile(`(?m)(?:\[ERROR\]|error:|ERROR:)\s*(.+)$`)
)

// checkEsbuild parses with the worktree's own esbuild (a Node project
// usually has it in node_modules); errNoChecker when there is none or it does not finish.
func (e *Executor) checkEsbuild(ext string, content []byte) (*syntaxError, error) {
	bin := filepath.Join(e.repoPath, "node_modules", ".bin", "esbuild")
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		logCheckerOnce("esbuild", "no node_modules/.bin/esbuild in the worktree; TypeScript edits get the brace-balance check")
		return nil, errNoChecker
	}
	ctx, cancel := context.WithTimeout(context.Background(), syntaxCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--loader="+strings.TrimPrefix(ext, "."), "--log-level=error", "--color=false")
	cmd.Dir = e.repoPath
	cmd.Stdin = bytes.NewReader(content)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = nil, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		logCheckerOnce("esbuild-timeout", "esbuild did not finish in 5s; not blocking")
		return nil, errNoChecker
	}
	if err == nil {
		return nil, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		logCheckerOnce("esbuild-run", "esbuild did not run: "+err.Error())
		return nil, errNoChecker
	}
	return parseEsbuildError(out.String()), nil
}

// parseEsbuildError reads esbuild's first error. Its columns are 0-based.
func parseEsbuildError(out string) *syntaxError {
	se := &syntaxError{checker: "esbuild", msg: strings.TrimSpace(firstLineOf(out))}
	if m := esbuildMsgRE.FindStringSubmatch(out); m != nil {
		se.msg = strings.TrimSpace(m[1])
	}
	if m := esbuildLocRE.FindStringSubmatch(out); m != nil {
		se.line, _ = strconv.Atoi(m[1])
		c, _ := strconv.Atoi(m[2])
		se.col = c + 1
	}
	return se
}

var nodeOnce struct {
	sync.Once
	path string
}

// checkNode runs `node --check` on a temp copy; errNoChecker without node.
func checkNode(ext string, content []byte) (*syntaxError, error) {
	nodeOnce.Do(func() { nodeOnce.path, _ = exec.LookPath("node") })
	if nodeOnce.path == "" {
		logCheckerOnce("node", "node not on PATH; JavaScript edits get the brace-balance check")
		return nil, errNoChecker
	}
	f, err := os.CreateTemp("", "ae-syntax-*"+ext)
	if err != nil {
		return nil, errNoChecker
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(content); err != nil {
		f.Close()
		return nil, errNoChecker
	}
	f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), syntaxCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, nodeOnce.path, "--check", f.Name()).CombinedOutput()
	if ctx.Err() != nil {
		logCheckerOnce("node-timeout", "node --check did not finish in 5s; not blocking")
		return nil, errNoChecker
	}
	if err == nil {
		return nil, nil
	}
	se := &syntaxError{checker: "node --check", msg: "syntax error"}
	text := string(out)
	if m := regexp.MustCompile(`(?m)^(SyntaxError: .+)$`).FindStringSubmatch(text); m != nil {
		se.msg = m[1]
	}
	if m := regexp.MustCompile(regexp.QuoteMeta(f.Name()) + `:(\d+)`).FindStringSubmatch(text); m != nil {
		se.line, _ = strconv.Atoi(m[1])
		se.col = 1
	}
	return se, nil
}

var pythonOnce struct {
	sync.Once
	path string
}

// checkPython runs py_compile when a python is on PATH; else no check.
func checkPython(content []byte) (*syntaxError, error) {
	pythonOnce.Do(func() {
		for _, n := range []string{"python3", "python"} {
			if p, err := exec.LookPath(n); err == nil {
				pythonOnce.path = p
				return
			}
		}
	})
	if pythonOnce.path == "" {
		logCheckerOnce("python", "no python on PATH; Python edits are not syntax-checked")
		return nil, errNoChecker
	}
	f, err := os.CreateTemp("", "ae-syntax-*.py")
	if err != nil {
		return nil, errNoChecker
	}
	defer os.Remove(f.Name())
	_, _ = f.Write(content)
	f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), syntaxCheckTimeout)
	defer cancel()
	src := "import ast,sys\ntry:\n ast.parse(open(sys.argv[1],encoding='utf-8').read())\nexcept SyntaxError as e:\n print(f'{e.lineno}:{e.offset}:{e.msg}'); sys.exit(1)\n"
	out, err := exec.CommandContext(ctx, pythonOnce.path, "-c", src, f.Name()).CombinedOutput()
	if ctx.Err() != nil {
		return nil, errNoChecker
	}
	if err == nil {
		return nil, nil
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), ":", 3)
	se := &syntaxError{checker: "python ast", msg: strings.TrimSpace(string(out))}
	if len(parts) == 3 {
		se.line, _ = strconv.Atoi(parts[0])
		se.col, _ = strconv.Atoi(parts[1])
		se.msg = parts[2]
	}
	return se, nil
}

// checkHTML checks that every <script> and <style> element is closed and
// that the braces inside inline scripts balance (attempt 10 left the first
// of two <script> blocks unclosed).
func checkHTML(s string) *syntaxError { return checkHTMLWith(s, balanceInlineScript) }

// inlineScriptCheck checks one inline script: body is its text, which
// starts at line:col of the page; openTag is its opening tag, lower-cased.
// Reported positions are the page's.
type inlineScriptCheck func(body string, line, col int, openTag string) *syntaxError

func balanceInlineScript(body string, line, _ int, _ string) *syntaxError {
	return checkBalance("inline <script> brace balance", body, line-1)
}

// checkInlineScript (R17) parses a JavaScript inline script with the same
// checkers as a .js file — the worktree's esbuild, else node --check (a
// classic script as .js, type=module as .mjs) — with lines mapped back to
// the page. A live run (attempt 14) inserted a fetch block inside an
// object literal passed to a call (`createX('id', {`): braces balanced, so the balance scan
// passed it. Non-JavaScript script types (JSON, templates), empty bodies
// and hosts without either checker get the balance scan.
func (e *Executor) checkInlineScript(body string, line, col int, openTag string) *syntaxError {
	typ := scriptType(openTag)
	if strings.TrimSpace(body) == "" || !isJSScriptType(typ) {
		return balanceInlineScript(body, line, col, openTag)
	}
	module := typ == "module"
	key := fmt.Sprintf("%s\x00%t\x00%x", e.repoPath, module, sha256.Sum256([]byte(body)))
	var se *syntaxError
	if v, ok := inlineScriptCache.Load(key); ok {
		se = v.(*syntaxError)
	} else {
		var err error
		se, err = e.checkEsbuild(".js", []byte(body))
		if errors.Is(err, errNoChecker) {
			ext := ".js"
			if module {
				ext = ".mjs"
			}
			se, err = checkNode(ext, []byte(body))
		}
		if errors.Is(err, errNoChecker) {
			return balanceInlineScript(body, line, col, openTag)
		}
		if inlineScriptCacheSize.Add(1) > 1024 {
			inlineScriptCache.Range(func(k, _ any) bool { inlineScriptCache.Delete(k); return true })
			inlineScriptCacheSize.Store(0)
		}
		inlineScriptCache.Store(key, se)
	}
	if se == nil {
		return nil
	}
	mapped := *se
	mapped.checker = se.checker + " (inline <script>)"
	if mapped.line > 0 {
		if mapped.line == 1 {
			mapped.col += col - 1
		}
		mapped.line += line - 1
	}
	return &mapped
}

// inlineScriptCache keeps external checker results by script text: an edit
// is checked before and after, and most of a page's scripts do not change.
var (
	inlineScriptCache     sync.Map // key -> *syntaxError (nil: parses)
	inlineScriptCacheSize atomic.Int64
)

var scriptTypeRE = regexp.MustCompile(`\stype\s*=\s*["']?([^"'\s>]*)`)

// scriptType is the type attribute of a lower-cased <script …> tag.
func scriptType(openTag string) string {
	if m := scriptTypeRE.FindStringSubmatch(openTag); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func isJSScriptType(t string) bool {
	switch t {
	case "", "module", "text/javascript", "application/javascript", "text/ecmascript", "application/ecmascript", "application/x-javascript", "text/jscript":
		return true
	}
	return false
}

// checkHTMLWith is checkHTML with check run on each inline script.
func checkHTMLWith(s string, check inlineScriptCheck) *syntaxError {
	lower := strings.ToLower(s)
	for _, tag := range []string{"script", "style"} {
		open, close := "<"+tag, "</"+tag
		pos := 0
		for {
			i := indexTag(lower, open, pos)
			if i < 0 {
				break
			}
			gt := strings.Index(lower[i:], ">")
			if gt < 0 {
				l, c := lineColAt(s, i)
				return &syntaxError{checker: "html", line: l, col: c, msg: fmt.Sprintf("<%s> tag is not finished", tag)}
			}
			bodyStart := i + gt + 1
			j := strings.Index(lower[bodyStart:], close)
			notClosed := j < 0
			// An opener before the close is a nested, unclosed element —
			// unless it is text inside a JS string, template or comment
			// (document.write('<script …>')).
			for next := indexTag(lower, open, bodyStart); !notClosed && next >= 0 && next < bodyStart+j; next = indexTag(lower, open, next+len(open)) {
				if tag != "script" || !inJSLiteral(s[bodyStart:], next-bodyStart) {
					notClosed = true
				}
			}
			if notClosed {
				l, c := lineColAt(s, i)
				return &syntaxError{checker: "html", line: l, col: c, msg: fmt.Sprintf("<%s> opened here is not closed before the next <%s> or the end of the file", tag, tag)}
			}
			if tag == "script" {
				startLine, startCol := lineColAt(s, bodyStart)
				if se := check(s[bodyStart:bodyStart+j], startLine, startCol, lower[i:bodyStart]); se != nil {
					return se
				}
			}
			pos = bodyStart + j + len(close)
		}
		// A closing tag without an opener.
		if strings.Count(lower, close) > countTags(lower, open) {
			i := strings.LastIndex(lower, close)
			l, c := lineColAt(s, i)
			return &syntaxError{checker: "html", line: l, col: c, msg: fmt.Sprintf("</%s> without an opening <%s>", tag, tag)}
		}
	}
	return nil
}

// indexTag finds "<script" followed by '>' or whitespace (not
// "<scripts"), from pos.
func indexTag(lower, open string, pos int) int {
	for pos < len(lower) {
		i := strings.Index(lower[pos:], open)
		if i < 0 {
			return -1
		}
		k := pos + i + len(open)
		if k < len(lower) && (lower[k] == '>' || lower[k] == ' ' || lower[k] == '\t' || lower[k] == '\n' || lower[k] == '\r') {
			return pos + i
		}
		pos = k
	}
	return -1
}

func countTags(lower, open string) int {
	n, pos := 0, 0
	for {
		i := indexTag(lower, open, pos)
		if i < 0 {
			return n
		}
		n++
		pos = i + len(open)
	}
}

// lineColAt is the 1-based line and column of byte offset off.
func lineColAt(s string, off int) (int, int) {
	if off > len(s) {
		off = len(s)
	}
	line := strings.Count(s[:off], "\n") + 1
	col := off - (strings.LastIndex(s[:off], "\n") + 1) + 1
	return line, col
}

// checkBalance checks that (), [] and {} balance in JS/TS-like source,
// skipping strings, template literals (with ${…} nesting), comments and
// regex literals. lineOffset is added to reported lines (an inline script).
func checkBalance(checker, src string, lineOffset int) *syntaxError {
	type open struct {
		ch        byte
		line, col int
		template  bool // the "${" of a template literal
	}
	var stack []open
	line, col := 1, 0
	at := func() (int, int) { return line + lineOffset, col }
	pair := map[byte]byte{')': '(', ']': '[', '}': '{'}
	n := len(src)
	inTemplate := 0 // depth of template literals whose text we are in
	for i := 0; i < n; i++ {
		c := src[i]
		if c == '\n' {
			line++
			col = 0
			continue
		}
		col++
		if inTemplate > 0 {
			switch {
			case c == '\\':
				i++
				col++
			case c == '`':
				inTemplate--
			case c == '$' && i+1 < n && src[i+1] == '{':
				l, cc := at()
				stack = append(stack, open{'{', l, cc, true})
				inTemplate--
				i++
				col++
			}
			continue
		}
		switch c {
		case ' ', '\t', '\r':
			continue
		case '/':
			if i+1 < n && src[i+1] == '/' {
				for i < n && src[i] != '\n' {
					i++
				}
				i--
				continue
			}
			if i+1 < n && src[i+1] == '*' {
				i += 2
				col += 2
				for i < n && !(src[i] == '*' && i+1 < n && src[i+1] == '/') {
					if src[i] == '\n' {
						line++
						col = 0
					} else {
						col++
					}
					i++
				}
				i++
				col++
				continue
			}
			if regexMayStart(src, i) {
				// A regex literal: skip to its closing '/', classes included.
				inClass := false
				for i++; i < n && src[i] != '\n'; i++ {
					col++
					if src[i] == '\\' {
						i++
						col++
						continue
					}
					if src[i] == '[' {
						inClass = true
					} else if src[i] == ']' {
						inClass = false
					} else if src[i] == '/' && !inClass {
						break
					}
				}
				continue
			}
		case '\'', '"':
			q := c
			for i++; i < n && src[i] != q && src[i] != '\n'; i++ {
				col++
				if src[i] == '\\' {
					i++
					col++
				}
			}
			col++
			continue
		case '`':
			inTemplate++
			continue
		case '(', '[', '{':
			l, cc := at()
			stack = append(stack, open{c, l, cc, false})
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1].ch != pair[c] {
				l, cc := at()
				if len(stack) == 0 {
					return &syntaxError{checker: checker, line: l, col: cc, msg: fmt.Sprintf("unexpected '%c' (nothing open)", c)}
				}
				top := stack[len(stack)-1]
				return &syntaxError{checker: checker, line: l, col: cc, msg: fmt.Sprintf("unexpected '%c': '%c' opened at %d:%d is still open", c, top.ch, top.line, top.col)}
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.template {
				inTemplate++ // back in the template's text
			}
		}
	}
	if inTemplate > 0 {
		l, cc := at()
		return &syntaxError{checker: checker, line: l, col: cc, msg: "unterminated template literal"}
	}
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		l, _ := at()
		return &syntaxError{checker: checker, line: top.line, col: top.col, msg: fmt.Sprintf("'%c' opened here is never closed (end of input at line %d)", top.ch, l)}
	}
	return nil
}

// regexKeywords may directly precede a regex literal.
var regexKeywords = map[string]bool{"return": true, "typeof": true, "case": true, "instanceof": true, "in": true, "of": true,
	"delete": true, "void": true, "throw": true, "new": true, "else": true, "do": true, "yield": true, "await": true}

// regexMayStart reports whether the '/' at src[i] starts a regex literal:
// at the start, after one of ( , = : [ ! & | ? { } ; + - * % < > ~ ^, or
// after a keyword such as return / typeof / case. After an identifier,
// a number, ')' or ']' it is a division.
func regexMayStart(src string, i int) bool {
	k := i - 1
	for k >= 0 && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n' || src[k] == '\r') {
		k--
	}
	if k < 0 {
		return true
	}
	c := src[k]
	if strings.IndexByte("(,=:[!&|?{};+-*%<>~^", c) >= 0 {
		return true
	}
	if isIdentByte(c) {
		e := k + 1
		for k >= 0 && isIdentByte(src[k]) {
			k--
		}
		return regexKeywords[src[k+1:e]]
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// inJSLiteral reports whether byte off of js is inside a string, template
// literal or comment.
func inJSLiteral(js string, off int) bool {
	for i := 0; i < off && i < len(js); i++ {
		switch c := js[i]; {
		case c == '/' && i+1 < len(js) && js[i+1] == '/':
			e := strings.IndexByte(js[i:], '\n')
			if e < 0 || i+e > off {
				return true
			}
			i += e
		case c == '/' && i+1 < len(js) && js[i+1] == '*':
			e := strings.Index(js[i+2:], "*/")
			if e < 0 || i+2+e+2 > off {
				return true
			}
			i += 2 + e + 1
		case c == '\'' || c == '"' || c == '`':
			k := i + 1
			for k < len(js) && js[k] != c && (c == '`' || js[k] != '\n') {
				if js[k] == '\\' {
					k++
				}
				k++
			}
			if k >= off {
				return true
			}
			i = k
		}
	}
	return false
}
