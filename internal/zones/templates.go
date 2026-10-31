package zones

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/autonomousengine/autonomous-engine/configs"
)

// Template is a zone to create: a default from codex-config.yaml or a
// proposal derived from the onboarding risk_zones answer.
type Template struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Patterns    []string `json:"patterns"`
	Policy      Policy   `json:"policy"`
	Source      Source   `json:"source"`
}

// DefaultTemplates are the zones block of the embedded
// configs/codex-config.yaml: requires_human_review false → autonomous,
// true → human_review, escalate_immediately → escalate. The shipped file
// has two repository-agnostic zones, guarded and restricted; there is no
// safe zone (a zone needs a pattern, and unmatched files are autonomous).
// The rest of the file is not loaded.
func DefaultTemplates() ([]Template, error) {
	return parseZonesYAML(configs.CodexConfig)
}

// parseZonesYAML reads the top-level "zones:" sequence of a codex-config
// file. It implements only the YAML subset that block uses — a sequence of
// mappings whose values are plain or quoted scalars, folded (">") or
// literal ("|") block scalars, block sequences of scalars and flow
// sequences ("[a, b]"), with "#" comments — so the engine needs no YAML
// dependency. Anything else in the block is an error (the shipped file is
// parsed by a test, so an edit outside the subset fails CI, not a
// running service).
func parseZonesYAML(src []byte) ([]Template, error) {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimRight(stripComment(l), " \t") == "zones:" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("codex config: no top-level zones: block")
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(stripComment(l)) == "" {
			continue
		}
		if l[0] != ' ' && l[0] != '\t' && !strings.HasPrefix(l, "-") {
			end = i
			break
		}
	}
	block := lines[start:end]

	type raw struct {
		fields map[string]bool
		t      Template
		human  bool
		esc    bool
	}
	var items []*raw
	var cur *raw
	for i := 0; i < len(block); i++ {
		line := stripComment(block[i])
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		body := strings.TrimSpace(line)
		if strings.HasPrefix(body, "- ") || body == "-" {
			cur = &raw{fields: map[string]bool{}, t: Template{Source: SourceConfig}}
			items = append(items, cur)
			body = strings.TrimSpace(strings.TrimPrefix(body, "-"))
			indent += 2
			if body == "" {
				continue
			}
		}
		if cur == nil {
			return nil, fmt.Errorf("codex config: zones line %q is not inside a zone", strings.TrimSpace(block[i]))
		}
		key, value, ok := strings.Cut(body, ":")
		if !ok {
			return nil, fmt.Errorf("codex config: zones line %q is not key: value", body)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if cur.fields[key] {
			return nil, fmt.Errorf("codex config: zone key %q repeated", key)
		}
		cur.fields[key] = true

		// Continuation lines: everything indented deeper than the key.
		var cont []string
		for i+1 < len(block) {
			next := block[i+1]
			if strings.TrimSpace(stripComment(next)) == "" {
				if value == ">" || value == "|" {
					cont = append(cont, "")
				}
				i++
				continue
			}
			ni := len(next) - len(strings.TrimLeft(next, " "))
			if ni <= indent {
				break
			}
			if value == ">" || value == "|" {
				cont = append(cont, strings.TrimSpace(next))
			} else {
				cont = append(cont, strings.TrimSpace(stripComment(next)))
			}
			i++
		}

		switch key {
		case "name", "description":
			s, err := scalarValue(value, cont)
			if err != nil {
				return nil, fmt.Errorf("codex config: zone %s: %w", key, err)
			}
			if key == "name" {
				cur.t.Name = s
			} else {
				cur.t.Description = s
			}
		case "patterns":
			ps, err := sequenceValue(value, cont)
			if err != nil {
				return nil, fmt.Errorf("codex config: zone patterns: %w", err)
			}
			cur.t.Patterns = ps
		case "requires_human_review", "escalate_immediately":
			if len(cont) > 0 {
				return nil, fmt.Errorf("codex config: zone %s must be true or false", key)
			}
			b, err := strconv.ParseBool(unquote(value))
			if err != nil {
				return nil, fmt.Errorf("codex config: zone %s: %w", key, err)
			}
			if key == "requires_human_review" {
				cur.human = b
			} else {
				cur.esc = b
			}
		default:
			return nil, fmt.Errorf("codex config: unknown zone key %q", key)
		}
	}

	out := make([]Template, 0, len(items))
	for _, it := range items {
		switch {
		case it.esc:
			it.t.Policy = PolicyEscalate
		case it.human:
			it.t.Policy = PolicyHumanReview
		default:
			it.t.Policy = PolicyAutonomous
		}
		it.t.Patterns = NormalizePatterns(it.t.Patterns)
		if err := Validate(it.t.Name, it.t.Patterns, it.t.Policy); err != nil {
			return nil, fmt.Errorf("codex config: %w", err)
		}
		out = append(out, it.t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("codex config: zones block is empty")
	}
	return out, nil
}

// stripComment removes a "#" comment that is not inside quotes and starts
// the line or follows whitespace.
func stripComment(l string) string {
	var quote byte
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || l[i-1] == ' ' || l[i-1] == '\t'):
			return strings.TrimRight(l[:i], " \t")
		}
	}
	return l
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		if s[0] == '"' {
			if u, err := strconv.Unquote(s); err == nil {
				return u
			}
		}
		return s[1 : len(s)-1]
	}
	return s
}

func scalarValue(value string, cont []string) (string, error) {
	switch value {
	case ">", "|":
		sep := " "
		if value == "|" {
			sep = "\n"
		}
		var parts []string
		for _, c := range cont {
			if c != "" {
				parts = append(parts, c)
			}
		}
		return strings.Join(parts, sep), nil
	}
	if len(cont) > 0 {
		return "", fmt.Errorf("unexpected continuation of %q", value)
	}
	return unquote(value), nil
}

func sequenceValue(value string, cont []string) ([]string, error) {
	if strings.HasPrefix(value, "[") {
		if !strings.HasSuffix(value, "]") || len(cont) > 0 {
			return nil, fmt.Errorf("flow sequence %q must be on one line", value)
		}
		inner := strings.TrimSpace(value[1 : len(value)-1])
		var out []string
		if inner == "" {
			return out, nil
		}
		for _, p := range strings.Split(inner, ",") {
			out = append(out, unquote(p))
		}
		return out, nil
	}
	if value != "" {
		return nil, fmt.Errorf("want a sequence, got %q", value)
	}
	var out []string
	for _, c := range cont {
		if c == "" {
			continue
		}
		if !strings.HasPrefix(c, "- ") && c != "-" {
			return nil, fmt.Errorf("sequence item %q does not start with -", c)
		}
		out = append(out, unquote(strings.TrimPrefix(c, "-")))
	}
	return out, nil
}
