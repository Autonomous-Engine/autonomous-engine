package testdetect

import (
	"os"
	"path/filepath"
	"strings"
)

// Language names reported by Languages.
const (
	LanguageGo         = "go"
	LanguageJavaScript = "javascript" // JavaScript and TypeScript (package.json)
	LanguagePython     = "python"
)

// Languages reports every language ecosystem the repository root shows
// signals for, in a fixed order (go, javascript, python). A repository may
// be multi-language: impact-based check selection (internal/checks) unions
// the plans of every language listed here.
//
// The signals are the manifests and runner markers DetectTestCommand
// already reads: go.mod; package.json; pyproject.toml, setup.py,
// setup.cfg, requirements.txt, Pipfile, or a pytest marker (pytest.ini,
// conftest.py, tox.ini, a tests/ or test/ directory with test files).
// Unlike DetectTestCommand a manifest alone is enough here: selection only
// ever narrows the command the final gate runs, so a language without a
// runner simply contributes no test targets.
func Languages(repoPath string) []string {
	if repoPath == "" {
		return nil
	}
	var out []string
	if HasGoModule(repoPath) {
		out = append(out, LanguageGo)
	}
	if HasNodePackage(repoPath) {
		out = append(out, LanguageJavaScript)
	}
	if HasPythonProject(repoPath) {
		out = append(out, LanguagePython)
	}
	return out
}

// HasGoModule reports a go.mod at the repository root.
func HasGoModule(repoPath string) bool { return isFile(filepath.Join(repoPath, "go.mod")) }

// HasNodePackage reports a package.json at the repository root.
func HasNodePackage(repoPath string) bool { return isFile(filepath.Join(repoPath, "package.json")) }

// HasPythonProject reports a Python manifest or a pytest marker at the
// repository root.
func HasPythonProject(repoPath string) bool {
	for _, f := range []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile"} {
		if isFile(filepath.Join(repoPath, f)) {
			return true
		}
	}
	return detectPython(repoPath) != ""
}

// IsPytestCommand reports whether cmd invokes pytest the way the Python
// detection does ("python -m pytest") or directly ("pytest", "py.test",
// "python3 -m pytest").
func IsPytestCommand(cmd string) bool {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return false
	}
	switch strings.TrimSuffix(filepath.Base(filepath.ToSlash(f[0])), ".exe") {
	case "pytest", "py.test":
		return true
	case "python", "python3", "py":
		return len(f) >= 3 && f[1] == "-m" && f[2] == "pytest"
	}
	return false
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
