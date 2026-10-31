package api_style

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// Extractor detects API style and framework conventions.
// Static phase: scans package.json and go.mod for framework markers.
// LLM phase: samples handler files for conventions.
type Extractor struct {
	llm adapter.ModelAdapter
}

func New() *Extractor { return &Extractor{} }

// SetLLM injects the LLM adapter for full analysis.
func (e *Extractor) SetLLM(a adapter.ModelAdapter) { e.llm = a }

func init() {
	framework.Register(New())
}

func (*Extractor) Name() string         { return "api_style" }
func (*Extractor) Family() string       { return "coding_practices" }
func (*Extractor) Kind() framework.Kind { return framework.KindHybrid }

var caresGlobs = []string{
	"package.json", "go.mod",
	"routes/**/*", "api/**/*", "app/api/**/*",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// StaticOutput is the payload from static analysis phase.
type StaticOutput struct {
	PrimaryFramework string   `json:"primary_framework"`
	Frameworks       []string `json:"frameworks"`
	Language         string   `json:"language"`
	HandlerFiles     []string `json:"handler_files,omitempty"`
	Fingerprint      string   `json:"fingerprint"`
	LLMStatus        string   `json:"llm_status"` // "pending" or "analyzed"
	LLMDetails       string   `json:"llm_details,omitempty"`
}

// Known Node.js API framework packages.
var nodeFrameworks = map[string]string{
	"express":           "express",
	"fastify":           "fastify",
	"koa":               "koa",
	"@trpc/server":      "trpc",
	"graphql":           "graphql",
	"apollo-server":     "graphql",
	"@apollo/server":    "graphql",
	"@nestjs/core":      "nestjs",
	"hapi":              "hapi",
	"@hapi/hapi":        "hapi",
	"next":              "nextjs",
}

// Known Go API framework modules.
var goFrameworks = map[string]string{
	"github.com/gin-gonic/gin":   "gin",
	"github.com/labstack/echo":   "echo",
	"github.com/go-chi/chi":      "chi",
	"github.com/gofiber/fiber":   "fiber",
	"github.com/gorilla/mux":     "gorilla-mux",
	"google.golang.org/grpc":     "grpc",
}

// Handler file search directories.
var handlerDirs = []string{"routes", "api", "app/api", "handlers", "controllers", "internal/api", "internal/handler"}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	var frameworks []string
	var language string

	// Check package.json for Node.js frameworks.
	pkgPath := filepath.Join(ec.RepoPath, "package.json")
	if data, err := os.ReadFile(pkgPath); err == nil {
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			allDeps := mergeMaps(pkg.Dependencies, pkg.DevDependencies)
			for dep, label := range nodeFrameworks {
				if _, ok := allDeps[dep]; ok {
					frameworks = append(frameworks, label)
				}
			}
			if len(frameworks) > 0 {
				language = "javascript"
			}
		}
	}

	// Check go.mod for Go frameworks.
	goModPath := filepath.Join(ec.RepoPath, "go.mod")
	if data, err := os.ReadFile(goModPath); err == nil {
		content := string(data)
		for mod, label := range goFrameworks {
			if strings.Contains(content, mod) {
				frameworks = append(frameworks, label)
				language = "go"
			}
		}
	}

	if len(frameworks) == 0 {
		return nil, nil
	}

	// Deduplicate frameworks.
	frameworks = dedupe(frameworks)

	primary := frameworks[0]

	// Find handler files for LLM analysis.
	handlerFiles := findHandlerFiles(ec.RepoPath, 5)

	// Build fingerprint from package.json + go.mod + handler files.
	fingerprintFiles := []string{}
	if _, err := os.Stat(pkgPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "package.json")
	}
	if _, err := os.Stat(goModPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "go.mod")
	}
	fingerprintFiles = append(fingerprintFiles, handlerFiles...)
	fingerprint := hashFiles(ec.RepoPath, fingerprintFiles)

	out := StaticOutput{
		PrimaryFramework: primary,
		Frameworks:       frameworks,
		Language:         language,
		HandlerFiles:     handlerFiles,
		Fingerprint:      fingerprint,
		LLMStatus:        "pending",
	}

	confidence := 0.85

	// LLM phase: if adapter available, analyze handler files.
	if e.llm != nil && len(handlerFiles) > 0 {
		details, err := e.llmAnalyzeHandlers(ctx, ec.RepoPath, handlerFiles, primary)
		if err == nil {
			out.LLMStatus = "analyzed"
			out.LLMDetails = details
			confidence = 0.80
		}
	}

	payload, _ := json.Marshal(out)
	evidence := []framework.Evidence{{File: "package.json", Note: "API framework detection"}}
	for _, f := range handlerFiles {
		evidence = append(evidence, framework.Evidence{File: f, Note: "handler file"})
	}

	return []*framework.Blob{{
		Section:    "api_style",
		Payload:    payload,
		Confidence: confidence,
		Evidence:   evidence,
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, prev []*framework.Blob) ([]*framework.Blob, error) {
	// Compute fingerprint for incremental check.
	fingerprintFiles := []string{}
	pkgPath := filepath.Join(ec.RepoPath, "package.json")
	goModPath := filepath.Join(ec.RepoPath, "go.mod")
	if _, err := os.Stat(pkgPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "package.json")
	}
	if _, err := os.Stat(goModPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "go.mod")
	}
	handlerFiles := findHandlerFiles(ec.RepoPath, 5)
	fingerprintFiles = append(fingerprintFiles, handlerFiles...)
	fingerprint := hashFiles(ec.RepoPath, fingerprintFiles)

	if len(prev) > 0 {
		var prevData map[string]any
		if json.Unmarshal(prev[0].Payload, &prevData) == nil {
			if fp, ok := prevData["fingerprint"].(string); ok && fp == fingerprint {
				return prev, nil
			}
		}
	}

	return e.ExtractFull(ctx, ec)
}

