package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

func attempt10Edit(t *testing.T, call, edit int) adapter.ToolCall {
	t.Helper()
	b, err := os.ReadFile("testdata/attempt10_edits.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Edits []struct {
			Call, Edit int
			Name       string
			Arguments  string
		} `json:"edits"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Edits {
		if e.Call == call && e.Edit == edit {
			return adapter.ToolCall{ID: "e", Name: e.Name, Arguments: e.Arguments}
		}
	}
	t.Fatalf("no edit %d.%d", call, edit)
	return adapter.ToolCall{}
}

// R10 W7: attempt 10's call-9 edit replaced the end of `if (isMainModule)
// { … }` without its closing brace. It is refused, naming the brace that
// is left open, and the file is unchanged. (The fixture has no
// node_modules: the check degrades to the brace balance.)
func TestSyntaxCheckRejectsAttempt10DeletedBrace(t *testing.T) {
	root := attempt9Tree(t)
	before := readTree(t, root, "src/server/index.ts")
	ex := NewExecutor(root)
	r := runEdit(t, ex, attempt10Edit(t, 9, 0))
	if !r.IsError || !strings.HasPrefix(r.Content, "edit rejected: it would leave src") || !strings.Contains(r.Content, "unparsable") {
		t.Fatalf("got %v %s", r.IsError, r.Content)
	}
	for _, want := range []string{"'{' opened here is never closed", "at 219:", "if (isMainModule) {", "brace balance"} {
		if !strings.Contains(r.Content, want) {
			t.Errorf("rejection lacks %q:\n%s", want, r.Content)
		}
	}
	if readTree(t, root, "src/server/index.ts") != before {
		t.Fatal("the file changed")
	}
}

// R10 W7: attempt 10's call 5 appended a <script> before </body>; call 6
// then replaced that block's closing tag with a second copy, leaving the
// first unclosed. Call 5 is accepted, call 6 refused.
func TestSyntaxCheckRejectsAttempt10UnclosedScript(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	if r := runEdit(t, ex, attempt10Edit(t, 5, 0)); r.IsError {
		t.Fatalf("call-5 edit (valid) rejected: %s", r.Content)
	}
	after5 := readTree(t, root, "public/index.html")
	call6 := attempt10Edit(t, 6, 1)
	r := runEdit(t, ex, call6)
	// The re-typed block guard (W1) refuses it first; the syntax check
	// refuses the same result on its own.
	if !r.IsError {
		t.Fatalf("call-6 edit applied: %s", r.Content)
	}
	if readTree(t, root, "public/index.html") != after5 {
		t.Fatal("the file changed")
	}
	var args map[string]string
	_ = json.Unmarshal([]byte(call6.Arguments), &args)
	result := strings.Replace(after5, args["old_string"], args["new_string"], 1)
	rej := ex.syntaxRejection("public/index.html", []byte(after5), true, []byte(result))
	if !strings.Contains(rej, "<script> opened here is not closed") || !strings.Contains(rej, "at 1696:") {
		t.Fatalf("syntax check on call 6's result: %s", rej)
	}
	if rej := ex.syntaxRejection("public/index.html", nil, false, []byte(after5)); rej != "" {
		t.Fatalf("call 5's (valid) result rejected: %s", rej)
	}
}

// Valid edits pass; an already-broken file is not blocked; JSON and Go are
// parsed for real; write_file is checked too.
func TestSyntaxCheckValidAndPreexisting(t *testing.T) {
	root := attempt9Tree(t)
	ex := NewExecutor(root)
	if r := runEdit(t, ex, editCall(t, map[string]any{"path": "src/server/index.ts", "old_string": "  return { app };", "new_string": "  app.get('/api/settings', (_req, res) => {\n    res.json({ ok: `${1}` });\n  });\n\n  return { app };"})); r.IsError {
		t.Fatalf("valid edit rejected: %s", r.Content)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("broken.ts", "function f() {\n  return 1;\n")
	if r := runEdit(t, ex, editCall(t, map[string]any{"path": "broken.ts", "old_string": "return 1;", "new_string": "return 2;"})); r.IsError {
		t.Fatalf("edit of an already-broken file blocked: %s", r.Content)
	}
	write("c.json", "{\"a\": 1}\n")
	if r := runEdit(t, ex, editCall(t, map[string]any{"path": "c.json", "old_string": "\"a\": 1", "new_string": "\"a\": 1,"})); !r.IsError || !strings.Contains(r.Content, "json") {
		t.Fatalf("invalid JSON accepted: %s", r.Content)
	}
	write("m.go", "package m\n\nfunc F() int {\n\treturn 1\n}\n")
	if r := runEdit(t, ex, editCall(t, map[string]any{"path": "m.go", "old_string": "\treturn 1\n}", "new_string": "\treturn 1\n"})); !r.IsError || !strings.Contains(r.Content, "go/parser") {
		t.Fatalf("broken Go accepted: %s", r.Content)
	}
	ex.Execute(context.Background(), adapter.ToolCall{ID: "r", Name: "read_file", Arguments: `{"path":"new.ts"}`})
	if r := ex.Execute(context.Background(), adapter.ToolCall{ID: "w", Name: "write_file", Arguments: `{"path":"new.ts","content":"export const x = {\n"}`}); !r.IsError || !strings.Contains(r.Content, "unparsable") {
		t.Fatalf("broken new file written: %s", r.Content)
	}
}

// R10 review: a regex after return/typeof/case/'}' is skipped, and a
// <script in a JS string does not count as an unclosed element.
func TestSyntaxCheckRegexAndScriptInString(t *testing.T) {
	for _, s := range []string{
		"function f(s) {\n  return /[}{]/.test(s);\n}\n",
		"const k = typeof /}/;\nswitch (x) { case 1: y = 2 }\nif (a) { b() }\n/}/.exec(z);\n",
	} {
		if se := checkBalance("b", s, 0); se != nil {
			t.Errorf("%q: %+v", s, *se)
		}
	}
	if se := checkBalance("b", "const r = a / b; const q = (c) / 2; {", 0); se == nil {
		t.Error("division misread as a regex hid the open brace")
	}
	html := "<html><body>\n<script>\n  document.write('<script src=\"a.js\"><\\/script>');\n  const t = `<script>`;\n</script>\n</body></html>\n"
	if se := checkHTML(html); se != nil {
		t.Fatalf("<script in a string flagged: %+v", *se)
	}
	if se := checkHTML("<script>\nfoo();\n<script>\nbar();\n</script>\n"); se == nil {
		t.Fatal("a real nested <script> passed")
	}
}

// R10 review: JSONC-dialect files are never checked strictly, a new JSON
// file only on a strict name, a new file never by an external checker,
// and large or binary content is skipped.
func TestSyntaxCheckScope(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	bad := []byte("{\n  // comment\n  \"a\": 1,\n}\n")
	for _, p := range []string{"tsconfig.json", "tsconfig.build.json", "jsconfig.json", ".vscode/settings.json", "x/.devcontainer/devcontainer.json", "a.jsonc"} {
		if rej := ex.syntaxRejection(p, []byte("{}"), true, bad); rej != "" {
			t.Errorf("%s refused: %s", p, rej)
		}
	}
	if rej := ex.syntaxRejection("data/new.json", nil, false, bad); rej != "" {
		t.Errorf("new non-strict JSON refused: %s", rej)
	}
	if rej := ex.syntaxRejection("package.json", nil, false, bad); rej == "" {
		t.Error("new package.json with a trailing comma accepted")
	}
	if rej := ex.syntaxRejection("data/x.json", []byte("{}"), true, bad); rej == "" {
		t.Error("existing JSON broken by the edit accepted")
	}
	if rej := ex.syntaxRejection("src/new.tsx", nil, false, []byte("export const A = () => <div>{</div>;\n")); rej != "" {
		t.Errorf("new .tsx refused without an in-process checker: %s", rej)
	}
	if rej := ex.syntaxRejection("src/new.ts", nil, false, []byte("export const x = {\n")); rej == "" {
		t.Error("new .ts with an open brace accepted (balance is in-process)")
	}
	big := []byte("const x = {\n" + strings.Repeat("// pad\n", 80000))
	if rej := ex.syntaxRejection("big.ts", []byte("const x = 1;\n"), true, big); rej != "" {
		t.Errorf("large file checked: %s", rej[:80])
	}
	if rej := ex.syntaxRejection("blob.js", []byte("x"), true, []byte{'{', 0, 1, 2}); rej != "" {
		t.Errorf("binary checked: %s", rej)
	}
}

// The balance heuristic skips strings, templates, comments and regexes.
func TestBalanceHeuristic(t *testing.T) {
	ok := []string{
		"const s = '{'; const t = \"}\"; // { \n/* } */ const r = /[{]/g; const u = `a ${ {x: 1}.x } }`;\n",
		"if (a) { b(`${c}`) }\nconst d = x / y / z;\n",
	}
	for _, s := range ok {
		if se := checkBalance("b", s, 0); se != nil {
			t.Errorf("%q: %+v", s, *se)
		}
	}
	bad := map[string]string{
		"function f() {\n  if (x) {\n}\n": "never closed",
		"a(b];\n":                         "unexpected ']'",
		"const t = `abc ${x}\n":           "unterminated template",
	}
	for s, want := range bad {
		if se := checkBalance("b", s, 0); se == nil || !strings.Contains(se.msg, want) {
			t.Errorf("%q: %+v, want %q", s, se, want)
		}
	}
	se := parseEsbuildError("✘ [ERROR] Unexpected end of file\n\n    <stdin>:311:0:\n      311 │ \n")
	if se.msg != "Unexpected end of file" || se.line != 311 || se.col != 1 {
		t.Fatalf("esbuild parse: %+v", *se)
	}
}
