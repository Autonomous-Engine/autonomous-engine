package strategy

import (
	"context"
	"fmt"
	"log"
	"strings"
	"unicode"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/models"
	"github.com/autonomousengine/autonomous-engine/internal/prompts"
)

// globalPromptStore is the package-level prompt store used to resolve prompts
// from the database. If nil, the hardcoded switch statement is used as a
// fallback, so the system works with or without a database connection.
var globalPromptStore *prompts.PromptStore

// SetPromptStore wires a PromptStore into the strategy layer. Call this once
// at startup, after the DB pool and migrations are ready.
func SetPromptStore(ps *prompts.PromptStore) {
	globalPromptStore = ps
}

// IntentType identifies the nature of a task being delegated to the LLM.
type IntentType string

const (
	IntentAnalyzePainPoint        IntentType = "analyze_pain_point"
	IntentGeneratePRD             IntentType = "generate_prd"
	IntentEvaluateFindings        IntentType = "evaluate_findings"
	IntentSynthesizeBrief         IntentType = "synthesize_brief"
	IntentDecomposeTasks          IntentType = "decompose_tasks"
	IntentVerifyAssumptions       IntentType = "verify_assumptions"
	IntentImplementChange         IntentType = "implement_change"
	IntentWriteTests              IntentType = "write_tests"
	IntentFixTestFailure          IntentType = "fix_test_failure"
	IntentGeneratePRDescription   IntentType = "generate_pr_description"
	IntentReviewCode              IntentType = "review_code"
	IntentValidateCriterion       IntentType = "validate_criterion"
	IntentGenerateEdgeCaseTests   IntentType = "generate_edge_case_tests"
	IntentOnboardingQuestion      IntentType = "onboarding_question"
	IntentClassifyChange          IntentType = "classify_change"
	IntentTraceCodePath           IntentType = "trace_code_path"
	IntentAnalyzeHistory          IntentType = "analyze_history"
	IntentMatchPatterns           IntentType = "match_patterns"
	IntentAssessImpact            IntentType = "assess_impact"
	IntentSearchRelatedBugs       IntentType = "search_related_bugs"
	IntentMapJiraWorkflow         IntentType = "map_jira_workflow"
)

// Intent describes what the agent needs from the LLM.
type Intent struct {
	Type         IntentType        `json:"type"`
	Context      map[string]string `json:"context"`
	MaxTokens    int               `json:"max_tokens,omitempty"`
	OutputFormat string            `json:"output_format,omitempty"`
	Temperature  float64           `json:"temperature,omitempty"`
}

// Result is the output of a strategy execution.
type Result struct {
	Content      string `json:"content"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`

	// Messages is the full chat history the strategy sent to the adapter
	// (system prompt + user prompt + any assistant/user follow-ups used
	// for self-review). Dashboard renders this so operators can see the
	// actual conversation, not just token counts.
	Messages []adapter.ChatMessage `json:"messages,omitempty"`
}

// Strategy executes an intent against an adapter and returns a result.
type Strategy interface {
	Execute(ctx context.Context, a adapter.ModelAdapter, intent Intent) (*Result, error)
}

// Select returns the appropriate Strategy for the given operating mode.
func Select(mode models.OperatingMode) Strategy {
	switch mode {
	case models.OperatingModeAdvanced:
		return &AdvancedStrategy{}
	case models.OperatingModeStandard:
		return &StandardStrategy{}
	case models.OperatingModeConstrained:
		return &ConstrainedStrategy{}
	default:
		return &StandardStrategy{}
	}
}

