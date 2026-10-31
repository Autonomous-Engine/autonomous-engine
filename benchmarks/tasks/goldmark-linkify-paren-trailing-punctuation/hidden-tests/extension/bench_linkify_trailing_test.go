package extension_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
)

// Held-out acceptance tests for linkify's trailing-character trimming: punctuation that is
// exposed after an unmatched ')' is removed must also be excluded from the autolink.

func benchLinkify(t *testing.T, src string) string {
	t.Helper()
	p := parser.New(parser.WithExtensions(extension.NewLinkifyParser()))
	r := html.New()
	source := []byte(src)
	doc := p.Parse(source)
	var buf bytes.Buffer
	if err := r.Render(&buf, source, doc); err != nil {
		t.Fatalf("render %q: %v", src, err)
	}
	return strings.TrimSpace(buf.String())
}

func TestBenchLinkifyPunctuationBeforeUnmatchedParen(t *testing.T) {
	cases := []struct{ in, want string }{
		{"see (https://example.net/docs.) now",
			`<p>see (<a href="https://example.net/docs">https://example.net/docs</a>.) now</p>`},
		{"(https://example.net/a/b!)",
			`<p>(<a href="https://example.net/a/b">https://example.net/a/b</a>!)</p>`},
		{"(www.example.org/page,)",
			`<p>(<a href="http://www.example.org/page">www.example.org/page</a>,)</p>`},
		{"(http://example.net/x?).",
			`<p>(<a href="http://example.net/x">http://example.net/x</a>?).</p>`},
		{"(https://example.net/x_(y).)",
			`<p>(<a href="https://example.net/x_(y)">https://example.net/x_(y)</a>.)</p>`},
	}
	for _, tc := range cases {
		if got := benchLinkify(t, tc.in); got != tc.want {
			t.Errorf("linkify %q\n got: %s\nwant: %s", tc.in, got, tc.want)
		}
	}
}

func TestBenchLinkifyExistingTrimmingUnchanged(t *testing.T) {
	cases := []struct{ in, want string }{
		{"visit https://example.net/path.",
			`<p>visit <a href="https://example.net/path">https://example.net/path</a>.</p>`},
		{"(https://example.net/path)",
			`<p>(<a href="https://example.net/path">https://example.net/path</a>)</p>`},
		{"https://example.net/wiki/Go_(language) rocks",
			`<p><a href="https://example.net/wiki/Go_(language)">https://example.net/wiki/Go_(language)</a> rocks</p>`},
		{"https://example.net/q?a=1&b=2",
			`<p><a href="https://example.net/q?a=1&amp;b=2">https://example.net/q?a=1&amp;b=2</a></p>`},
		{"https://example.net/search?q=x&amp;",
			`<p><a href="https://example.net/search?q=x">https://example.net/search?q=x</a>&amp;</p>`},
	}
	for _, tc := range cases {
		if got := benchLinkify(t, tc.in); got != tc.want {
			t.Errorf("linkify %q\n got: %s\nwant: %s", tc.in, got, tc.want)
		}
	}
}
