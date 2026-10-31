package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
	"github.com/google/uuid"
)

// ArchitectureInputs is the 2 KB summary fed to the LLM for the
// architecture_overview candidate. Fields are all small strings or short
// slices so the prompt stays below the model's practical limit even on
// constrained providers.
type ArchitectureInputs struct {
	Languages    []string
	TopModules   []string // e.g. first 20 module names
	HotspotFiles []string // top 10 most-churned files
	ModuleCount  int
}

// SummarizeArchitecture produces a 2–3-sentence architecture_overview
// candidate. If llmSvc is nil or the call fails, it returns a deterministic
// fallback string and confidence=low. Evidence always cites the inputs.
func SummarizeArchitecture(ctx context.Context, llmSvc *llm.Service, productID uuid.UUID, repoID *uuid.UUID, in ArchitectureInputs) Candidate {
	evidence := make([]Evidence, 0, len(in.TopModules)+len(in.HotspotFiles))
	for _, m := range in.TopModules {
		evidence = append(evidence, Evidence{Kind: "module", Name: m})
		if len(evidence) >= 15 {
			break
		}
	}
	for _, f := range in.HotspotFiles {
		evidence = append(evidence, Evidence{Kind: "file", Path: f})
		if len(evidence) >= 25 {
			break
		}
	}

	topMods := truncateSlice(in.TopModules, 8)
	hotspots := truncateSlice(in.HotspotFiles, 5)
	var deterParts []string
	deterParts = append(deterParts, fmt.Sprintf(
		"Codebase with %d detected modules across %s.",
		in.ModuleCount, strings.Join(in.Languages, ", "),
	))
	if len(topMods) > 0 {
		deterParts = append(deterParts, fmt.Sprintf(
			"Key modules: %s.", strings.Join(topMods, ", "),
		))
	}
	if len(hotspots) > 0 {
		deterParts = append(deterParts, fmt.Sprintf(
			"Most actively changed files: %s.", strings.Join(hotspots, ", "),
		))
	}
	if in.ModuleCount > 20 {
		deterParts = append(deterParts, "This is a large codebase — review module dependencies in the Codex graph for the full picture.")
	}
	deterministic := strings.Join(deterParts, " ")

	if llmSvc == nil {
		return Candidate{
			ProductID: productID, RepositoryID: repoID,
			QuestionKey: QArchitectureOverview,
			Value:       deterministic, Confidence: ConfidenceLow,
			Evidence: evidence, Extractor: "summarizeArchitecture",
			UserStatus: StatusPending,
		}
	}

	ctxJSON, _ := json.Marshal(in)
	intent := strategy.Intent{
		Type: strategy.IntentOnboardingQuestion,
		Context: map[string]string{
			"question_topic": "architecture_overview",
			"question_text":  "In 2-3 sentences, describe the high-level architecture of this codebase.",
			"signals":        string(ctxJSON),
		},
		MaxTokens:   220,
		Temperature: 0.1,
	}
	res, err := llmSvc.Execute(ctx, intent)
	if err != nil || res == nil || strings.TrimSpace(res.Content) == "" {
		return Candidate{
			ProductID: productID, RepositoryID: repoID,
			QuestionKey: QArchitectureOverview,
			Value:       deterministic, Confidence: ConfidenceLow,
			Evidence: evidence, Extractor: "summarizeArchitecture",
			UserStatus: StatusPending,
		}
	}
	return Candidate{
		ProductID: productID, RepositoryID: repoID,
		QuestionKey: QArchitectureOverview,
		Value:       strings.TrimSpace(res.Content), Confidence: ConfidenceMedium,
		Evidence: evidence, Extractor: "summarizeArchitecture",
		UserStatus: StatusPending,
	}
}

// SummarizeReadme turns a README slice into a domain_knowledge candidate.
// readmeBody may be empty — caller signals that via an empty readmePath, which
// forces the not_detected bucket.
func SummarizeReadme(ctx context.Context, llmSvc *llm.Service, productID uuid.UUID, repoID *uuid.UUID, readmePath, readmeBody string) Candidate {
	if readmePath == "" || strings.TrimSpace(readmeBody) == "" {
		return NotDetected(productID, repoID, QDomainKnowledge, "summarizeReadme", "no README found")
	}
	lineCount := strings.Count(readmeBody, "\n")
	evidence := []Evidence{{
		Kind: "file", Path: readmePath, Lines: fmt.Sprintf("1-%d", lineCount),
	}}
	deterministic := fmt.Sprintf("README (%s, first %d lines) describes the product. Review manually for domain language and bounded contexts.", readmePath, lineCount)
	if llmSvc == nil {
		return Candidate{
			ProductID: productID, RepositoryID: repoID,
			QuestionKey: QDomainKnowledge,
			Value:       deterministic, Confidence: ConfidenceLow,
			Evidence: evidence, Extractor: "summarizeReadme",
			UserStatus: StatusPending,
		}
	}
	intent := strategy.Intent{
		Type: strategy.IntentOnboardingQuestion,
		Context: map[string]string{
			"question_topic": "domain_knowledge",
			"question_text":  "Summarise the core domain concepts an engineer must understand, based only on the README text below.",
			"readme":         readmeBody,
		},
		MaxTokens:   300,
		Temperature: 0.1,
	}
	res, err := llmSvc.Execute(ctx, intent)
	if err != nil || res == nil || strings.TrimSpace(res.Content) == "" {
		return Candidate{
			ProductID: productID, RepositoryID: repoID,
			QuestionKey: QDomainKnowledge,
			Value:       deterministic, Confidence: ConfidenceLow,
			Evidence: evidence, Extractor: "summarizeReadme",
			UserStatus: StatusPending,
		}
	}
	return Candidate{
		ProductID: productID, RepositoryID: repoID,
		QuestionKey: QDomainKnowledge,
		Value:       strings.TrimSpace(res.Content), Confidence: ConfidenceLow, // README-only input — confidence stays low even when LLM answers
		Evidence: evidence, Extractor: "summarizeReadme",
		UserStatus: StatusPending,
	}
}

func truncateSlice(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
