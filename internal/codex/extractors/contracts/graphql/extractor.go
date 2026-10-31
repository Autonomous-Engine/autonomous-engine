package graphql

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

// Extractor detects GraphQL schema definitions (producers) and client
// usage patterns (consumers) such as useMutation/useQuery calls.
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.graphql" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"**/*.graphql", "**/*.gql", "**/*.ts", "**/*.tsx",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Regex patterns for producer extraction from .graphql/.gql files.
var (
	// Matches "type Query {", "type Mutation {", "type Subscription {"
	rootTypeRe = regexp.MustCompile(`(?i)^type\s+(Query|Mutation|Subscription)\s*\{`)
	// Matches field definitions like "  users: [User!]!" or "  createUser(input: CreateUserInput!): User!"
	fieldRe = regexp.MustCompile(`^\s+(\w+)\s*[\(:]`)
)

// Consumer patterns for .ts/.tsx/.js files.
var (
	hookCallRe  = regexp.MustCompile(`(?:useMutation|useQuery|useSubscription|useLazyQuery)\s*\(\s*['"](\w+)['"]`)
	gqlTagRe    = regexp.MustCompile("gql\\s*`\\s*(?:mutation|query|subscription)\\s+(\\w+)")
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
		ext := filepath.Ext(d.Name())
		if ext != ".graphql" && ext != ".gql" {
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
		var currentType string
		braceDepth := 0

		for scanner.Scan() {
			lineNum++
			line := scanner.Text()

			if m := rootTypeRe.FindStringSubmatch(line); m != nil {
				currentType = strings.ToLower(m[1])
				braceDepth = 1
				continue
			}

			if currentType != "" {
				braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
				if braceDepth <= 0 {
					currentType = ""
					continue
				}
				if fm := fieldRe.FindStringSubmatch(line); fm != nil {
					producers = append(producers, &framework.ProducerContract{
						Type: "graphql." + currentType,
						Name: fm[1],
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
		if ext != ".ts" && ext != ".tsx" && ext != ".js" {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		content := string(data)
		lines := strings.Split(content, "\n")

		for i, line := range lines {
			for _, m := range hookCallRe.FindAllStringSubmatch(line, -1) {
				if producerNames[m[1]] {
					edges = append(edges, ConsumerEdge{
						Producer: m[1],
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "hook_call",
					})
				}
			}
			for _, m := range gqlTagRe.FindAllStringSubmatch(line, -1) {
				if producerNames[m[1]] {
					edges = append(edges, ConsumerEdge{
						Producer: m[1],
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "gql_tag",
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