// buildSystemPrompt returns a role-appropriate system prompt for the given
// intent. It first attempts to resolve the prompt from the database via
// globalPromptStore. If the store is not configured or no template is found,
// it falls back to the hardcoded switch statement below.
//
// The intent.Context map may carry "agent_type", "provider", "model_id", and
// "operating_mode" keys so that the resolution chain can find the most
// specific override.
func buildSystemPrompt(ctx context.Context, intent Intent) string {
	if globalPromptStore != nil {
		agentType, _ := intent.Context["agent_type"]
		provider, _ := intent.Context["provider"]
		modelID, _ := intent.Context["model_id"]
		mode, _ := intent.Context["operating_mode"]
		content, err := globalPromptStore.Resolve(
			ctx, string(intent.Type), agentType, provider, modelID, mode, "system",
		)
		if err == nil && content != "" {
			return content
		}
		// Log at debug level only — fallback is expected when no override exists.
		log.Printf("strategy: db prompt resolve: %v — using hardcoded fallback", err)
	}

	// Hardcoded fallback — kept in sync with the DB seeds in prompts/prompts.go.
	switch intent.Type {

	// Product / PM intents
	case IntentAnalyzePainPoint:
		return `You are a senior product manager skilled at identifying user pain points and their business impact. Analyze problems with clarity, quantify impact where possible, and distinguish root causes from symptoms. Be concise and structured.`

	case IntentGeneratePRD:
		return `You are an experienced product manager writing Product Requirements Documents. Structure your output with clear sections: Overview, Problem Statement, Goals, Non-Goals, User Stories, and Acceptance Criteria. Be precise about requirements without over-specifying implementation.

You may include an Open Questions section ONLY for genuinely unresolvable unknowns — things that require business owner input that cannot be inferred from the request or the codebase. Questions about implementation approach, UI layout, label text, design patterns, or how to communicate changes are NOT open questions — those are engineering decisions the development team will make. If there are no genuine open questions, omit the section entirely. Most straightforward feature requests have zero open questions.`

	case IntentEvaluateFindings:
		return `You are a senior technical analyst evaluating research findings. Assess evidence quality, identify patterns, flag contradictions, and rank findings by impact and confidence. Distinguish facts from assumptions.`

	case IntentSynthesizeBrief:
		return `You are a technical writer who synthesizes complex information into actionable briefs. Produce structured summaries with clear recommendations, prioritized by impact. Avoid jargon without explanation.`

	case IntentDecomposeTasks:
		return `You are a technical project lead decomposing a technical brief into implementation tasks.

RULES:
1. Every task MUST involve writing or modifying CODE or TESTS. No documentation tasks, no research tasks, no process tasks.
2. Every task MUST reference specific files that exist in the repository. Do not create tasks for files that don't exist.
3. Do NOT create tasks for: updating documentation, searching git history, performance benchmarking, stakeholder communication, deployment, or monitoring. These are handled by other systems.
4. Keep tasks focused: one logical change per task. Don't create tasks that just "verify" without changing code — verification is built into the implementation process.
5. The minimum number of tasks is 1. Only decompose further if the changes genuinely span independent areas of the codebase.
6. Each task must specify which files to modify in the "files" field.

Output a JSON array of tasks. Each task: {"title":"...","description":"...","files":["path/to/file.go"],"depends_on":["Title of dependency if any"]}`

	case IntentVerifyAssumptions:
		return `You are a careful technical reviewer who identifies and challenges assumptions. For each assumption, assess whether it is validated, unvalidated, or contradicted by available evidence. Suggest how to verify unvalidated assumptions.`

	// Engineer / Code intents
	case IntentImplementChange:
		return `You are a senior software engineer implementing production-quality code changes. Write clean, idiomatic code that follows existing patterns. Include error handling, respect style conventions, and produce minimal diff-friendly changes. Output only the implementation.`

	case IntentWriteTests:
		return `You are a senior software engineer writing comprehensive tests. Cover happy paths, edge cases, and error conditions. Use table-driven tests where appropriate. Tests should be self-documenting and independent. Output only the test code.`

	case IntentFixTestFailure:
		return `You are a senior software engineer diagnosing and fixing test failures. Analyze the failure message, identify the root cause, and produce a minimal targeted fix. Explain your diagnosis briefly before providing the fix.`

	case IntentGeneratePRDescription:
		return `You are a senior engineer writing clear pull request descriptions. Summarize what changed, why, and how to test it. Include a brief risk assessment. Use markdown formatting with sections: Summary, Changes, Test Plan, Risk.`

	// Review intents
	case IntentReviewCode:
		return `You are a senior code reviewer with deep expertise in software engineering best practices, security, and performance. Review code for correctness, security vulnerabilities, performance issues, maintainability, and adherence to existing patterns. Be specific, constructive, and prioritize issues by severity.`

	case IntentValidateCriterion:
		return `You are a quality assurance engineer validating whether code or output meets specified acceptance criteria. For each criterion, give a clear pass/fail verdict with specific evidence from the code or output. Be objective and precise.`

	case IntentGenerateEdgeCaseTests:
		return `You are a QA engineer specializing in edge case discovery. Identify boundary conditions, error paths, concurrency issues, and unusual inputs. Write test cases that cover scenarios most developers overlook.`

	// Research / Analysis intents
	case IntentOnboardingQuestion:
		return `You are a helpful engineering mentor answering questions about a codebase. Provide clear, accurate explanations with relevant examples from the code. Point to specific files and functions when helpful.`

	case IntentClassifyChange:
		return `You are a technical analyst classifying code changes by their nature and impact. Categorize changes as: feature, bugfix, refactor, performance, security, test, docs, or config. Assess blast radius and risk level.`

	case IntentTraceCodePath:
		return `You are a senior engineer tracing execution paths through a codebase. Follow the code flow step by step, identify entry points, key decision points, data transformations, and exit conditions. Be precise about file and function references.`

	case IntentAnalyzeHistory:
		return `You are a technical historian analyzing code change history. Identify patterns in how code has evolved, recurring issues, risky areas, and contributor patterns. Extract insights that inform future development decisions.`

	case IntentMatchPatterns:
		return `You are a pattern recognition expert analyzing code for structural and behavioral patterns. Identify design patterns, anti-patterns, recurring idioms, and deviations from established conventions. Relate findings to maintainability and extensibility.`

	case IntentAssessImpact:
		return `You are a senior architect assessing the impact of proposed changes. Analyze downstream effects, integration risks, performance implications, and backwards compatibility concerns. Produce a structured impact assessment with risk ratings.`

	case IntentSearchRelatedBugs:
		return `You are a debugging specialist identifying related bugs and vulnerabilities. Given a bug description or failure, identify similar patterns in the codebase, related known issues, and root cause hypotheses. Prioritize by likelihood and impact.`

	case IntentMapJiraWorkflow:
		return `You are mapping a customer's Jira workflow to an internal pipeline. Given their statuses, issue types, priorities, and workflows, propose the best mapping. Use Jira's status categories as hints: 'new' = early pipeline stages, 'indeterminate' = active work, 'done' = completed stages. Internal states are: intake, prd, research, planning, implementation, review, human_review, qa, passed, failed, blocked, rejected, released. Internal types are: feature, bug, task, subtask, research, qa. Internal priorities are: critical, high, medium, low. Output valid JSON with keys: status_mapping, issue_type_map, priority_map, ingest_jql, ingest_label.`

	default:
		return `You are a senior software engineer and technical expert. Provide precise, well-structured responses based on the provided context. Be concise and actionable.`
	}
}

