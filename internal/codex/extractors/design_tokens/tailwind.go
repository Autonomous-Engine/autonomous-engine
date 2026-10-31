package design_tokens

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

type TailwindExtractor struct{}

func NewTailwind() *TailwindExtractor { return &TailwindExtractor{} }

func init() {
	framework.Register(NewTailwind())
}

func (*TailwindExtractor) Name() string         { return "design_tokens.tailwind" }
func (*TailwindExtractor) Family() string       { return "design_tokens" }
func (*TailwindExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*TailwindExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{"tailwind.config.*"})
}

// TailwindTokens is the structured output for Tailwind design tokens.
type TailwindTokens struct {
	Colors     map[string]string `json:"colors,omitempty"`
	Spacing    map[string]string `json:"spacing,omitempty"`
	FontSize   map[string]string `json:"fontSize,omitempty"`
	FontFamily map[string]string `json:"fontFamily,omitempty"`
}

var tailwindConfigFiles = []string{
	"tailwind.config.js",
	"tailwind.config.ts",
	"tailwind.config.cjs",
	"tailwind.config.mjs",
}

func (e *TailwindExtractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	var configPath string
	var content []byte
	for _, name := range tailwindConfigFiles {
		p := filepath.Join(ec.RepoPath, name)
		data, err := os.ReadFile(p)
		if err == nil {
			configPath = name
			content = data
			break
		}
	}
	if configPath == "" {
		return nil, nil
	}

	tokens := TailwindTokens{
		Colors:     extractSection(string(content), "colors"),
		Spacing:    extractSection(string(content), "spacing"),
		FontSize:   extractSection(string(content), "fontSize"),
		FontFamily: extractSection(string(content), "fontFamily"),
	}

	payload, _ := json.Marshal(tokens)
	return []*framework.Blob{{
		Section:    "design_tokens",
		Payload:    payload,
		Confidence: 0.95,
		Evidence:   []framework.Evidence{{File: configPath, Note: "tailwind config parsed"}},
	}}, nil
}

func (e *TailwindExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

// kvRe matches a key-value pair like:  brand: '#3B82F6',  or  '18': '4.5rem',
var kvRe = regexp.MustCompile(`['"]?(\w[\w-]*)['"]?\s*:\s*['"]([^'"]+)['"]`)

// sectionStartRe matches the opening of a named section, e.g. "colors: {"
var sectionStartRe = regexp.MustCompile(`(\w+)\s*:\s*\{`)

func extractSection(content string, sectionName string) map[string]string {
	// Find the start of the named section, then extract the balanced brace body.
	for _, loc := range sectionStartRe.FindAllStringSubmatchIndex(content, -1) {
		name := content[loc[2]:loc[3]]
		if name != sectionName {
			continue
		}
		// loc[0] is start of full match; find the opening brace
		braceStart := strings.IndexByte(content[loc[0]:], '{')
		if braceStart < 0 {
			continue
		}
		body := extractBraceBody(content[loc[0]+braceStart:])
		if body == "" {
			continue
		}
		result := make(map[string]string)
		kvs := kvRe.FindAllStringSubmatch(body, -1)
		for _, kv := range kvs {
			result[kv[1]] = kv[2]
		}
		if len(result) > 0 {
			return result
		}
	}
	return nil
}

// extractBraceBody returns the content between a matched pair of braces.
// Input must start with '{'.
func extractBraceBody(s string) string {
	if len(s) == 0 || s[0] != '{' {
		return ""
	}
	depth := 0
	for i, ch := range s {
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i]
			}
		}
	}
	return ""
}
