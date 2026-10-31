package design_tokens

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

type CSSVarsExtractor struct{}

func NewCSSVars() *CSSVarsExtractor { return &CSSVarsExtractor{} }

func init() {
	framework.Register(NewCSSVars())
}

func (*CSSVarsExtractor) Name() string         { return "design_tokens.css_vars" }
func (*CSSVarsExtractor) Family() string       { return "design_tokens" }
func (*CSSVarsExtractor) Kind() framework.Kind { return framework.KindStatic }

func (*CSSVarsExtractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{"**/*.css", "**/*.scss"})
}

// CSSVar represents a single CSS custom property declaration.
type CSSVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

var cssVarRe = regexp.MustCompile(`^\s*(--[\w-]+)\s*:\s*(.+?)\s*;\s*$`)

func (e *CSSVarsExtractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	var vars []CSSVar
	var evidence []framework.Evidence

	err := filepath.Walk(ec.RepoPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := info.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".css" && ext != ".scss" {
			return nil
		}

		relPath, _ := filepath.Rel(ec.RepoPath, path)
		relPath = filepath.ToSlash(relPath)

		found := scanFileForVars(path, relPath)
		vars = append(vars, found...)
		if len(found) > 0 {
			evidence = append(evidence, framework.Evidence{
				File: relPath,
				Note: "CSS custom properties found",
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(vars) == 0 {
		return nil, nil
	}

	payload, _ := json.Marshal(vars)
	return []*framework.Blob{{
		Section:    "design_tokens_css_vars",
		Payload:    payload,
		Confidence: 0.95,
		Evidence:   evidence,
	}}, nil
}

func (e *CSSVarsExtractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

func scanFileForVars(absPath, relPath string) []CSSVar {
	f, err := os.Open(absPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var vars []CSSVar
	inRoot := false
	braceDepth := 0
	lineNum := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		if strings.Contains(line, ":root") {
			inRoot = true
		}

		if inRoot {
			braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
			if braceDepth <= 0 {
				inRoot = false
				braceDepth = 0
			}

			if m := cssVarRe.FindStringSubmatch(line); m != nil {
				vars = append(vars, CSSVar{
					Name:  m[1],
					Value: m[2],
					File:  relPath,
					Line:  lineNum,
				})
			}
		}
	}
	return vars
}
