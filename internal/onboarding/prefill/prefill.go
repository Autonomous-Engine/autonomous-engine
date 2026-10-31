// Package prefill orchestrates all onboarding candidate extractors and
// summarizers into a single entrypoint. It lives in a sub-package of
// internal/onboarding because extractors/ already imports onboarding for its
// Candidate/Evidence/QuestionKey types — keeping the orchestrator here avoids
// an import cycle while still satisfying the "files only under
// internal/onboarding/" layering constraint.
package prefill

import (
	"context"
	"fmt"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding"
	"github.com/autonomousengine/autonomous-engine/internal/onboarding/extractors"
	"github.com/google/uuid"
)

// Inputs is everything RunCandidateExtractors needs. The caller (the
// knowledge-indexer, which has already computed all of these) passes them in
// so we don't re-scan. This keeps prefill cheap and deterministic.
type Inputs struct {
	ProductID       uuid.UUID
	RepositoryID    *uuid.UUID
	RepoPath        string
	Languages       []string // e.g. ["Go","TypeScript"]
	ModuleNames     []string // all detected module names
	HotspotFiles    []string // top 10 most-churned file paths
	PatternGroups   []PatternGroupSummary
	TestFramework   string   // from testdetect, e.g. "go test ./..."
	TestFunctions   int      // optional count
	HighRiskModules []string // modules with risk_level=high (dependents>5)
	SiblingRepoURLs []string // other repos in this product for cross_repo_imports
}

// PatternGroupSummary is a minimal struct mirroring knowledge-indexer's
// patternGroup (we avoid importing cmd/ from internal/ — classic Go layering).
type PatternGroupSummary struct {
	Pattern  string
	Count    int
	RefFiles []string // up to 5
}

