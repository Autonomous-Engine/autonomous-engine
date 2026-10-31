package ts_exports

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

// Extractor detects TypeScript/JavaScript exports from index files (producers)
// and import statements that consume them (consumers).
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.ts_exports" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"**/index.ts", "**/index.tsx", "package.json", "tsconfig.json",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Export declaration patterns.
var (
	// export { Foo, Bar, Baz }
	exportBracesRe = regexp.MustCompile(`export\s*\{([^}]+)\}`)
	// export function Foo(
	exportFuncRe = regexp.MustCompile(`export\s+(?:async\s+)?function\s+(\w+)`)
	// export class Foo
	exportClassRe = regexp.MustCompile(`export\s+class\s+(\w+)`)
	// export const Foo =
	exportConstRe = regexp.MustCompile(`export\s+const\s+(\w+)`)
	// export type Foo =
	exportTypeRe = regexp.MustCompile(`export\s+type\s+(\w+)`)
	// export interface Foo
	exportInterfaceRe = regexp.MustCompile(`export\s+interface\s+(\w+)`)
	// export default
	exportDefaultRe = regexp.MustCompile(`export\s+default\s+(?:class|function)?\s*(\w+)?`)
)

// Consumer pattern: import { X } from '...'
var importRe = regexp.MustCompile(`import\s+\{([^}]+)\}\s+from\s+['"]([^'"]+)['"]`)

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
		if name != "index.ts" && name != "index.tsx" {
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

			// export { Foo, Bar }
			if m := exportBracesRe.FindStringSubmatch(line); m != nil {
				names := strings.Split(m[1], ",")
				for _, n := range names {
					n = strings.TrimSpace(n)
					// Handle "Foo as Bar" renames
					if parts := strings.Fields(n); len(parts) >= 3 && parts[1] == "as" {
						n = parts[2]
					} else if len(parts) > 0 {
						n = parts[0]
					}
					if n != "" {
						producers = append(producers, &framework.ProducerContract{
							Type: "ts.export",
							Name: n,
							File: rel,
							Line: lineNum,
						})
					}
				}
				continue
			}

			// Single export declarations
			for _, re := range []*regexp.Regexp{exportFuncRe, exportClassRe, exportConstRe, exportTypeRe, exportInterfaceRe} {
				if m := re.FindStringSubmatch(line); m != nil {
					producers = append(producers, &framework.ProducerContract{
						Type: "ts.export",
						Name: m[1],
						File: rel,
						Line: lineNum,
					})
					break
				}
			}

			// export default
			if m := exportDefaultRe.FindStringSubmatch(line); m != nil && m[1] != "" {
				producers = append(producers, &framework.ProducerContract{
					Type: "ts.export.default",
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
	producerNames := map[string]bool{}
	for _, p := range producers {
		producerNames[p.Name] = true
	}

	type ConsumerEdge struct {
		Producer string `json:"producer"`
		Consumer string `json:"consumer_file"`
		Line     int    `json:"line"`
		Pattern  string `json:"pattern"`
	}

	var edges []ConsumerEdge

	exts := map[string]bool{".ts": true, ".tsx": true, ".js": true, ".jsx": true}

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
		ext := filepath.Ext(d.Name())
		if !exts[ext] {
			return nil
		}
		// Skip index files (they are producers, not consumers of themselves).
		name := d.Name()
		if name == "index.ts" || name == "index.tsx" {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")

		for i, line := range lines {
			for _, m := range importRe.FindAllStringSubmatch(line, -1) {
				imported := strings.Split(m[1], ",")
				for _, imp := range imported {
					imp = strings.TrimSpace(imp)
					if parts := strings.Fields(imp); len(parts) >= 3 && parts[1] == "as" {
						imp = parts[0]
					} else if len(parts) > 0 {
						imp = parts[0]
					}
					if producerNames[imp] {
						edges = append(edges, ConsumerEdge{
							Producer: imp,
							Consumer: rel,
							Line:     i + 1,
							Pattern:  "import",
						})
					}
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
		Confidence: 0.90,
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