func (e *Extractor) llmAnalyzeHandlers(ctx context.Context, repoPath string, files []string, framework string) (string, error) {
	var sb strings.Builder
	sb.WriteString("Analyze these API handler files for a " + framework + " application.\n")
	sb.WriteString("Identify: routing conventions, middleware usage, error handling, response format.\n")
	sb.WriteString("Return JSON with a single 'details' string field summarizing conventions.\n\n")

	for _, f := range files {
		fullPath := filepath.Join(repoPath, f)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 2000 {
			content = content[:2000] + "\n... (truncated)"
		}
		sb.WriteString("=== " + f + " ===\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	resp, err := e.llm.Complete(ctx, adapter.CompletionRequest{
		Messages: []adapter.ChatMessage{
			{Role: adapter.RoleSystem, Content: "You are an API style analyzer. Return valid JSON only."},
			{Role: adapter.RoleUser, Content: sb.String()},
		},
		MaxTokens:   512,
		Temperature: 0.2,
		JSONMode:    true,
	})
	if err != nil {
		return "", err
	}

	var result map[string]any
	if json.Unmarshal([]byte(resp.Content), &result) == nil {
		if v, ok := result["details"].(string); ok {
			return v, nil
		}
	}

	return resp.Content, nil
}

// findHandlerFiles walks known handler directories for source files.
func findHandlerFiles(repoPath string, maxFiles int) []string {
	var result []string
	exts := map[string]bool{".go": true, ".ts": true, ".js": true, ".tsx": true, ".jsx": true}

	for _, dir := range handlerDirs {
		if len(result) >= maxFiles {
			break
		}
		dirPath := filepath.Join(repoPath, dir)
		if info, err := os.Stat(dirPath); err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.WalkDir(dirPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				base := d.Name()
				if base == "node_modules" || base == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if len(result) >= maxFiles {
				return filepath.SkipAll
			}
			ext := filepath.Ext(d.Name())
			if exts[ext] {
				rel, _ := filepath.Rel(repoPath, path)
				result = append(result, filepath.ToSlash(rel))
			}
			return nil
		})
	}

	return result
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

func dedupe(items []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

// hashFiles computes a SHA-256 hash over the content of the given files.
func hashFiles(repoPath string, paths []string) string {
	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(repoPath, p))
		if err != nil {
			continue
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
