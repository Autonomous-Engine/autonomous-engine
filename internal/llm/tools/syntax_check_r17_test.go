package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// R17 L3: inline <script> blocks are parsed like a .js file.

func requireJSChecker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node on PATH (and no esbuild in the fixture): inline scripts get the balance scan")
	}
}

// landingPage is the sample-app's landing/index.html (LF), a synthetic
// page whose inline script opens an options object at line 843.
func landingPage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sample-app", "landing", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// attempt14Fetch has the shape of the block a live run (attempt 14)
// inserted into its landing page: blank lines, a comment, then a fetch
// chain whose .catch body runs on into a stray if block.
const attempt14Fetch = `

// Load the reader settings
fetch(` + "`${window.location.origin}/api/settings`" + `)
  .then(response => response.json())
  .then(settings => {
    const searchApiKey = settings.searchApiKey;

    // Search box above the shelf
    createSearchBox(` + "`https://search.example.test/v1/books${searchApiKey ? `?key=${searchApiKey}` : ''}`" + `, {
      placeholder: 'Search the shelf',
      maxResults: 20,
    }).mount(shelf);
  })
  .catch(error => {
    console.error('Failed to load reader settings:', error);


if (!settings.searchApiKey) {
  const notice = document.createElement('div');
  notice.className = 'search-notice';
  notice.textContent = 'Search unavailable: SEARCH_API_KEY is not configured. See README (Configuration).';
  document.body.appendChild(notice);
}
  });`

// insertAfterLine inserts block after the 1-based line n.
func insertAfterLine(t *testing.T, page string, n int, want, block string) string {
	t.Helper()
	lines := strings.Split(page, "\n")
	if strings.TrimSpace(lines[n-1]) != want {
		t.Fatalf("line %d = %q, want %q", n, lines[n-1], want)
	}
	out := append([]string{}, lines[:n]...)
	out = append(out, strings.Split(block, "\n")...)
	return strings.Join(append(out, lines[n:]...), "\n")
}

// Attempt 14's insertion inside the `createShelf('shelf', {` options object
// balances its braces but is not JavaScript: it is refused, naming the
// page line of the error.
func TestInlineScriptInsertInsideAnObjectIsRefused(t *testing.T) {
	requireJSChecker(t)
	page := landingPage(t)
	edited := insertAfterLine(t, page, 843, "const shelf = createShelf('shelf', {", attempt14Fetch)
	if se := checkBalance("b", edited, 0); se != nil {
		t.Fatalf("precondition: the balance scan should pass this edit, got %+v", se)
	}
	ex := NewExecutor(t.TempDir())
	got := ex.syntaxRejection("landing/index.html", []byte(page), true, []byte(edited))
	if got == "" {
		t.Fatal("the insertion inside createShelf's options object was not refused")
	}
	fetchLine := 0
	for i, l := range strings.Split(edited, "\n") {
		if strings.HasPrefix(l, "fetch(") {
			fetchLine = i + 1
		}
	}
	if !strings.Contains(got, "(inline <script>)") || !strings.Contains(got, "landing/index.html unparsable") {
		t.Fatalf("rejection:\n%s", got)
	}
	// The mapped line is the page's: the fetch line (847), not line 9 of
	// the script.
	if !strings.Contains(got, " at 847:") || fetchLine != 847 {
		t.Fatalf("rejection does not name page line 847 (fetch at %d):\n%s", fetchLine, got)
	}
	if !strings.Contains(got, ">  847: fetch(") {
		t.Fatalf("rejection does not quote the line:\n%s", got)
	}
	t.Logf("refused: %s", strings.SplitN(got, "\n", 2)[0])
}

// The same block as a statement after the cover strip is valid and passes.
func TestInlineScriptValidInsertPasses(t *testing.T) {
	requireJSChecker(t)
	page := landingPage(t)
	edited := insertAfterLine(t, page, 859, "}).mount(shelf);", attempt14Fetch)
	ex := NewExecutor(t.TempDir())
	if got := ex.syntaxRejection("landing/index.html", []byte(page), true, []byte(edited)); got != "" {
		t.Fatalf("a valid insertion was refused:\n%s", got)
	}
}

// Positions map back to the page on the script's first line too; a
// non-JavaScript script type keeps the balance scan; an already-broken
// script does not block an edit elsewhere.
func TestInlineScriptPositionsAndTypes(t *testing.T) {
	requireJSChecker(t)
	ex := NewExecutor(t.TempDir())
	page := "<html>\n<body>\n<script>const a = {\n  b: 1,\n  fetch(`x`)\n};\n</script>\n</body>\n</html>\n"
	se := ex.checkSyntax("p.html", []byte(page), false)
	if se == nil || se.line != 5 || !strings.Contains(se.checker, "inline <script>") {
		t.Fatalf("se = %+v, want line 5", se)
	}
	first := "<script>  const a = ;\n</script>\n"
	if se := ex.checkSyntax("p.html", []byte(first), false); se == nil || se.line != 1 || se.col < 9 {
		t.Fatalf("first-line error = %+v, want 1:>=9 (a page column: the script starts at 1:9)", se)
	}
	ld := "<script type=\"application/ld+json\">{\"@context\": \"https://schema.org\", \"name\": \"x\"}</script>\n"
	if se := ex.checkSyntax("p.html", []byte(ld), false); se != nil {
		t.Fatalf("ld+json script: %+v", se)
	}
	mod := "<script type=\"module\">import { a } from './a.js';\nconsole.log(a);\n</script>\n"
	if se := ex.checkSyntax("p.html", []byte(mod), false); se != nil {
		t.Fatalf("module script: %+v", se)
	}
	broken := "<script>\nconst a = {\n  fetch(`x`)\n};\n</script>\n<p>old</p>\n"
	if got := ex.syntaxRejection("p.html", []byte(broken), true, []byte(strings.Replace(broken, "old", "new", 1))); got != "" {
		t.Fatalf("an already-broken script blocked an unrelated edit:\n%s", got)
	}
	// A new file gets the in-process scan only.
	if se := ex.checkSyntax("p.html", []byte(broken), true); se != nil {
		t.Fatalf("new file: %+v", se)
	}
}
