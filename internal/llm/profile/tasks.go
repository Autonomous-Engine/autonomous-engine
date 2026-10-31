package profile

import (
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// EvalTask is a single capability evaluation task.
type EvalTask struct {
	Name     string
	Category string
	Prompt   adapter.CompletionRequest
	Scorer   func(response string) int
}

// GetEvalTasks returns the standard set of evaluation tasks used to profile a model.
// Each scorer returns a score in the range 0–100.
func GetEvalTasks() []EvalTask {
	return []EvalTask{
		{
			Name:     "instruction_following",
			Category: "instruction_following",
			Prompt: adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{
						Role:    adapter.RoleSystem,
						Content: "You are a precise assistant. Follow instructions exactly.",
					},
					{
						Role: adapter.RoleUser,
						Content: `Respond with ONLY the following JSON and nothing else, no markdown, no explanation:
{"status":"ok","value":42}`,
					},
				},
				MaxTokens:   64,
				Temperature: 0.0,
			},
			Scorer: func(resp string) int {
				resp = strings.TrimSpace(resp)
				target := `{"status":"ok","value":42}`
				if resp == target {
					return 100
				}
				score := 0
				if strings.Contains(resp, `"status"`) {
					score += 20
				}
				if strings.Contains(resp, `"ok"`) {
					score += 20
				}
				if strings.Contains(resp, `"value"`) {
					score += 20
				}
				if strings.Contains(resp, `42`) {
					score += 20
				}
				if strings.HasPrefix(resp, "{") && strings.HasSuffix(resp, "}") {
					score += 20
				}
				return score
			},
		},
		{
			Name:     "code_generation_simple",
			Category: "code_generation",
			Prompt: adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{
						Role:    adapter.RoleSystem,
						Content: "You are an expert Go programmer. Respond with only the requested code, no markdown fences.",
					},
					{
						Role:    adapter.RoleUser,
						Content: "Write a Go function called isPrime that takes an int and returns bool. Include only the function, no package declaration.",
					},
				},
				MaxTokens:   256,
				Temperature: 0.1,
			},
			Scorer: func(resp string) int {
				score := 0
				lower := strings.ToLower(resp)
				if strings.Contains(lower, "func isprime") {
					score += 30
				}
				if strings.Contains(resp, "bool") {
					score += 15
				}
				if strings.Contains(resp, "return false") || strings.Contains(resp, "return true") {
					score += 15
				}
				if strings.Contains(resp, "% 2") || strings.Contains(resp, "%2") || strings.Contains(resp, "for i") {
					score += 20
				}
				if strings.Contains(resp, "n <= 1") || strings.Contains(resp, "n < 2") || strings.Contains(resp, "n == 1") {
					score += 20
				}
				return score
			},
		},
		{
			Name:     "code_generation_complex",
			Category: "code_generation",
			Prompt: adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{
						Role:    adapter.RoleSystem,
						Content: "You are an expert Go programmer. Respond with only the requested code, no markdown fences.",
					},
					{
						Role: adapter.RoleUser,
						Content: `Write a Go function called groupAverage that takes a slice of structs with fields Name string and Value float64, and returns a map[string]float64 where each key is a Name and the value is the average of all Values with that Name. Include only the function and any necessary type definitions.`,
					},
				},
				MaxTokens:   512,
				Temperature: 0.1,
			},
			Scorer: func(resp string) int {
				score := 0
				lower := strings.ToLower(resp)
				if strings.Contains(lower, "func groupaverage") {
					score += 25
				}
				if strings.Contains(resp, "map[string]float64") {
					score += 20
				}
				if strings.Contains(resp, "float64") {
					score += 10
				}
				if strings.Contains(resp, "range") {
					score += 15
				}
				if strings.Contains(resp, "sum") || strings.Contains(resp, "total") || strings.Contains(resp, "acc") {
					score += 15
				}
				if strings.Contains(resp, "count") || strings.Contains(resp, "len") {
					score += 15
				}
				return score
			},
		},
		{
			Name:     "code_understanding",
			Category: "code_understanding",
			Prompt: adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{
						Role:    adapter.RoleSystem,
						Content: "You are a code analysis expert. Explain code behavior concisely.",
					},
					{
						Role: adapter.RoleUser,
						Content: `Explain what the following Go function does:

func deduplicate(items []string) []string {
	seen := make(map[string]bool)
	result := []string{}
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

Describe: what it returns, how it handles duplicates, and whether it preserves order.`,
					},
				},
				MaxTokens:   256,
				Temperature: 0.1,
			},
			Scorer: func(resp string) int {
				score := 0
				lower := strings.ToLower(resp)
				if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicat") {
					score += 35
				}
				if strings.Contains(lower, "order") || strings.Contains(lower, "preserv") {
					score += 35
				}
				if strings.Contains(lower, "map") || strings.Contains(lower, "seen") || strings.Contains(lower, "track") {
					score += 30
				}
				return score
			},
		},
		{
			Name:     "reasoning",
			Category: "reasoning",
			Prompt: adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{
						Role:    adapter.RoleSystem,
						Content: "You are a careful analyst. Identify bugs and compute correct values.",
					},
					{
						Role: adapter.RoleUser,
						Content: `Analyze this discount calculation function:

func applyDiscount(price float64, discountPct float64) float64 {
    discount := price * discountPct
    return price - discount
}

If called with applyDiscount(100.0, 20.0):
1. What value does it currently return?
2. What value should it return for a 20% discount?
3. What is the bug?`,
					},
				},
				MaxTokens:   256,
				Temperature: 0.1,
			},
			Scorer: func(resp string) int {
				score := 0
				// Current return: 100 - 100*20 = 100-2000 = -1900
				if strings.Contains(resp, "-1900") || strings.Contains(resp, "-1,900") {
					score += 35
				}
				// Correct: 80
				if strings.Contains(resp, "80") {
					score += 35
				}
				// Bug: should divide by 100
				lower := strings.ToLower(resp)
				if strings.Contains(lower, "100") && (strings.Contains(lower, "divid") || strings.Contains(lower, "percent") || strings.Contains(lower, "0.20") || strings.Contains(lower, "0.2")) {
					score += 30
				}
				return score
			},
		},
		{
			Name:     "synthesis",
			Category: "synthesis",
			Prompt: adapter.CompletionRequest{
				Messages: []adapter.ChatMessage{
					{
						Role:    adapter.RoleSystem,
						Content: "You are a technical writer. Synthesize information into clear summaries.",
					},
					{
						Role: adapter.RoleUser,
						Content: `Given these findings from a code review:
- Finding 1: The authentication module has a race condition in token refresh
- Finding 2: Database queries in the user service lack proper indexing
- Finding 3: Error messages expose internal stack traces to clients
- Finding 4: The rate limiter uses a fixed window algorithm prone to boundary bursts

Write a concise technical brief (3-5 sentences) summarizing the security and performance concerns, and recommend a priority order for fixing them.`,
					},
				},
				MaxTokens:   512,
				Temperature: 0.2,
			},
			Scorer: func(resp string) int {
				score := 0
				lower := strings.ToLower(resp)
				if strings.Contains(lower, "race") || strings.Contains(lower, "authentication") || strings.Contains(lower, "token") {
					score += 20
				}
				if strings.Contains(lower, "index") || strings.Contains(lower, "database") || strings.Contains(lower, "query") {
					score += 20
				}
				if strings.Contains(lower, "security") || strings.Contains(lower, "error") || strings.Contains(lower, "stack trace") || strings.Contains(lower, "expose") {
					score += 20
				}
				if strings.Contains(lower, "rate limit") || strings.Contains(lower, "burst") || strings.Contains(lower, "window") {
					score += 20
				}
				if strings.Contains(lower, "priorit") || strings.Contains(lower, "first") || strings.Contains(lower, "recommend") {
					score += 20
				}
				return score
			},
		},
	}
}
