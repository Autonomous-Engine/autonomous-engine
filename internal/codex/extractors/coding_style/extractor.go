package coding_style

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

// Extractor analyses source files to detect coding style conventions.
// When llm is nil, it returns a pending blob with sampled file paths.
type Extractor struct {
	llm adapter.ModelAdapter
}

func New() *Extractor { return &Extractor{} }

// SetLLM injects the LLM adapter for real analysis.
func (e *Extractor) SetLLM(a adapter.ModelAdapter) { e.llm = a }

func init() {
	framework.Register(New())
}

func (*Extractor) Name() string         { return "coding_style" }
func (*Extractor) Family() string       { return "coding_practices" }
func (*Extractor) Kind() framework.Kind { return framework.KindLLM }

var caresGlobs = []string{
	"**/*.go", "**/*.ts", "**/*.tsx", "**/*.py", "**/*.js", "**/*.jsx",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// PendingOutput is the payload when no LLM is available.
type PendingOutput struct {
	Status       string   `json:"status"`
	SampledFiles []string `json:"sampled_files"`
	Fingerprint  string   `json:"fingerprint"`
}

// AnalysisOutput is the payload from LLM analysis.
type AnalysisOutput struct {
	Status          string            `json:"status"`
	ImportOrder     string            `json:"import_order,omitempty"`
	ErrorHandling   string            `json:"error_handling,omitempty"`
	NamingConvention string           `json:"naming_convention,omitempty"`
	AsyncStyle      string            `json:"async_style,omitempty"`
	Languages       map[string]int    `json:"languages"`
	Fingerprint     string            `json:"fingerprint"`
	Details         string            `json:"details,omitempty"`
}

// scanDirs are the directories we search for source files.
var scanDirs = []string{"src", "app", "internal", "cmd", "lib"}

// extensions for sampling.
var extensions = []string{".go", ".ts", ".tsx", ".py", ".js", ".jsx"}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	sampled := sampleFiles(ec.RepoPath, 10)
	if len(sampled) == 0 {
		return nil, nil
	}

	fingerprint := hashFiles(ec.RepoPath, sampled)

	if e.llm == nil {
		return e.pendingBlob(sampled, fingerprint)
	}

	return e.llmAnalyze(ctx, ec.RepoPath, sampled, fingerprint)
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, prev []*framework.Blob) ([]*framework.Blob, error) {
	sampled := sampleFiles(ec.RepoPath, 10)
	if len(sampled) == 0 {
		return nil, nil
	}

	fingerprint := hashFiles(ec.RepoPath, sampled)

	// If fingerprint matches previous blob, carry forward.
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

func (e *Extractor) pendingBlob(sampled []string, fingerprint string) ([]*framework.Blob, error) {
	out := PendingOutput{
		Status:       "pending_llm_analysis",
		SampledFiles: sampled,
		Fingerprint:  fingerprint,
	}
	payload, _ := json.Marshal(out)
	evidence := make([]framework.Evidence, 0, len(sampled))
	for _, f := range sampled {
		evidence = append(evidence, framework.Evidence{File: f, Note: "sampled for coding style analysis"})
	}
	return []*framework.Blob{{
		Section:    "coding_style",
		Payload:    payload,
		Confidence: 0.5,
		Evidence:   evidence,
	}}, nil
}

func (e *Extractor) llmAnalyze(ctx context.Context, repoPath string, sampled []string, fingerprint string) ([]*framework.Blob, error) {
	// Build a prompt with file samples.
	var sb strings.Builder
	sb.WriteString("Analyze these source files and identify coding style conventions.\n")
	sb.WriteString("Report on: import order, error handling patterns, naming conventions, async style.\n")
	sb.WriteString("Return JSON with fields: import_order, error_handling, naming_convention, async_style, details.\n\n")

	languages := map[string]int{}
	for _, f := range sampled {
		ext := filepath.Ext(f)
		languages[ext]++

		fullPath := filepath.Join(repoPath, f)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}
		// Limit each file to 2000 bytes for prompt size.
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
			{Role: adapter.RoleSystem, Content: "You are a code style analyzer. Return valid JSON only."},
			{Role: adapter.RoleUser, Content: sb.String()},
		},
		MaxTokens:   1024,
		Temperature: 0.2,
		JSONMode:    true,
	})
	if err != nil {
		// Fall back to pending on LLM error.
		return e.pendingBlob(sampled, fingerprint)
	}

	// Parse LLM response into AnalysisOutput.
	out := AnalysisOutput{
		Status:      "analyzed",
		Languages:   languages,
		Fingerprint: fingerprint,
	}

	var llmResult map[string]any
	if json.Unmarshal([]byte(resp.Content), &llmResult) == nil {
		if v, ok := llmResult["import_order"].(string); ok {
			out.ImportOrder = v
		}
		if v, ok := llmResult["error_handling"].(string); ok {
			out.ErrorHandling = v
		}
		if v, ok := llmResult["naming_convention"].(string); ok {
			out.NamingConvention = v
		}
		if v, ok := llmResult["async_style"].(string); ok {
			out.AsyncStyle = v
		}
		if v, ok := llmResult["details"].(string); ok {
			out.Details = v
		}
	}

	payload, _ := json.Marshal(out)
	evidence := make([]framework.Evidence, 0, len(sampled))
	for _, f := range sampled {
		evidence = append(evidence, framework.Evidence{File: f, Note: "analyzed by LLM"})
	}
	return []*framework.Blob{{
		Section:    "coding_style",
		Payload:    payload,
		Confidence: 0.80,
		Evidence:   evidence,
	}}, nil
}

// sampleFiles walks scanDirs for source files and returns up to maxFiles
// via round-robin across directories.
func sampleFiles(repoPath string, maxFiles int) []string {
	buckets := map[string][]string{}
	var dirOrder []string

	for _, dir := range scanDirs {
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
				if base == ".git" || base == "node_modules" || base == "vendor" || base == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(d.Name())
			for _, e := range extensions {
				if ext == e {
					rel, _ := filepath.Rel(repoPath, path)
					rel = filepath.ToSlash(rel)
					bucket := filepath.ToSlash(filepath.Dir(rel))
					if _, exists := buckets[bucket]; !exists {
						dirOrder = append(dirOrder, bucket)
					}
					buckets[bucket] = append(buckets[bucket], rel)
					break
				}
			}
			return nil
		})
	}

	if len(buckets) == 0 {
		return nil
	}

	// Round-robin across directories.
	var result []string
	indices := map[string]int{}
	for len(result) < maxFiles {
		added := false
		for _, dir := range dirOrder {
			if len(result) >= maxFiles {
				break
			}
			idx := indices[dir]
			files := buckets[dir]
			if idx < len(files) {
				result = append(result, files[idx])
				indices[dir] = idx + 1
				added = true
			}
		}
		if !added {
			break
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
