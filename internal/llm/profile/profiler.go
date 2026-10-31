package profile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
	"github.com/autonomousengine/autonomous-engine/internal/models"
)

// Profiler evaluates a ModelAdapter against a suite of tasks and produces a
// ModelProfile describing its capabilities and recommended operating mode.
type Profiler struct {
	adapter adapter.ModelAdapter
}

// NewProfiler creates a new Profiler for the given adapter.
func NewProfiler(a adapter.ModelAdapter) *Profiler {
	return &Profiler{adapter: a}
}

// Evaluate runs all evaluation tasks and returns a populated ModelProfile.
func (p *Profiler) Evaluate(ctx context.Context) (*models.ModelProfile, error) {
	tasks := GetEvalTasks()

	// category → scores
	categoryScores := make(map[string][]int)
	var totalLatencyMs int64
	var totalInputTokens, totalOutputTokens int

	for _, task := range tasks {
		start := time.Now()
		resp, err := p.adapter.Complete(ctx, task.Prompt)
		latency := time.Since(start).Milliseconds()

		if err != nil {
			// Treat errors as zero score; record latency as timeout-like value
			categoryScores[task.Category] = append(categoryScores[task.Category], 0)
			totalLatencyMs += latency
			continue
		}

		score := task.Scorer(resp.Content)
		categoryScores[task.Category] = append(categoryScores[task.Category], score)
		totalLatencyMs += latency
		totalInputTokens += resp.InputTokens
		totalOutputTokens += resp.OutputTokens
	}

	avg := func(scores []int) int {
		if len(scores) == 0 {
			return 0
		}
		sum := 0
		for _, s := range scores {
			sum += s
		}
		return sum / len(scores)
	}

	instructionFollowing := avg(categoryScores["instruction_following"])
	codeGeneration := avg(categoryScores["code_generation"])
	codeUnderstanding := avg(categoryScores["code_understanding"])
	reasoning := avg(categoryScores["reasoning"])
	synthesis := avg(categoryScores["synthesis"])

	overallAvg := (instructionFollowing + codeGeneration + codeUnderstanding + reasoning + synthesis) / 5

	var mode models.OperatingMode
	switch {
	case overallAvg >= 70:
		mode = models.OperatingModeAdvanced
	case overallAvg >= 40:
		mode = models.OperatingModeStandard
	default:
		mode = models.OperatingModeConstrained
	}

	// Tokens per second (output tokens over total latency)
	avgInferenceSpeed := float64(0)
	if totalLatencyMs > 0 {
		avgInferenceSpeed = float64(totalOutputTokens) / (float64(totalLatencyMs) / 1000.0)
	}

	caps := p.adapter.Capabilities()

	profile := &models.ModelProfile{
		ID:                     uuid.New(),
		Provider:               providerFromName(p.adapter.Name()),
		ModelID:                p.adapter.Name(),
		EffectiveContextWindow: caps.MaxContextTokens,
		InstructionFollowing:   instructionFollowing,
		CodeGeneration:         codeGeneration,
		CodeUnderstanding:      codeUnderstanding,
		Reasoning:              reasoning,
		Synthesis:              synthesis,
		AvgInferenceSpeed:      avgInferenceSpeed,
		ConcurrentCapacity:     1,
		OperatingMode:          mode,
		EvaluatedAt:            time.Now(),
	}

	return profile, nil
}

// providerFromName extracts the provider prefix from an adapter name like "openai/gpt-4o".
func providerFromName(name string) string {
	for i, c := range name {
		if c == '/' {
			return name[:i]
		}
	}
	return name
}

// ScoreSummary is a helper for logging/reporting eval results.
type ScoreSummary struct {
	TaskName string
	Category string
	Score    int
	LatencyMs int64
	Error    error
}

// EvaluateVerbose runs all tasks and returns per-task summaries in addition to the profile.
func (p *Profiler) EvaluateVerbose(ctx context.Context) (*models.ModelProfile, []ScoreSummary, error) {
	tasks := GetEvalTasks()
	summaries := make([]ScoreSummary, 0, len(tasks))
	categoryScores := make(map[string][]int)
	var totalLatencyMs int64
	var totalOutputTokens int

	for _, task := range tasks {
		start := time.Now()
		resp, err := p.adapter.Complete(ctx, task.Prompt)
		latency := time.Since(start).Milliseconds()

		s := ScoreSummary{
			TaskName:  task.Name,
			Category:  task.Category,
			LatencyMs: latency,
		}

		if err != nil {
			s.Error = fmt.Errorf("task %s: %w", task.Name, err)
			s.Score = 0
		} else {
			s.Score = task.Scorer(resp.Content)
			totalOutputTokens += resp.OutputTokens
		}

		categoryScores[task.Category] = append(categoryScores[task.Category], s.Score)
		totalLatencyMs += latency
		summaries = append(summaries, s)
	}

	avg := func(scores []int) int {
		if len(scores) == 0 {
			return 0
		}
		sum := 0
		for _, s := range scores {
			sum += s
		}
		return sum / len(scores)
	}

	instructionFollowing := avg(categoryScores["instruction_following"])
	codeGeneration := avg(categoryScores["code_generation"])
	codeUnderstanding := avg(categoryScores["code_understanding"])
	reasoning := avg(categoryScores["reasoning"])
	synthesis := avg(categoryScores["synthesis"])

	overallAvg := (instructionFollowing + codeGeneration + codeUnderstanding + reasoning + synthesis) / 5

	var mode models.OperatingMode
	switch {
	case overallAvg >= 70:
		mode = models.OperatingModeAdvanced
	case overallAvg >= 40:
		mode = models.OperatingModeStandard
	default:
		mode = models.OperatingModeConstrained
	}

	avgInferenceSpeed := float64(0)
	if totalLatencyMs > 0 {
		avgInferenceSpeed = float64(totalOutputTokens) / (float64(totalLatencyMs) / 1000.0)
	}

	caps := p.adapter.Capabilities()
	profile := &models.ModelProfile{
		ID:                     uuid.New(),
		Provider:               providerFromName(p.adapter.Name()),
		ModelID:                p.adapter.Name(),
		EffectiveContextWindow: caps.MaxContextTokens,
		InstructionFollowing:   instructionFollowing,
		CodeGeneration:         codeGeneration,
		CodeUnderstanding:      codeUnderstanding,
		Reasoning:              reasoning,
		Synthesis:              synthesis,
		AvgInferenceSpeed:      avgInferenceSpeed,
		ConcurrentCapacity:     1,
		OperatingMode:          mode,
		EvaluatedAt:            time.Now(),
	}

	return profile, summaries, nil
}
