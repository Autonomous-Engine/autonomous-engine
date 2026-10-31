package python_exports

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

// Extractor detects Python module exports (producers) via __all__, top-level
// class/def declarations, and .pyi stubs, then finds import statements that
// consume them.
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.python_exports" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"**/*.py", "**/*.pyi",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Producer patterns.
var (
	// __all__ = ['Foo', 'bar', "Baz"]
	allRe = regexp.MustCompile(`^__all__\s*=\s*\[([^\]]+)\]`)
	// top-level class Foo: or class Foo(Base):
	classRe = regexp.MustCompile(`^class\s+([A-Za-z_]\w*)\s*[:\(]`)
	// top-level def bar( — exported if no leading underscore
	defRe = regexp.MustCompile(`^def\s+([A-Za-z_]\w*)\s*\(`)
)

// Consumer patterns.
var (
	// import pkg  or  import pkg.sub
	importModuleRe = regexp.MustCompile(`^import\s+([\w.]+)`)
	// from pkg import X  or  from pkg.sub import Y
	fromImportRe = regexp.MustCompile(`^from\s+([\w.]+)\s+import\s+(.+)`)
)

func (e *Extractor) ProducerExtract(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.ProducerContract, error) {
	var producers []*framework.ProducerContract

	err := filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" || base == "__pycache__" || base == ".venv" || base == "venv" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(d.Name())
		if ext != ".py" && ext != ".pyi" {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		f, ferr := os.Open(path)
		if ferr != nil {
			return nil
		}
		defer f.Close()

		isInit := d.Name() == "__init__.py" || d.Name() == "__init__.pyi"
		isStub := ext == ".pyi"

		scanner := bufio.NewScanner(f)
		lineNum := 0

		for scanner.Scan() {
			lineNum++
			line := scanner.Text()

			// __all__ declarations (typically in __init__.py)
			if isInit || isStub {
				if m := allRe.FindStringSubmatch(line); m != nil {
					// Parse the list entries: 'Foo', "bar"
					entries := m[1]
					for _, part := range strings.Split(entries, ",") {
						part = strings.TrimSpace(part)
						part = strings.Trim(part, `"'`)
						part = strings.TrimSpace(part)
						if part != "" {
							producers = append(producers, &framework.ProducerContract{
								Type: "python.export",
								Name: part,
								File: rel,
								Line: lineNum,
							})
						}
					}
					continue
				}
			}

			// Top-level class declarations (no leading whitespace)
			if m := classRe.FindStringSubmatch(line); m != nil {
				name := m[1]
				// Skip private classes
				if !strings.HasPrefix(name, "_") {
					typ := "python.class"
					if isStub {
						typ = "python.stub.class"
					}
					producers = append(producers, &framework.ProducerContract{
						Type: typ,
						Name: name,
						File: rel,
						Line: lineNum,
					})
				}
				continue
			}

			// Top-level function declarations (no leading whitespace)
			if m := defRe.FindStringSubmatch(line); m != nil {
				name := m[1]
				// Skip private functions
				if !strings.HasPrefix(name, "_") {
					typ := "python.func"
					if isStub {
						typ = "python.stub.func"
					}
					producers = append(producers, &framework.ProducerContract{
						Type: typ,
						Name: name,
						File: rel,
						Line: lineNum,
					})
				}
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

	// Collect the package dir names that contain producers.
	producerPkgDirs := map[string]bool{}
	for _, p := range producers {
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
			if base == "node_modules" || base == ".git" || base == "vendor" || base == "__pycache__" || base == ".venv" || base == "venv" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(d.Name())
		if ext != ".py" && ext != ".pyi" {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		// Skip files in producer directories
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
			trimmed := strings.TrimSpace(line)

			// from pkg import X, Y
			if m := fromImportRe.FindStringSubmatch(trimmed); m != nil {
				imported := strings.Split(m[2], ",")
				for _, imp := range imported {
					imp = strings.TrimSpace(imp)
					// Handle "X as Y" — the producer name is X
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
							Pattern:  "from-import",
						})
					}
				}
				continue
			}

			// import pkg
			if m := importModuleRe.FindStringSubmatch(trimmed); m != nil {
				modName := m[1]
				// Check if the full module or last part matches a producer
				parts := strings.Split(modName, ".")
				for _, part := range parts {
					if producerNames[part] {
						edges = append(edges, ConsumerEdge{
							Producer: part,
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
