package testdetect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLanguages(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []string
	}{
		{"empty", nil, nil},
		{"go", []string{"go.mod"}, []string{LanguageGo}},
		{"node without a runner still counts", []string{"package.json"}, []string{LanguageJavaScript}},
		{"python manifest", []string{"pyproject.toml"}, []string{LanguagePython}},
		{"python tests only", []string{"tests/test_x.py"}, []string{LanguagePython}},
		{"multi-language", []string{"requirements.txt", "go.mod", "package.json"}, []string{LanguageGo, LanguageJavaScript, LanguagePython}},
		{"a directory named go.mod is not a module", []string{"go.mod/x"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				p := filepath.Join(dir, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := Languages(dir); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Languages = %v, want %v", got, tc.want)
			}
		})
	}
	if Languages("") != nil {
		t.Fatal(`Languages("") should be nil`)
	}
}

func TestIsPytestCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"python -m pytest":          true,
		"python3 -m pytest -q":      true,
		"pytest tests":              true,
		"py.test":                   true,
		"/usr/bin/python -m pytest": true,
		"python -m unittest":        false,
		"npm test":                  false,
		"":                          false,
	} {
		if got := IsPytestCommand(cmd); got != want {
			t.Errorf("IsPytestCommand(%q) = %t, want %t", cmd, got, want)
		}
	}
}