// buildUserPrompt assembles the context map into a structured prompt.
// Context keys are prioritized in the order defined below.
func buildUserPrompt(intent Intent) string {
	var sb strings.Builder

	// Priority order for context keys
	orderedKeys := []string{
		"technical_brief",
		"prd",
		"pain_point",
		"pattern_reference",
		"target_file",
		"diff",
		"codex_patterns",
		"style_rules",
	}

	// Track which keys have been written
	written := make(map[string]bool)

	writeSection := func(key, heading string) {
		if val, ok := intent.Context[key]; ok && val != "" {
			sb.WriteString(fmt.Sprintf("## %s\n%s\n\n", heading, val))
			written[key] = true
		}
	}

	for _, key := range orderedKeys {
		heading := toTitleCase(strings.ReplaceAll(key, "_", " "))
		writeSection(key, heading)
	}

	// Write any remaining context keys not in the priority list
	for key, val := range intent.Context {
		if !written[key] && val != "" {
			heading := toTitleCase(strings.ReplaceAll(key, "_", " "))
			sb.WriteString(fmt.Sprintf("## %s\n%s\n\n", heading, val))
		}
	}

	// Add output format instruction if specified
	if intent.OutputFormat != "" {
		sb.WriteString(fmt.Sprintf("## Output Format\n%s\n\n", intent.OutputFormat))
	}

	// Add intent-specific task instruction
	sb.WriteString(fmt.Sprintf("## Task\n%s", intentInstruction(intent.Type)))

	return sb.String()
}

