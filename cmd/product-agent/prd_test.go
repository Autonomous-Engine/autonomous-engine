package main

import (
	"os"
	"strings"
	"testing"
)

func TestStripPRDPreamble(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "chatter before the H1",
			in:   "After reviewing the PRD for consistency, I will output the PRD unchanged.\n\n# Radar toggle\n\n## Overview\nText.",
			want: "# Radar toggle\n\n## Overview\nText.",
		},
		{
			name: "chatter before an H2",
			in:   "Sure! Here is the PRD:\n## Overview\nText.\n## Goals\n- g",
			want: "## Overview\nText.\n## Goals\n- g",
		},
		{
			name: "fenced markdown after the chatter",
			in:   "Here you go:\n```markdown\n# Title\nBody\n```\nLet me know if you need changes.",
			want: "# Title\nBody",
		},
		{
			name: "already clean",
			in:   "# Title\nBody",
			want: "# Title\nBody",
		},
		{
			name: "no heading at all is kept as is",
			in:   "Overview: the map should toggle.\nGoals: fewer reloads.",
			want: "Overview: the map should toggle.\nGoals: fewer reloads.",
		},
		{
			name: "H3 alone is not a PRD start; hashtags are not headings",
			in:   "#notaheading and ### Sub\nBody",
			want: "#notaheading and ### Sub\nBody",
		},
		{
			name: "outer markdown fence with an inner code block keeps the whole body",
			in:   "Here you go:\n```markdown\n# Title\n## API\n```json\n{\"a\":1}\n```\n## Goals\n- g\n```",
			want: "# Title\n## API\n```json\n{\"a\":1}\n```\n## Goals\n- g",
		},
		{
			name: "a # comment inside a code fence is not a heading",
			in:   "This PRD covers setup.\n```bash\n# install deps\nnpm i\n```\n## Overview\nText.",
			want: "## Overview\nText.",
		},
		{
			name: "a non-markdown fence is never stripped as an outer wrapper",
			in:   "Intro.\n```\nnot a heading\n```\n# Title\nBody\n```\ncode\n```",
			want: "# Title\nBody\n```\ncode\n```",
		},
		{
			name: "CRLF input",
			in:   "Preamble line.\r\n\r\n# Title\r\nBody",
			want: "# Title\r\nBody",
		},
	}
	for _, c := range cases {
		if got := stripPRDPreamble(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// Source scan: the stored PRD is the stripped body.
func TestPRDArtifactStoresTheStrippedBody(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(s, "prdContent := stripPRDPreamble(prdResult.Content)") {
		t.Fatal("main.go does not strip the PRD preamble before storing it")
	}
}
