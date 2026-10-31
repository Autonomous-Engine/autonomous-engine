package rest_openapi

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

// Extractor detects REST API route registrations (producers) and
// fetch/axios consumer calls.
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.rest_openapi" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"openapi.*", "**/*.go", "**/*.ts", "**/*.js", "routes/**/*", "api/**/*",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Producer patterns for route registration.
var (
	// Go: mux.HandleFunc("/api/...", handler) or r.GET("/api/...", ...)
	goRouteRe = regexp.MustCompile(`(?:HandleFunc|Handle|Get|Post|Put|Delete|Patch|GET|POST|PUT|DELETE|PATCH)\s*\(\s*"(/api/[^"]*)"`)
	// JS/TS: app.get('/api/...', ...) or router.post('/api/...', ...)
	jsRouteRe = regexp.MustCompile(`(?:app|router|server)\s*\.\s*(?:get|post|put|delete|patch|options|head|all)\s*\(\s*['"](/api/[^'"]*)['"]\s*`)
)

// Consumer patterns for API calls.
var (
	fetchRe = regexp.MustCompile(`fetch\s*\(\s*['"]([^'"]*?/api/[^'"]*)['"]\s*`)
	axiosRe = regexp.MustCompile(`axios\s*\.\s*(?:get|post|put|delete|patch|head|options|request)\s*\(\s*['"]([^'"]*?/api/[^'"]*)['"]\s*`)
)

func (e *Extractor) ProducerExtract(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.ProducerContract, error) {
	var producers []*framework.ProducerContract

	goExts := map[string]bool{".go": true}
	jsExts := map[string]bool{".ts": true, ".tsx": true, ".js": true, ".jsx": true}

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

		ext := filepath.Ext(d.Name())

		var re *regexp.Regexp
		if goExts[ext] {
			re = goRouteRe
		} else if jsExts[ext] {
			re = jsRouteRe
		} else {
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
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				method := extractMethod(line, ext)
				producers = append(producers, &framework.ProducerContract{
					Type: "rest.route",
					Name: method + " " + m[1],
					File: rel,
					Line: lineNum,
					Signature: map[string]any{
						"method": method,
						"path":   m[1],
					},
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

// extractMethod attempts to determine the HTTP method from the line context.
func extractMethod(line, ext string) string {
	upper := strings.ToUpper(line)
	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD"}
	for _, m := range methods {
		if strings.Contains(upper, "."+m+"(") || strings.Contains(upper, "."+m+"FUNC(") ||
			strings.Contains(upper, m+"(") {
			return m
		}
	}
	if strings.Contains(upper, "HANDLEFUNC") || strings.Contains(upper, "HANDLE(") {
		return "ANY"
	}
	return "ANY"
}

func (e *Extractor) ConsumerScan(ctx context.Context, ec *framework.ExtractionContext, producers []*framework.ProducerContract) ([]*framework.Blob, error) {
	producerPaths := map[string]bool{}
	for _, p := range producers {
		if sig, ok := p.Signature["path"].(string); ok {
			producerPaths[sig] = true
		}
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
		ext := filepath.Ext(d.Name())
		if ext != ".ts" && ext != ".tsx" && ext != ".js" && ext != ".jsx" {
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
			for _, m := range fetchRe.FindAllStringSubmatch(line, -1) {
				if producerPaths[m[1]] {
					edges = append(edges, ConsumerEdge{
						Producer: m[1],
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "fetch",
					})
				}
			}
			for _, m := range axiosRe.FindAllStringSubmatch(line, -1) {
				if producerPaths[m[1]] {
					edges = append(edges, ConsumerEdge{
						Producer: m[1],
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "axios",
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
