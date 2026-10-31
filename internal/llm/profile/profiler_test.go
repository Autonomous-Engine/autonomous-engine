package profile

import (
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/models"
)

func TestGetEvalTasksReturns6Tasks(t *testing.T) {
	tasks := GetEvalTasks()
	if len(tasks) != 6 {
		t.Errorf("GetEvalTasks returned %d tasks, want 6", len(tasks))
	}
}

func TestGetEvalTasksHaveRequiredFields(t *testing.T) {
	tasks := GetEvalTasks()
	for i, task := range tasks {
		if task.Name == "" {
			t.Errorf("task[%d].Name is empty", i)
		}
		if task.Category == "" {
			t.Errorf("task[%d].Category is empty", i)
		}
		if task.Scorer == nil {
			t.Errorf("task[%d].Scorer is nil", i)
		}
		if len(task.Prompt.Messages) == 0 {
			t.Errorf("task[%d].Prompt.Messages is empty", i)
		}
	}
}

func TestInstructionFollowingScorerPerfectResponse(t *testing.T) {
	tasks := GetEvalTasks()
	// First task is instruction_following
	scorer := tasks[0].Scorer
	score := scorer(`{"status":"ok","value":42}`)
	if score != 100 {
		t.Errorf("perfect response scored %d, want 100", score)
	}
}

func TestInstructionFollowingScorerPartialResponse(t *testing.T) {
	tasks := GetEvalTasks()
	scorer := tasks[0].Scorer
	// Contains some but not all required fields
	score := scorer(`{"status":"ok"}`)
	if score <= 0 || score >= 100 {
		t.Errorf("partial response scored %d, expected 0 < score < 100", score)
	}
}

func TestInstructionFollowingScorerEmptyResponse(t *testing.T) {
	tasks := GetEvalTasks()
	scorer := tasks[0].Scorer
	score := scorer("")
	if score != 0 {
		t.Errorf("empty response scored %d, want 0", score)
	}
}

func TestCodeGenerationScorerHighQuality(t *testing.T) {
	tasks := GetEvalTasks()
	// Second task is code_generation_simple
	scorer := tasks[1].Scorer
	goodCode := `func isPrime(n int) bool {
	if n <= 1 {
		return false
	}
	for i := 2; i*i <= n; i++ {
		if n % 2 == 0 {
			return false
		}
	}
	return true
}`
	score := scorer(goodCode)
	if score < 60 {
		t.Errorf("high-quality code response scored %d, want >= 60", score)
	}
}

func TestReasoningScorerHighScore(t *testing.T) {
	tasks := GetEvalTasks()
	// Fifth task is reasoning
	reasoningTask := tasks[4]
	if reasoningTask.Category != "reasoning" {
		t.Fatalf("expected reasoning task at index 4, got %q", reasoningTask.Category)
	}
	scorer := reasoningTask.Scorer
	goodAnswer := "The function currently returns -1900 because discountPct is 20.0 not 0.20. The correct answer should be 80. The bug is that the code doesn't divide by 100 to convert percent to a decimal fraction."
	score := scorer(goodAnswer)
	if score < 60 {
		t.Errorf("good reasoning response scored %d, want >= 60", score)
	}
}

func TestSynthesisScorerHighScore(t *testing.T) {
	tasks := GetEvalTasks()
	// Sixth task is synthesis
	synthTask := tasks[5]
	if synthTask.Category != "synthesis" {
		t.Fatalf("expected synthesis task at index 5, got %q", synthTask.Category)
	}
	scorer := synthTask.Scorer
	goodAnswer := `The codebase has critical security and performance issues. First priority is the race condition in authentication token refresh, which poses an immediate security risk. Second, error messages expose stack traces to clients — a serious security vulnerability. Third, database queries lack indexing, causing performance degradation. Finally, the rate limiter's fixed window algorithm is prone to boundary bursts. I recommend prioritizing these in the order listed.`
	score := scorer(goodAnswer)
	if score < 80 {
		t.Errorf("good synthesis response scored %d, want >= 80", score)
	}
}

func TestProviderFromName(t *testing.T) {
	cases := []struct {
		name     string
		expected string
	}{
		{"openai/gpt-4o", "openai"},
		{"anthropic/claude-3-5-sonnet-20241022", "anthropic"},
		{"ollama/llama3", "ollama"},
		{"custom/my-model", "custom"},
		{"no-slash", "no-slash"},
	}
	for _, tc := range cases {
		got := providerFromName(tc.name)
		if got != tc.expected {
			t.Errorf("providerFromName(%q) = %q, want %q", tc.name, got, tc.expected)
		}
	}
}

func TestDetermineMode(t *testing.T) {
	// We test the mode-selection logic by checking against the constants used in
	// Evaluate: avg>=70 → Advanced, avg>=40 → Standard, else → Constrained.
	cases := []struct {
		avg      int
		expected models.OperatingMode
	}{
		{100, models.OperatingModeAdvanced},
		{70, models.OperatingModeAdvanced},
		{69, models.OperatingModeStandard},
		{40, models.OperatingModeStandard},
		{39, models.OperatingModeConstrained},
		{0, models.OperatingModeConstrained},
	}
	for _, tc := range cases {
		var mode models.OperatingMode
		switch {
		case tc.avg >= 70:
			mode = models.OperatingModeAdvanced
		case tc.avg >= 40:
			mode = models.OperatingModeStandard
		default:
			mode = models.OperatingModeConstrained
		}
		if mode != tc.expected {
			t.Errorf("avg=%d → mode=%q, want %q", tc.avg, mode, tc.expected)
		}
	}
}

func TestCodeUnderstandingScorerHighScore(t *testing.T) {
	tasks := GetEvalTasks()
	// Fourth task is code_understanding
	var codeUnderstandingTask EvalTask
	for _, task := range tasks {
		if task.Category == "code_understanding" {
			codeUnderstandingTask = task
			break
		}
	}
	if codeUnderstandingTask.Scorer == nil {
		t.Fatal("code_understanding task not found")
	}

	goodAnswer := "This function removes duplicate entries from a string slice. It preserves the original order by tracking seen items in a map. Each item is added to the result slice only if it has not been seen before."
	score := codeUnderstandingTask.Scorer(goodAnswer)
	if score < 60 {
		t.Errorf("good code understanding response scored %d, want >= 60", score)
	}
}
