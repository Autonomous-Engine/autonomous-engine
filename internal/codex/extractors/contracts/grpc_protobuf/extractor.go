package grpc_protobuf

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

// Extractor detects gRPC service definitions from .proto files (producers)
// and usage of generated stubs in Go/TS/Python code (consumers).
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.grpc_protobuf" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"**/*.proto", "**/*.go", "**/*.ts", "**/*.py",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Regex patterns for .proto file parsing.
var (
	serviceRe = regexp.MustCompile(`^\s*service\s+(\w+)\s*\{`)
	rpcRe     = regexp.MustCompile(`^\s*rpc\s+(\w+)\s*\(`)
)

// Consumer patterns for generated stub usage.
var (
	// Go: pb "some/package/proto" or import of pb package
	goPBImportRe = regexp.MustCompile(`(?:pb|proto)\s+"([^"]+)"`)
	// Method calls on service clients: .MethodName(
	methodCallRe = regexp.MustCompile(`\.(\w+)\s*\(`)
)

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
		if filepath.Ext(d.Name()) != ".proto" {
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
		var currentService string
		braceDepth := 0

		for scanner.Scan() {
			lineNum++
			line := scanner.Text()

			if m := serviceRe.FindStringSubmatch(line); m != nil {
				currentService = m[1]
				braceDepth = 1
				producers = append(producers, &framework.ProducerContract{
					Type: "grpc.service",
					Name: currentService,
					File: rel,
					Line: lineNum,
				})
				continue
			}

			if currentService != "" {
				braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
				if braceDepth <= 0 {
					currentService = ""
					continue
				}
				if m := rpcRe.FindStringSubmatch(line); m != nil {
					producers = append(producers, &framework.ProducerContract{
						Type: "grpc.rpc",
						Name: currentService + "." + m[1],
						File: rel,
						Line: lineNum,
						Signature: map[string]any{
							"service": currentService,
							"method":  m[1],
						},
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
	// Build a set of RPC method names for matching.
	rpcMethods := map[string]string{} // method -> full name
	for _, p := range producers {
		if p.Type == "grpc.rpc" {
			if sig, ok := p.Signature["method"].(string); ok {
				rpcMethods[sig] = p.Name
			}
		}
	}

	type ConsumerEdge struct {
		Producer string `json:"producer"`
		Consumer string `json:"consumer_file"`
		Line     int    `json:"line"`
		Pattern  string `json:"pattern"`
	}

	var edges []ConsumerEdge

	exts := map[string]bool{".go": true, ".ts": true, ".py": true}

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
		if !exts[ext] {
			return nil
		}
		// Skip .proto files themselves.
		if ext == ".proto" {
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
			for _, m := range methodCallRe.FindAllStringSubmatch(line, -1) {
				if fullName, ok := rpcMethods[m[1]]; ok {
					edges = append(edges, ConsumerEdge{
						Producer: fullName,
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "method_call",
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
