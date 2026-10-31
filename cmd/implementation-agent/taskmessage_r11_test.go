package main

import (
	"strings"
	"testing"
)

// R11 S4: the env-config test hint appears when a step's files include a
// test and the files in scope include a config/env module — and only then.
func TestEnvTestHint(t *testing.T) {
	msg := buildTaskMessage(attempt11PlanTask(t))
	if strings.Count(msg, envTestHint) != 1 {
		t.Fatalf("attempt-11 task message lacks the hint (or has it twice):\n%s", msg)
	}
	if !strings.Contains(msg, "## Files in scope\nsrc/config/env.ts") {
		t.Fatalf("scope line moved:\n%s", msg)
	}

	step := func(n int, files string) string {
		return "## Step " + string(rune('0'+n)) + ": step\n\nDo it.\nFiles: [" + files + "]\n"
	}
	cases := []struct {
		name  string
		desc  string
		files []string
		want  bool
	}{
		{"test step + env module", step(1, "src/config/env.ts") + "\n" + step(2, "tests/unit/server.test.ts"), []string{"src/config/env.ts", "tests/unit/server.test.ts"}, true},
		{"test step + settings.py", step(1, "tests/test_app.py"), []string{"app/settings.py", "tests/test_app.py"}, true},
		{"no test step", step(1, "src/config/env.ts") + "\n" + step(2, "README.md"), []string{"src/config/env.ts", "README.md", "tests/unit/server.test.ts"}, false},
		{"test step, no config module", step(1, "src/server/index.ts") + "\n" + step(2, "tests/unit/server.test.ts"), []string{"src/server/index.ts", "tests/unit/server.test.ts", ".env.example", "docker-compose.yml"}, false},
		{"config only as a test helper", step(1, "tests/config/env.test.ts"), []string{"tests/config/env.test.ts"}, false},
		{"no steps", "Fix the config.", []string{"src/config/env.ts", "tests/unit/server.test.ts"}, false},
	}
	for _, c := range cases {
		got := strings.Contains(buildTaskMessage(implementationTaskMessage{Title: "t", Description: c.desc, FilesToChange: c.files}), envTestHint)
		if got != c.want {
			t.Errorf("%s: hint present = %v, want %v", c.name, got, c.want)
		}
	}
}
