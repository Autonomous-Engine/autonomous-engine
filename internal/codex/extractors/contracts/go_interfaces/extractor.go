package go_interfaces

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// Extractor detects exported Go interfaces, structs, and functions (producers)
// and finds consumer code that imports and uses them.
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.go_interfaces" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"**/*.go", "go.mod",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Regex patterns for exported Go declarations.
var (
	interfaceRe = regexp.MustCompile(`^type\s+([A-Z]\w*)\s+interface\s*\{`)
	structRe    = regexp.MustCompile(`^type\s+([A-Z]\w*)\s+struct\s*\{`)
	funcRe      = regexp.MustCompile(`^func\s+([A-Z]\w*)\s*\(`)
)

// Consumer pattern: import "module/path" or import alias "module/path"
var goImportRe = regexp.MustCompile(`^\s*(?:\w+\s+)?"([^"]+)"`)

func (e *Extractor) ProducerExtract(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.ProducerContract, error) {
	var producers []*framework.ProducerContract

	err := filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".go" {
			return nil
		}
		// Skip test files for producer detection.
		if strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		f, ferr := os.Open(path)
		if ferr != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineNum := 0

		for scanner.Scan() {
			lineNum++
			line := scanner.Text()

			if m := interfaceRe.FindStringSubmatch(line); m != nil {
				producers = append(producers, &framework.ProducerContract{
					Type: "go.interface",
					Name: m[1],
					File: rel,
					Line: lineNum,
				})
				continue
			}

			if m := structRe.FindStringSubmatch(line); m != nil {
				producers = append(producers, &framework.ProducerContract{
					Type: "go.struct",
					Name: m[1],
					File: rel,
					Line: lineNum,
				})
				continue
			}

			if m := funcRe.FindStringSubmatch(line); m != nil {
				producers = append(producers, &framework.ProducerContract{
					Type: "go.func",
					Name: m[1],
					File: rel,
					Line: lineNum,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return producers, nil
}

func (e *Extractor) ConsumerScan(ctx context.Context, ec *framework.ExtractionContext, producers []*framework.ProducerContract) ([]*framework.Blob, error) {
	// Build sets: exported names, and the package paths that define them.
	producerNames := map[string]bool{}
	producerPkgDirs := map[string]bool{} // relative dirs that contain producers
	for _, p := range producers {
		producerNames[p.Name] = true
		producerPkgDirs[filepath.ToSlash(filepath.Dir(p.File))] = true
	}

	type ConsumerEdge struct {
		Producer string `json:"producer"`
		Consumer string `json:"consumer_file"`
		Line     int    `json:"line"`
		Pattern  string `json:"pattern"`
	}

	var edges []ConsumerEdge

	_ = filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".go" {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		// Skip files in the same package directories as producers (self-reference).
		fileDir := filepath.ToSlash(filepath.Dir(rel))
		if producerPkgDirs[fileDir] {
			return nil
		}

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")

		for i, line := range lines {
			// Look for usage of exported names as identifiers.
			for name := range producerNames {
				if containsIdentifier(line, name) {
					edges = append(edges, ConsumerEdge{
						Producer: name,
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "usage",
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

// containsIdentifier checks if a line contains the given identifier as a
// standalone word (not a substring of a larger identifier).
func containsIdentifier(line, ident string) bool {
	idx := 0
	for {
		pos := strings.Index(line[idx:], ident)
		if pos < 0 {
			return false
		}
		pos += idx

		// Check boundary before.
		if pos > 0 {
			r := rune(line[pos-1])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				idx = pos + len(ident)
				continue
			}
		}

		// Check boundary after.
		end := pos + len(ident)
		if end < len(line) {
			r := rune(line[end])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				idx = pos + len(ident)
				continue
			}
		}

		return true
	}
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
