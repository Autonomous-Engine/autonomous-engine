package css_methodology

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() {
	framework.Register(New())
}

func (*Extractor) Name() string         { return "css_methodology" }
func (*Extractor) Family() string       { return "design_system" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{"**/*.css", "**/*.scss", "**/*.module.css", "package.json"})
}

// MethodologyOutput describes the detected CSS methodology.
type MethodologyOutput struct {
	Primary    string   `json:"primary"`
	Secondary  string   `json:"secondary,omitempty"`
	Indicators []string `json:"indicators"`
}

// Known CSS-in-JS / utility framework package names.
var cssDeps = map[string]string{
	"tailwindcss":        "tailwind",
	"styled-components":  "styled-components",
	"@emotion/styled":    "emotion",
	"@emotion/react":     "emotion",
	"@stitches/react":    "stitches",
	"vanilla-extract":    "vanilla-extract",
	"linaria":            "linaria",
}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	var indicators []string

	// Count file extensions
	moduleCSSCount := 0
	bareCSSCount := 0
	stylesTS := 0

	_ = filepath.Walk(ec.RepoPath, func(path string, info os.FileInfo, err error) error {
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
		name := info.Name()
		if strings.HasSuffix(name, ".module.css") || strings.HasSuffix(name, ".module.scss") {
			moduleCSSCount++
		} else if strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".scss") {
			bareCSSCount++
		}
		if strings.HasSuffix(name, ".styles.ts") || strings.HasSuffix(name, ".styles.tsx") {
			stylesTS++
		}
		return nil
	})

	if moduleCSSCount > 0 {
		indicators = append(indicators, "css_modules")
	}
	if bareCSSCount > 0 {
		indicators = append(indicators, "plain_css")
	}
	if stylesTS > 0 {
		indicators = append(indicators, "css_in_js_files")
	}

	// Check package.json for CSS framework deps
	var depIndicators []string
	pkgPath := filepath.Join(ec.RepoPath, "package.json")
	if data, err := os.ReadFile(pkgPath); err == nil {
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			allDeps := mergeMaps(pkg.Dependencies, pkg.DevDependencies)
			for dep, label := range cssDeps {
				if _, ok := allDeps[dep]; ok {
					depIndicators = append(depIndicators, label)
					indicators = append(indicators, label)
				}
			}
		}
	}

	if len(indicators) == 0 {
		return nil, nil
	}

	// Determine primary and secondary
	primary, secondary := classifyMethodology(depIndicators, moduleCSSCount, bareCSSCount, stylesTS)

	out := MethodologyOutput{
		Primary:    primary,
		Secondary:  secondary,
		Indicators: indicators,
	}

	payload, _ := json.Marshal(out)
	return []*framework.Blob{{
		Section:    "css_methodology",
		Payload:    payload,
		Confidence: 0.90,
		Evidence:   []framework.Evidence{{File: ".", Note: "file extension and dependency analysis"}},
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

func classifyMethodology(depIndicators []string, moduleCSS, bareCSS, stylesTS int) (primary, secondary string) {
	hasDep := map[string]bool{}
	for _, d := range depIndicators {
		hasDep[d] = true
	}

	// Priority: explicit framework deps > file-based signals
	switch {
	case hasDep["tailwind"]:
		primary = "tailwind"
	case hasDep["styled-components"]:
		primary = "styled-components"
	case hasDep["emotion"]:
		primary = "emotion"
	case hasDep["stitches"]:
		primary = "stitches"
	case hasDep["vanilla-extract"]:
		primary = "vanilla-extract"
	case stylesTS > 0:
		primary = "css_in_js"
	case moduleCSS > 0:
		primary = "css_modules"
	case bareCSS > 0:
		primary = "plain_css"
	default:
		primary = "unknown"
	}

	// Secondary: strongest file-based signal that differs from primary
	switch {
	case moduleCSS > 0 && primary != "css_modules":
		secondary = "css_modules"
	case stylesTS > 0 && primary != "css_in_js":
		secondary = "css_in_js"
	case bareCSS > 0 && primary != "plain_css" && primary != "tailwind":
		secondary = "plain_css"
	}

	return primary, secondary
}

func mergeMaps(a, b map[string]string) map[string]string {
	m := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}
