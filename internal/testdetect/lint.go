package testdetect

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// DetectBuildCommand returns the repository's build command when its
// toolchain names one — "go build ./..." for a Go module, "npm run build"
// for a package.json with a build script — or "".
func DetectBuildCommand(repoPath string) string {
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err == nil {
		return "go build ./..."
	}
	if raw, err := os.ReadFile(filepath.Join(repoPath, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(raw, &pkg) == nil && pkg.Scripts["build"] != "" {
			return "npm run build"
		}
	}
	return ""
}

// DetectLintCommand returns the repository's lint command when its
// toolchain names one — "go vet ./..." for a Go module, "npm run lint" for
// a package.json with a lint script — or "" (no lint is better than an
// invented one). The planner attaches it when a criterion requires a lint
// check and no step of the plan runs one.
func DetectLintCommand(repoPath string) string {
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err == nil {
		return "go vet ./..."
	}
	if raw, err := os.ReadFile(filepath.Join(repoPath, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(raw, &pkg) == nil && pkg.Scripts["lint"] != "" {
			return "npm run lint"
		}
	}
	return ""
}
