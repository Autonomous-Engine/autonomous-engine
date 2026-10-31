package testdetect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DetectTestCommand analyzes a repository directory and returns the
// appropriate test command — or "" when the repository has no detectable
// test infrastructure. Historically this returned a command purely on the
// presence of a manifest file (package.json → "npm test"), which produced
// false positives for repos that have the manifest but no test runner
// configured. The implementation-agent then wasted iterations bootstrapping
// a test stack from scratch on every run.
//
// The current policy is stricter: every positive match requires a real
// runner, not just a manifest. When in doubt, return "" — the caller
// should interpret that as "this repo has no tests and the agent should
// not create any."
func DetectTestCommand(repoPath string) string {
	// Language-specific heuristics, ordered from most-specific to least.
	// Each returns a command only when a real runner is wired up.
	if cmd := detectNode(repoPath); cmd != "" {
		return cmd
	}
	if cmd := detectGo(repoPath); cmd != "" {
		return cmd
	}
	if cmd := detectPython(repoPath); cmd != "" {
		return cmd
	}

	if cmd := detectPHP(repoPath); cmd != "" {
		return cmd
	}

	// Fallback heuristics for stacks where a manifest is a stronger signal
	// on its own (Java/Rust/C#/Ruby conventionally ship tests alongside
	// the build system). These stay permissive; tighten if they cause the
	// same false-positive problem.
	permissive := []struct {
		file    string
		command string
	}{
		{"Cargo.toml", "cargo test"},
		{"pom.xml", "mvn test -B"},
		{"build.gradle", "gradle test"},
		{"build.gradle.kts", "gradle test"},
		{"Makefile", "make test"},
		{"CMakeLists.txt", "cmake --build . --target test"},
		{"*.sln", "dotnet test"},
		{"*.csproj", "dotnet test"},
		{"Gemfile", "bundle exec rake test"},
	}
	for _, c := range permissive {
		if c.file[0] == '*' {
			matches, _ := filepath.Glob(filepath.Join(repoPath, c.file))
			if len(matches) > 0 {
				return c.command
			}
		} else {
			if _, err := os.Stat(filepath.Join(repoPath, c.file)); err == nil {
				return c.command
			}
		}
	}

	return ""
}

// FeatureRequestsTests reports whether a work item's title or description
// indicates the feature itself is about adding test coverage. Used by the
// planner and implementation-agent to decide whether the "no test infra
// detected" heuristic applies: when the user explicitly asked for tests,
// the agent MUST create the test infrastructure — dropping test tasks
// under the "no test runner" guard would turn a legitimate feature
// request into an empty subtask. Match is lowercased, conservative, and
// requires a writing/installation verb + the word "test(s)" to avoid
// false positives like "update the test user fixture."
func FeatureRequestsTests(title, description string) bool {
	text := strings.ToLower(title + " " + description)
	phrases := []string{
		"add test", "add tests", "add unit test", "add integration test", "add e2e test",
		"write test", "write tests", "write unit test", "write integration test",
		"create test", "create tests", "create unit test", "create integration test",
		"set up test", "setup test", "setting up test", "set up testing", "setup testing",
		"implement test", "implement tests", "implement testing",
		"introduce test", "introduce tests", "introduce testing",
		"install jest", "install vitest", "install pytest", "install mocha",
		"test coverage", "test suite", "testing framework", "testing infrastructure",
	}
	for _, p := range phrases {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// detectNode returns "npm test" only when package.json declares a test
// script that isn't the npm-default placeholder AND a recognised test
// runner is in dependencies or devDependencies. Without both signals the
// repo has no real tests and the agent should not fabricate any.
func detectNode(repoPath string) string {
	path := filepath.Join(repoPath, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	script, ok := pkg.Scripts["test"]
	if !ok {
		return ""
	}
	// Default npm-init placeholder.
	if strings.Contains(script, "no test specified") {
		return ""
	}
	runners := []string{
		"jest", "vitest", "mocha", "ava", "tap", "jasmine",
		"cypress", "playwright", "@playwright/test", "karma",
		"@testing-library", "node:test",
	}
	hasRunner := false
	for _, r := range runners {
		if _, ok := pkg.Dependencies[r]; ok {
			hasRunner = true
			break
		}
		if _, ok := pkg.DevDependencies[r]; ok {
			hasRunner = true
			break
		}
	}
	// Accept `node --test` style scripts even without a devDep runner.
	if !hasRunner && strings.Contains(script, "node --test") {
		hasRunner = true
	}
	if !hasRunner {
		return ""
	}
	return "npm test"
}

// detectGo returns "go test ./..." only when the repo has at least one
// *_test.go file — go.mod alone is a module marker, not evidence of tests.
func detectGo(repoPath string) string {
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err != nil {
		return ""
	}
	// A single _test.go anywhere in the tree is enough signal.
	found := false
	_ = filepath.Walk(repoPath, func(p string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		name := info.Name()
		if info.IsDir() {
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			found = true
		}
		return nil
	})
	if !found {
		return ""
	}
	return "go test ./... -count=1"
}

// detectPHP returns a phpunit command only when composer.json declares a
// test script OR a phpunit config (phpunit.xml / phpunit.xml.dist / tests/
// directory) is present. Bare composer.json with no test wiring is not
// evidence of tests — plenty of PHP projects ship production code
// without a suite.
func detectPHP(repoPath string) string {
	// Explicit config files are the strongest signal.
	for _, f := range []string{"phpunit.xml", "phpunit.xml.dist", "phpunit.dist.xml"} {
		if _, err := os.Stat(filepath.Join(repoPath, f)); err == nil {
			return "vendor/bin/phpunit"
		}
	}
	// composer.json declares a test script? Prefer that — honours
	// whatever the project actually uses (pest, phpunit, paratest, etc.).
	path := filepath.Join(repoPath, "composer.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]interface{} `json:"scripts"`
		Require map[string]string      `json:"require"`
		RequireDev map[string]string   `json:"require-dev"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	if _, ok := pkg.Scripts["test"]; ok {
		return "composer test"
	}
	runners := []string{"phpunit/phpunit", "pestphp/pest", "phpspec/phpspec", "codeception/codeception"}
	for _, r := range runners {
		if _, ok := pkg.RequireDev[r]; ok {
			return "vendor/bin/phpunit"
		}
		if _, ok := pkg.Require[r]; ok {
			return "vendor/bin/phpunit"
		}
	}
	return ""
}

// detectPython returns "python -m pytest" only when there is a pytest
// config OR a tests/ directory with at least one test_*.py / *_test.py
// file. A bare requirements.txt is not evidence of tests.
func detectPython(repoPath string) string {
	// pytest config markers.
	for _, f := range []string{"pytest.ini", "conftest.py", "tox.ini"} {
		if _, err := os.Stat(filepath.Join(repoPath, f)); err == nil {
			return "python -m pytest"
		}
	}
	// pyproject.toml with a [tool.pytest.ini_options] section.
	if data, err := os.ReadFile(filepath.Join(repoPath, "pyproject.toml")); err == nil {
		if strings.Contains(string(data), "[tool.pytest") {
			return "python -m pytest"
		}
	}
	// Conventional tests directory with test files.
	for _, dir := range []string{"tests", "test"} {
		matches, _ := filepath.Glob(filepath.Join(repoPath, dir, "test_*.py"))
		if len(matches) > 0 {
			return "python -m pytest"
		}
		matches, _ = filepath.Glob(filepath.Join(repoPath, dir, "*_test.py"))
		if len(matches) > 0 {
			return "python -m pytest"
		}
	}
	return ""
}
