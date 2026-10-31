package component_library

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

func (*Extractor) Name() string         { return "component_library" }
func (*Extractor) Family() string       { return "design_system" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched([]string{"package.json", "components.json"})
}

// LibraryInfo describes the detected component library stack.
type LibraryInfo struct {
	Libraries  []DetectedLibrary `json:"libraries"`
	HasShadcn  bool              `json:"has_shadcn"`
	HasCVA     bool              `json:"has_cva"`
	StackLabel string            `json:"stack_label"`
}

// DetectedLibrary represents a single detected component library dependency.
type DetectedLibrary struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	Version string `json:"version,omitempty"`
}

// knownLibraries maps dependency prefixes/names to library labels.
var knownLibraries = map[string]string{
	"@radix-ui/":              "radix-ui",
	"@mui/material":           "mui",
	"@mui/joy":                "mui",
	"@chakra-ui/react":        "chakra",
	"@headlessui/react":       "headlessui",
	"@headlessui/vue":         "headlessui",
	"@mantine/core":           "mantine",
	"antd":                    "ant-design",
	"react-bootstrap":         "react-bootstrap",
	"@nextui-org/react":       "nextui",
	"@ariakit/react":          "ariakit",
}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	pkgPath := filepath.Join(ec.RepoPath, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return nil, nil // no package.json, nothing to do
	}

	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, nil
	}

	allDeps := mergeMaps(pkg.Dependencies, pkg.DevDependencies)

	info := LibraryInfo{}

	// Check for known libraries
	seen := map[string]bool{}
	for dep, version := range allDeps {
		for prefix, label := range knownLibraries {
			if dep == prefix || strings.HasPrefix(dep, prefix) {
				if !seen[label] {
					seen[label] = true
					info.Libraries = append(info.Libraries, DetectedLibrary{
						Name:    label,
						Package: dep,
						Version: version,
					})
				}
			}
		}
	}

	// Check for CVA (class-variance-authority)
	if _, ok := allDeps["class-variance-authority"]; ok {
		info.HasCVA = true
	}

	// Check for shadcn indicators
	componentsPath := filepath.Join(ec.RepoPath, "components.json")
	if _, err := os.Stat(componentsPath); err == nil {
		info.HasShadcn = true
	}
	// Also check for tailwind-merge + CVA + radix combo as shadcn indicator
	_, hasTWMerge := allDeps["tailwind-merge"]
	if info.HasCVA && hasTWMerge && seen["radix-ui"] {
		info.HasShadcn = true
	}

	// Build stack label
	info.StackLabel = buildStackLabel(info, seen)

	if len(info.Libraries) == 0 && !info.HasShadcn && !info.HasCVA {
		return nil, nil
	}

	payload, _ := json.Marshal(info)
	evidence := []framework.Evidence{{File: "package.json", Note: "dependency analysis"}}
	if info.HasShadcn {
		evidence = append(evidence, framework.Evidence{File: "components.json", Note: "shadcn indicator"})
	}

	return []*framework.Blob{{
		Section:    "component_library",
		Payload:    payload,
		Confidence: 0.95,
		Evidence:   evidence,
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}

func buildStackLabel(info LibraryInfo, seen map[string]bool) string {
	if info.HasShadcn || (info.HasCVA && seen["radix-ui"]) {
		return "shadcn/ui (radix + cva)"
	}
	if seen["radix-ui"] {
		return "radix-ui"
	}
	if seen["mui"] {
		return "material-ui"
	}
	if seen["chakra"] {
		return "chakra-ui"
	}
	if seen["headlessui"] {
		return "headless-ui"
	}
	if seen["mantine"] {
		return "mantine"
	}
	if seen["ant-design"] {
		return "ant-design"
	}
	// fallback: first library found
	if len(info.Libraries) > 0 {
		return info.Libraries[0].Name
	}
	return "unknown"
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