// RunCandidateExtractors executes every pre-fill extractor and summarizer. It
// is crash-safe: any panicking extractor is recovered and produces a
// NotDetected candidate with the panic message in evidence. All 15
// QuestionKeys are covered — Fail Honestly means we never silently skip.
func RunCandidateExtractors(ctx context.Context, llmSvc *llm.Service, in Inputs) []onboarding.Candidate {
	out := make([]onboarding.Candidate, 0, 15)
	emit := func(fn func() onboarding.Candidate, key, extractorName string) {
		defer func() {
			if r := recover(); r != nil {
				out = append(out, onboarding.NotDetected(in.ProductID, in.RepositoryID, key, extractorName, fmt.Sprintf("extractor panic: %v", r)))
			}
		}()
		out = append(out, fn())
	}

	// 1. tech_stack — from languages + module names (no extractor file needed)
	emit(func() onboarding.Candidate {
		if len(in.Languages) == 0 {
			return onboarding.NotDetected(in.ProductID, in.RepositoryID, onboarding.QTechStack, "detectLanguages", "no languages detected")
		}
		return onboarding.Candidate{
			ProductID: in.ProductID, RepositoryID: in.RepositoryID,
			QuestionKey: onboarding.QTechStack,
			Value:       "Primary languages: " + strings.Join(in.Languages, ", ") + ".",
			Confidence:  onboarding.ConfidenceHigh,
			Evidence:    []onboarding.Evidence{{Kind: "file", Path: "go.mod"}, {Kind: "file", Path: "package.json"}},
			Extractor:   "detectLanguages",
			UserStatus:  onboarding.StatusPending,
		}
	}, onboarding.QTechStack, "detectLanguages")

	// 2. architecture_overview — LLM summary with deterministic fallback
	emit(func() onboarding.Candidate {
		return onboarding.SummarizeArchitecture(ctx, llmSvc, in.ProductID, in.RepositoryID, onboarding.ArchitectureInputs{
			Languages:    in.Languages,
			TopModules:   firstN(in.ModuleNames, 20),
			HotspotFiles: firstN(in.HotspotFiles, 10),
			ModuleCount:  len(in.ModuleNames),
		})
	}, onboarding.QArchitectureOverview, "summarizeArchitecture")

	// 3. patterns_discovered — join pattern groups with count>=2
	emit(func() onboarding.Candidate {
		var bullets []string
		var evidence []onboarding.Evidence
		for _, g := range in.PatternGroups {
			if g.Count < 2 {
				continue
			}
			bullets = append(bullets, fmt.Sprintf("- %s pattern in %d files (e.g. %s)", g.Pattern, g.Count, strings.Join(firstN(g.RefFiles, 2), ", ")))
			for _, f := range firstN(g.RefFiles, 2) {
				evidence = append(evidence, onboarding.Evidence{Kind: "file", Path: f, Name: g.Pattern})
			}
		}
		if len(bullets) == 0 {
			return onboarding.NotDetected(in.ProductID, in.RepositoryID, onboarding.QPatternsDiscovered, "detectPatterns", "no naming-pattern groups with count>=2")
		}
		return onboarding.Candidate{
			ProductID: in.ProductID, RepositoryID: in.RepositoryID,
			QuestionKey: onboarding.QPatternsDiscovered,
			Value:       strings.Join(bullets, "\n"),
			Confidence:  onboarding.ConfidenceHigh,
			Evidence:    evidence,
			Extractor:   "detectPatterns",
			UserStatus:  onboarding.StatusPending,
		}
	}, onboarding.QPatternsDiscovered, "detectPatterns")

	// 4. anti_patterns — explicit not_detected (see spec row 4)
	out = append(out, onboarding.NotDetected(in.ProductID, in.RepositoryID, onboarding.QAntiPatterns, "none", "no deterministic signal — ask the user directly"))

	// 5. branching_strategy
	emit(func() onboarding.Candidate {
		return extractors.DetectBranchingStrategy(ctx, in.ProductID, in.RepositoryID, in.RepoPath)
	}, onboarding.QBranchingStrategy, "detectBranchingStrategy")

	// 6. pr_process
	emit(func() onboarding.Candidate {
		return extractors.DetectPRProcess(ctx, in.ProductID, in.RepositoryID, in.RepoPath)
	}, onboarding.QPRProcess, "detectPRProcess")

	// 7. deployment_process
	emit(func() onboarding.Candidate {
		return extractors.DetectDeploymentSignals(ctx, in.ProductID, in.RepositoryID, in.RepoPath)
	}, onboarding.QDeploymentProcess, "detectDeploymentSignals")

	// 8. domain_knowledge — README summary
	emit(func() onboarding.Candidate {
		body, path, err := extractors.SliceReadme(in.RepoPath)
		if err != nil {
			return onboarding.NotDetected(in.ProductID, in.RepositoryID, onboarding.QDomainKnowledge, "summarizeReadme", err.Error())
		}
		return onboarding.SummarizeReadme(ctx, llmSvc, in.ProductID, in.RepositoryID, path, body)
	}, onboarding.QDomainKnowledge, "summarizeReadme")

	// 9. critical_constraints — compliance file names
	emit(func() onboarding.Candidate {
		return extractors.DetectComplianceFiles(ctx, in.ProductID, in.RepositoryID, in.RepoPath)
	}, onboarding.QCriticalConstraints, "detectComplianceFiles")

	// 10. testing_philosophy — from testdetect
	emit(func() onboarding.Candidate {
		if in.TestFramework == "" {
			return onboarding.NotDetected(in.ProductID, in.RepositoryID, onboarding.QTestingPhilosophy, "testdetect", "no recognised test framework manifest")
		}
		value := fmt.Sprintf("Test framework: `%s`.", in.TestFramework)
		if in.TestFunctions > 0 {
			value += fmt.Sprintf(" ~%d test functions across the repo.", in.TestFunctions)
		}
		return onboarding.Candidate{
			ProductID: in.ProductID, RepositoryID: in.RepositoryID,
			QuestionKey: onboarding.QTestingPhilosophy, Value: value,
			Confidence: onboarding.ConfidenceHigh,
			Evidence:   []onboarding.Evidence{{Kind: "file", Path: "go.mod", Detail: in.TestFramework}},
			Extractor:  "testdetect", UserStatus: onboarding.StatusPending,
		}
	}, onboarding.QTestingPhilosophy, "testdetect")

	// 11. test_conventions — from pattern group "test"
	emit(func() onboarding.Candidate {
		for _, g := range in.PatternGroups {
			if g.Pattern == "test" && g.Count >= 2 && len(g.RefFiles) > 0 {
				return onboarding.Candidate{
					ProductID: in.ProductID, RepositoryID: in.RepositoryID,
					QuestionKey: onboarding.QTestConventions,
					Value:       fmt.Sprintf("Test files follow `_test.` suffix convention across %d files.", g.Count),
					Confidence:  onboarding.ConfidenceMedium,
					Evidence:    []onboarding.Evidence{{Kind: "file", Path: g.RefFiles[0], Name: "test"}},
					Extractor:   "detectPatterns",
					UserStatus:  onboarding.StatusPending,
				}
			}
		}
		return onboarding.NotDetected(in.ProductID, in.RepositoryID, onboarding.QTestConventions, "detectPatterns", "no _test.* pattern group with count>=2")
	}, onboarding.QTestConventions, "detectPatterns")

	// 12-14. autonomous_scope / review_triggers / risk_zones — derived from
	//        HighRiskModules and total module count for codebase-specific answers.
	riskNotes := "(none identified)"
	if len(in.HighRiskModules) > 0 {
		riskNotes = strings.Join(firstN(in.HighRiskModules, 10), ", ")
	}

	totalMods := len(in.ModuleNames)
	lowRiskCount := totalMods - len(in.HighRiskModules)
	if lowRiskCount < 0 {
		lowRiskCount = 0
	}

	// 12. autonomous_scope — enumerate what the system can/can't auto-change
	scopeConf := onboarding.ConfidenceMedium
	var scopeValue string
	if len(in.HighRiskModules) > 0 {
		scopeValue = fmt.Sprintf(
			"Autonomous changes allowed on %d of %d modules (low-risk, few dependents). "+
				"Require human review for: %s. These modules have >5 dependents — changes ripple widely.",
			lowRiskCount, totalMods, riskNotes,
		)
	} else if totalMods > 0 {
		scopeValue = fmt.Sprintf(
			"All %d modules are low-risk (none have >5 dependents). "+
				"Autonomous changes allowed across the codebase. "+
				"Consider restricting auth, database, and API boundary modules once identified.",
			totalMods,
		)
		scopeConf = onboarding.ConfidenceLow
	} else {
		scopeValue = "No modules detected — unable to determine autonomous scope. Run analysis first."
		scopeConf = onboarding.ConfidenceLow
	}
	out = append(out, onboarding.Candidate{
		ProductID: in.ProductID, RepositoryID: in.RepositoryID,
		QuestionKey: onboarding.QAutonomousScope,
		Value:       scopeValue, Confidence: scopeConf,
		Evidence:    highRiskEvidence(in.HighRiskModules),
		Extractor:   "riskHeuristic", UserStatus: onboarding.StatusPending,
	})

	// 13. review_triggers — specific to what was detected
	var triggerParts []string
	triggerParts = append(triggerParts, "auth/security modules", "database schema changes", "public API surfaces")
	if len(in.HighRiskModules) > 0 {
		triggerParts = append(triggerParts, fmt.Sprintf("high-risk modules: %s", riskNotes))
	}
	triggerValue := fmt.Sprintf(
		"Trigger human review on changes to: %s. "+
			"(%d of %d modules flagged as high-risk.)",
		strings.Join(triggerParts, "; "),
		len(in.HighRiskModules), totalMods,
	)
	out = append(out, onboarding.Candidate{
		ProductID: in.ProductID, RepositoryID: in.RepositoryID,
		QuestionKey: onboarding.QReviewTriggers,
		Value:       triggerValue, Confidence: onboarding.ConfidenceMedium,
		Evidence:    highRiskEvidence(in.HighRiskModules),
		Extractor:   "riskHeuristic", UserStatus: onboarding.StatusPending,
	})

	// 14. risk_zones — enumerate the actual high-risk modules
	var riskValue string
	if len(in.HighRiskModules) > 0 {
		riskValue = fmt.Sprintf(
			"%d of %d modules are high-risk (>5 dependents): %s. "+
				"Changes to these modules affect the most downstream code.",
			len(in.HighRiskModules), totalMods, riskNotes,
		)
	} else if totalMods > 0 {
		riskValue = fmt.Sprintf(
			"No high-risk modules detected across %d modules. "+
				"All modules have 5 or fewer dependents. Risk profile is flat.",
			totalMods,
		)
	} else {
		riskValue = "No modules detected — risk analysis requires at least one analyzed module."
	}
	out = append(out, onboarding.Candidate{
		ProductID: in.ProductID, RepositoryID: in.RepositoryID,
		QuestionKey: onboarding.QRiskZones,
		Value:       riskValue,
		Confidence:  onboarding.ConfidenceMedium,
		Evidence:    highRiskEvidence(in.HighRiskModules),
		Extractor:   "riskHeuristic", UserStatus: onboarding.StatusPending,
	})

	// 15. cross_repo_imports — siblings
	emit(func() onboarding.Candidate {
		return extractors.DetectCrossRepoImports(ctx, in.ProductID, in.RepositoryID, in.RepoPath, in.SiblingRepoURLs)
	}, onboarding.QCrossRepoImports, "detectCrossRepoImports")

	return out
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func highRiskEvidence(modules []string) []onboarding.Evidence {
	out := make([]onboarding.Evidence, 0, len(modules))
	for _, m := range modules {
		out = append(out, onboarding.Evidence{Kind: "module", Name: m})
		if len(out) >= 10 {
			break
		}
	}
	return out
}