// intentInstruction returns a concise task instruction for the intent type.
func intentInstruction(t IntentType) string {
	switch t {
	case IntentAnalyzePainPoint:
		return "Analyze the pain point described above. Identify root causes, affected users, business impact, and key symptoms."
	case IntentGeneratePRD:
		return "Generate a comprehensive Product Requirements Document based on the information provided."
	case IntentEvaluateFindings:
		return "Evaluate the findings provided. Assess evidence quality, identify patterns, and rank by confidence and impact."
	case IntentSynthesizeBrief:
		return "Synthesize the provided information into a concise technical brief with clear recommendations."
	case IntentDecomposeTasks:
		return "Decompose the requirements into concrete, independently executable engineering tasks with clear scope and dependencies."
	case IntentVerifyAssumptions:
		return "Identify and verify the assumptions present in the provided context. For each, assess validation status and suggest verification steps."
	case IntentImplementChange:
		return "Implement the requested change following the existing code patterns and style rules provided."
	case IntentWriteTests:
		return "Write comprehensive tests covering the specified functionality, including edge cases and error conditions."
	case IntentFixTestFailure:
		return "Diagnose the test failure and provide a targeted fix. Explain the root cause before presenting the solution."
	case IntentGeneratePRDescription:
		return "Write a clear and complete pull request description for the changes shown in the diff."
	case IntentReviewCode:
		return "Review the provided code. Identify issues by severity (critical, major, minor) with specific line references and suggested fixes."
	case IntentValidateCriterion:
		return "Validate each acceptance criterion against the provided code or output. Give pass/fail verdicts with evidence."
	case IntentGenerateEdgeCaseTests:
		return "Generate comprehensive edge case tests that cover boundary conditions, error paths, and unusual inputs."
	case IntentOnboardingQuestion:
		return "Answer the question based on the codebase context provided. Be specific and educational."
	case IntentClassifyChange:
		return "Classify the change by type, assess its blast radius, and provide a risk rating with justification."
	case IntentTraceCodePath:
		return "Trace the execution path step by step, identifying all key functions, decision points, and data flows."
	case IntentAnalyzeHistory:
		return "Analyze the change history for patterns, recurring issues, and insights that inform future development."
	case IntentMatchPatterns:
		return "Identify design patterns, anti-patterns, and deviations from conventions in the provided code."
	case IntentAssessImpact:
		return "Produce a structured impact assessment of the proposed change, including risk ratings for each area of concern."
	case IntentSearchRelatedBugs:
		return "Identify related bugs, similar failure patterns, and root cause hypotheses based on the provided context."
	case IntentMapJiraWorkflow:
		return "Analyse the provided Jira project structure and produce a JSON mapping of Jira statuses, issue types, and priorities to internal pipeline equivalents. Include a suitable ingest_jql query and ingest_label value."
	default:
		return "Complete the task described in the context above."
	}
}

// toTitleCase converts a space-separated string to Title Case without using
// the deprecated strings.Title function.
func toTitleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) == 0 {
			continue
		}
		runes := []rune(w)
		runes[0] = unicode.ToUpper(runes[0])
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}
