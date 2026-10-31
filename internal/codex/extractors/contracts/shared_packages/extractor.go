package shared_packages

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

// Extractor detects workspace/monorepo shared packages (producers) declared in
// package.json workspaces, go.mod replace directives, or Cargo.toml workspace
// members, then finds dependency references to those packages (consumers).
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.shared_packages" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"package.json", "go.mod", "Cargo.toml", "pom.xml",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Patterns for workspace declarations.
var (
	// npm: "workspaces": ["packages/*", ...] — we look for the field.
	npmWorkspacesRe = regexp.MustCompile(`"workspaces"\s*:\s*\[`)
	// npm: "name": "@scope/pkg"
	npmNameRe = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)
	// Go: replace module/path => ./local/path
	goReplaceRe = regexp.MustCompile(`replace\s+([\w./\-]+)\s+=>\s+(\..+)`)
	// Cargo: members = ["crates/*", ...]
	cargoMembersRe = regexp.MustCompile(`members\s*=\s*\[([^\]]+)\]`)
)

// Consumer patterns: dependency references.
var (
	// npm: "@scope/pkg": "workspace:*" or "@scope/pkg": "^1.0"
	npmDepRe = regexp.MustCompile(`"([^"]+)"\s*:\s*"(?:workspace:[^"]*|[^"]*)"`)
)

func (e *Extractor) ProducerExtract(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.ProducerContract, error) {
	var producers []*framework.ProducerContract

	err := filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" || base == "dist" || base == "build" {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		switch name {
		case "package.json":
			prods := extractNpmWorkspacePackages(ec.RepoPath, path, rel)
			producers = append(producers, prods...)

		case "go.mod":
			prods := extractGoReplacePackages(path, rel)
			producers = append(producers, prods...)

		case "Cargo.toml":
			prods := extractCargoWorkspaceMembers(path, rel)
			producers = append(producers, prods...)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return producers, nil
}

// extractNpmWorkspacePackages finds workspace packages in a root package.json.
// If the file contains a "workspaces" field, it scans workspace paths for
// sub-package.json files and extracts their "name" field.
func extractNpmWorkspacePackages(repoPath, pkgPath, relPath string) []*framework.ProducerContract {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return nil
	}
	content := string(data)

	// Check if this package.json has a workspaces field.
	if !npmWorkspacesRe.MatchString(content) {
		return nil
	}

	// Parse the workspaces array — look for quoted strings in the array.
	wsPatternRe := regexp.MustCompile(`"workspaces"\s*:\s*\[([^\]]+)\]`)
	m := wsPatternRe.FindStringSubmatch(content)
	if m == nil {
		return nil
	}

	// Extract workspace glob patterns.
	var wsGlobs []string
	entryRe := regexp.MustCompile(`"([^"]+)"`)
	for _, match := range entryRe.FindAllStringSubmatch(m[1], -1) {
		wsGlobs = append(wsGlobs, match[1])
	}

	dir := filepath.Dir(pkgPath)
	var producers []*framework.ProducerContract

	// For each workspace glob, find matching package.json files.
	for _, glob := range wsGlobs {
		// Convert glob pattern: "packages/*" => look for packages/*/package.json
		pattern := filepath.Join(dir, filepath.FromSlash(glob), "package.json")
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			pkgData, rerr := os.ReadFile(match)
			if rerr != nil {
				continue
			}
			if nm := npmNameRe.FindStringSubmatch(string(pkgData)); nm != nil {
				matchRel, _ := filepath.Rel(repoPath, match)
				matchRel = filepath.ToSlash(matchRel)
				producers = append(producers, &framework.ProducerContract{
					Type: "npm.workspace-package",
					Name: nm[1],
					File: matchRel,
					Line: 1,
				})
			}
		}
	}

	return producers
}

// extractGoReplacePackages finds local replace directives in go.mod.
func extractGoReplacePackages(modPath, relPath string) []*framework.ProducerContract {
	f, err := os.Open(modPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var producers []*framework.ProducerContract
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		if m := goReplaceRe.FindStringSubmatch(line); m != nil {
			producers = append(producers, &framework.ProducerContract{
				Type: "go.replace",
				Name: m[1],
				File: relPath,
				Line: lineNum,
				Signature: map[string]any{
					"local_path": strings.TrimSpace(m[2]),
				},
			})
		}
	}

	return producers
}

// extractCargoWorkspaceMembers finds workspace members in Cargo.toml.
func extractCargoWorkspaceMembers(cargoPath, relPath string) []*framework.ProducerContract {
	data, err := os.ReadFile(cargoPath)
	if err != nil {
		return nil
	}

	m := cargoMembersRe.FindStringSubmatch(string(data))
	if m == nil {
		return nil
	}

	var producers []*framework.ProducerContract
	entryRe := regexp.MustCompile(`"([^"]+)"`)
	for _, match := range entryRe.FindAllStringSubmatch(m[1], -1) {
		producers = append(producers, &framework.ProducerContract{
			Type: "cargo.workspace-member",
			Name: match[1],
			File: relPath,
			Line: 1,
		})
	}

	return producers
}

func (e *Extractor) ConsumerScan(ctx context.Context, ec *framework.ExtractionContext, producers []*framework.ProducerContract) ([]*framework.Blob, error) {
	producerNames := map[string]bool{}
	for _, p := range producers {
		producerNames[p.Name] = true
	}

	// Track producer files to skip self-references.
	producerFiles := map[string]bool{}
	for _, p := range producers {
		producerFiles[p.File] = true
	}

	type ConsumerEdge struct {
		Producer string `json:"producer"`
		Consumer string `json:"consumer_file"`
		Line     int    `json:"line"`
		Pattern  string `json:"pattern"`
	}

	var edges []ConsumerEdge

	manifests := map[string]bool{
		"package.json": true,
		"go.mod":       true,
		"Cargo.toml":   true,
		"pom.xml":      true,
	}

	_ = filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" || base == "dist" || base == "build" {
				return filepath.SkipDir
			}
			return nil
		}

		if !manifests[d.Name()] {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		// Skip producer files themselves.
		if producerFiles[rel] {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")

		for i, line := range lines {
			// Check for dependency references to known producer package names.
			for name := range producerNames {
				if strings.Contains(line, name) {
					pattern := "dependency"
					if strings.Contains(line, "workspace:") {
						pattern = "workspace-dependency"
					}
					edges = append(edges, ConsumerEdge{
						Producer: name,
						Consumer: rel,
						Line:     i + 1,
						Pattern:  pattern,
					})
				}
			}
		}
		return nil
	})

	if len(edges) == 0 {
		return nil, nil
	}

	payload, _ := json.Marshal(edges)
	return []*framework.Blob{{
		Section:    "contract_edges",
		Payload:    payload,
		Confidence: 0.85,
	}}, nil
}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	producers, err := e.ProducerExtract(ctx, ec)
	if err != nil {
		return nil, err
	}
	if len(producers) == 0 {
		return nil, nil
	}
	payload, _ := json.Marshal(producers)
	return []*framework.Blob{{
		Section:    "contract_edges",
		Payload:    payload,
		Confidence: 0.90,
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, prev []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}
