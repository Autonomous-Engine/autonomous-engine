package events

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// Extractor detects event/topic constant declarations (producers) and finds
// Publish/Subscribe/Emit calls that reference them (consumers).
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.events" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"**/*.go", "**/*.ts", "**/*.yaml", "**/*.yml",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Producer patterns: topic/event constant declarations.
var (
	// Go: const TopicFoo = "foo.bar"  or  TopicFoo string = "foo.bar"
	goTopicConstRe = regexp.MustCompile(`(?:const\s+|^\s*)(Topic\w+|Event\w+)\s+(?:string\s*)?=\s*"([^"]+)"`)
	// TS: export const TOPIC_FOO = "foo.bar"
	tsTopicConstRe = regexp.MustCompile(`(?:export\s+)?const\s+(TOPIC_\w+|EVENT_\w+)\s*=\s*['"]([^'"]+)['"]`)
	// YAML: topic: foo.bar  or  - topic: foo.bar
	yamlTopicRe = regexp.MustCompile(`(?:^|\s)topics?\s*:\s*['"]?([a-zA-Z][\w.]+)['"]?`)
)

// Consumer patterns: Publish/Subscribe/Emit calls.
var (
	// Publish(ctx, TopicFoo, ...) or Subscribe(TopicFoo, ...) or Emit("foo.bar", ...)
	publishSubRe = regexp.MustCompile(`(?:Publish|Subscribe|Emit)\s*\(\s*(?:ctx\s*,\s*)?(\w+|"[^"]+"|'[^']+')`)
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

		ext := filepath.Ext(d.Name())
		validExts := map[string]bool{".go": true, ".ts": true, ".yaml": true, ".yml": true}
		if !validExts[ext] {
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

			switch ext {
			case ".go":
				if m := goTopicConstRe.FindStringSubmatch(line); m != nil {
					producers = append(producers, &framework.ProducerContract{
						Type: "event.topic",
						Name: m[1],
						File: rel,
						Line: lineNum,
						Signature: map[string]any{
							"value": m[2],
						},
					})
				}
			case ".ts":
				if m := tsTopicConstRe.FindStringSubmatch(line); m != nil {
					producers = append(producers, &framework.ProducerContract{
						Type: "event.topic",
						Name: m[1],
						File: rel,
						Line: lineNum,
						Signature: map[string]any{
							"value": m[2],
						},
					})
				}
			case ".yaml", ".yml":
				if m := yamlTopicRe.FindStringSubmatch(line); m != nil {
					producers = append(producers, &framework.ProducerContract{
						Type: "event.topic.config",
						Name: m[1],
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
	// Build lookup sets: constant names and their string values.
	producerNames := map[string]bool{}
	topicValues := map[string]string{} // "user.created" -> "TopicUserCreated"
	for _, p := range producers {
		producerNames[p.Name] = true
		if p.Signature != nil {
			if v, ok := p.Signature["value"].(string); ok {
				topicValues[v] = p.Name
			}
		}
	}

	// Track producer file dirs to skip self-references.
	producerDirs := map[string]bool{}
	for _, p := range producers {
		producerDirs[filepath.ToSlash(filepath.Dir(p.File))] = true
	}

	type ConsumerEdge struct {
		Producer string `json:"producer"`
		Consumer string `json:"consumer_file"`
		Line     int    `json:"line"`
		Pattern  string `json:"pattern"`
	}

	var edges []ConsumerEdge

	codeExts := map[string]bool{".go": true, ".ts": true}

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
		if !codeExts[ext] {
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
			if m := publishSubRe.FindStringSubmatch(line); m != nil {
				arg := m[1]
				// Strip quotes if it's a string literal.
				arg = strings.Trim(arg, `"'`)

				// Check if the arg is a known producer constant name.
				if producerNames[arg] {
					edges = append(edges, ConsumerEdge{
						Producer: arg,
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "publish-subscribe",
					})
					continue
				}

				// Check if it's a string value matching a topic.
				if name, ok := topicValues[arg]; ok {
					edges = append(edges, ConsumerEdge{
						Producer: name,
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "publish-subscribe-literal",
					})
				}
			}

			// Also check for usage of topic constant identifiers in other contexts.
			for name := range producerNames {
				if containsIdentifier(line, name) {
					// Only count if it's in a file outside the producer dir.
					fileDir := filepath.ToSlash(filepath.Dir(rel))
					if !producerDirs[fileDir] {
						edges = append(edges, ConsumerEdge{
							Producer: name,
							Consumer: rel,
							Line:     i + 1,
							Pattern:  "usage",
						})
					}
				}
			}
		}
		return nil
	})

	// Deduplicate edges (same producer+consumer+line can appear via multiple patterns).
	seen := map[string]bool{}
	var deduped []ConsumerEdge
	for _, e := range edges {
		key := fmt.Sprintf("%s|%s|%d", e.Producer, e.Consumer, e.Line)
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, e)
		}
	}

	if len(deduped) == 0 {
		return nil, nil
	}

	payload, _ := json.Marshal(deduped)
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

		if pos > 0 {
			r := rune(line[pos-1])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				idx = pos + len(ident)
				continue
			}
		}

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
