package testing_conventions

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

// Extractor detects testing framework and conventions.
// Static phase: detects test framework from package.json or go.mod,
// finds test directory layout.
// LLM phase: samples test files for mock library usage.
type Extractor struct {
	llm adapter.ModelAdapter
}

func New() *Extractor { return &Extractor{} }

// SetLLM injects the LLM adapter for full analysis.
func (e *Extractor) SetLLM(a adapter.ModelAdapter) { e.llm = a }

func init() {
	framework.Register(New())
}

func (*Extractor) Name() string         { return "testing_conventions" }
func (*Extractor) Family() string       { return "coding_practices" }
func (*Extractor) Kind() framework.Kind { return framework.KindHybrid }

var caresGlobs = []string{
	"package.json", "go.mod",
	"jest.config.*", "vitest.config.*",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// StaticOutput is the payload from static analysis.
type StaticOutput struct {
	TestFramework    string   `json:"test_framework"`
	TestFrameworks   []string `json:"test_frameworks"`
	Language         string   `json:"language"`
	TestDirs         []string `json:"test_dirs"`
	TestFilePattern  string   `json:"test_file_pattern"`
	TestFileSamples  []string `json:"test_file_samples,omitempty"`
	Fingerprint      string   `json:"fingerprint"`
	LLMStatus        string   `json:"llm_status"`
	LLMDetails       string   `json:"llm_details,omitempty"`
}

// nodeTestFramework pairs a package name with a label. The slice is ordered by
// priority so that test runners (jest, vitest, ...) appear before auxiliary
// libraries (testing-library). The first match becomes the primary framework.
type nodeTestFramework struct {
	pkg   string
	label string
}

var nodeTestFrameworks = []nodeTestFramework{
	{"jest", "jest"},
	{"vitest", "vitest"},
	{"mocha", "mocha"},
	{"playwright", "playwright"},
	{"@playwright/test", "playwright"},
	{"cypress", "cypress"},
	{"ava", "ava"},
	{"jasmine", "jasmine"},
	{"@testing-library/react", "testing-library"},
	{"@testing-library/jest-dom", "testing-library"},
}

// Test directory patterns.
var testDirNames = []string{
	"__tests__", "test", "tests", "spec", "specs",
	"e2e", "integration", "cypress",
}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	var frameworks []string
	var language string

	// Check package.json for Node.js test frameworks.
	pkgPath := filepath.Join(ec.RepoPath, "package.json")
	if data, err := os.ReadFile(pkgPath); err == nil {
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
			Scripts         map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			allDeps := mergeMaps(pkg.Dependencies, pkg.DevDependencies)
			for _, ntf := range nodeTestFrameworks {
				if _, ok := allDeps[ntf.pkg]; ok {
					if !contains(frameworks, ntf.label) {
						frameworks = append(frameworks, ntf.label)
					}
				}
			}
			// Also check scripts for test framework hints.
			if testScript, ok := pkg.Scripts["test"]; ok {
				for _, fw := range []string{"jest", "vitest", "mocha", "ava"} {
					if strings.Contains(testScript, fw) && !contains(frameworks, fw) {
						frameworks = append(frameworks, fw)
					}
				}
			}
			if len(frameworks) > 0 {
				language = "javascript"
			}
		}
	}

	// Check go.mod for Go testing (always uses stdlib testing).
	goModPath := filepath.Join(ec.RepoPath, "go.mod")
	if _, err := os.Stat(goModPath); err == nil {
		// Go projects always use stdlib testing package.
		if !contains(frameworks, "go-testing") {
			frameworks = append(frameworks, "go-testing")
		}
		if language == "" {
			language = "go"
		}

		// Check for popular Go test libraries.
		if data, err := os.ReadFile(goModPath); err == nil {
			content := string(data)
			goTestLibs := map[string]string{
				"github.com/stretchr/testify": "testify",
				"github.com/onsi/ginkgo":      "ginkgo",
				"github.com/onsi/gomega":      "gomega",
			}
			for mod, label := range goTestLibs {
				if strings.Contains(content, mod) {
					frameworks = append(frameworks, label)
				}
			}
		}
	}

	if len(frameworks) == 0 {
		return nil, nil
	}

	// Deduplicate.
	frameworks = dedupe(frameworks)

	primary := frameworks[0]

	// Find test directories.
	testDirs := findTestDirs(ec.RepoPath)

	// Detect test file pattern.
	testFilePattern := detectTestFilePattern(ec.RepoPath, language)

	// Find test file samples for LLM.
	testFileSamples := findTestFiles(ec.RepoPath, 5)

	// Build fingerprint.
	fingerprintFiles := []string{}
	if _, err := os.Stat(pkgPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "package.json")
	}
	if _, err := os.Stat(goModPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "go.mod")
	}
	fingerprintFiles = append(fingerprintFiles, testFileSamples...)
	fingerprint := hashFiles(ec.RepoPath, fingerprintFiles)

	out := StaticOutput{
		TestFramework:   primary,
		TestFrameworks:  frameworks,
		Language:        language,
		TestDirs:        testDirs,
		TestFilePattern: testFilePattern,
		TestFileSamples: testFileSamples,
		Fingerprint:     fingerprint,
		LLMStatus:       "pending",
	}

	confidence := 0.85

	// LLM phase: if adapter available, analyze test files.
	if e.llm != nil && len(testFileSamples) > 0 {
		details, err := e.llmAnalyzeTests(ctx, ec.RepoPath, testFileSamples, primary)
		if err == nil {
			out.LLMStatus = "analyzed"
			out.LLMDetails = details
			confidence = 0.80
		}
	}

	payload, _ := json.Marshal(out)
	evidence := []framework.Evidence{}
	if _, err := os.Stat(pkgPath); err == nil {
		evidence = append(evidence, framework.Evidence{File: "package.json", Note: "test framework detection"})
	}
	if _, err := os.Stat(goModPath); err == nil {
		evidence = append(evidence, framework.Evidence{File: "go.mod", Note: "test framework detection"})
	}
	for _, d := range testDirs {
		evidence = append(evidence, framework.Evidence{File: d, Note: "test directory"})
	}

	return []*framework.Blob{{
		Section:    "testing_conventions",
		Payload:    payload,
		Confidence: confidence,
		Evidence:   evidence,
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, prev []*framework.Blob) ([]*framework.Blob, error) {
	fingerprintFiles := []string{}
	pkgPath := filepath.Join(ec.RepoPath, "package.json")
	goModPath := filepath.Join(ec.RepoPath, "go.mod")
	if _, err := os.Stat(pkgPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "package.json")
	}
	if _, err := os.Stat(goModPath); err == nil {
		fingerprintFiles = append(fingerprintFiles, "go.mod")
	}
	testFileSamples := findTestFiles(ec.RepoPath, 5)
	fingerprintFiles = append(fingerprintFiles, testFileSamples...)
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

func (e *Extractor) llmAnalyzeTests(ctx context.Context, repoPath string, files []string, fw string) (string, error) {
	var sb strings.Builder
	sb.WriteString("Analyze these test files for a project using " + fw + ".\n")
	sb.WriteString("Identify: mock library usage, test organization, assertion style, setup/teardown patterns.\n")
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
			{Role: adapter.RoleSystem, Content: "You are a testing conventions analyzer. Return valid JSON only."},
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

// findTestDirs looks for known test directory names in the repo root.
func findTestDirs(repoPath string) []string {
	var dirs []string
	for _, name := range testDirNames {
		dirPath := filepath.Join(repoPath, name)
		if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
			dirs = append(dirs, name)
		}
	}
	return dirs
}

// detectTestFilePattern determines the naming convention for test files.
func detectTestFilePattern(repoPath string, language string) string {
	specCount := 0
	testCount := 0

	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.Contains(name, ".spec.") {
			specCount++
		} else if strings.Contains(name, ".test.") || strings.Contains(name, "_test.") {
			testCount++
		}
		return nil
	})

	switch {
	case specCount > testCount:
		return "*.spec.*"
	case testCount > 0:
		if language == "go" {
			return "*_test.go"
		}
		return "*.test.*"
	default:
		if language == "go" {
			return "*_test.go"
		}
		return "*.test.*"
	}
}

// findTestFiles walks the repo for test files and returns up to maxFiles.
func findTestFiles(repoPath string, maxFiles int) []string {
	var result []string
	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(result) >= maxFiles {
			return filepath.SkipAll
		}
		name := d.Name()
		if strings.Contains(name, ".spec.") || strings.Contains(name, ".test.") || strings.HasSuffix(name, "_test.go") {
			rel, _ := filepath.Rel(repoPath, path)
			result = append(result, filepath.ToSlash(rel))
		}
		return nil
	})
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

func contains(items []string, val string) bool {
	for _, item := range items {
		if item == val {
			return true
		}
	}
	return false
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
