package codex

import (
	"reflect"
	"testing"
)

func TestChangeStrings(t *testing.T) {
	cases := map[string]string{
		ModuleChange("internal/codex"):            "module:internal/codex",
		SectionChange("risk_zones"):               "section:risk_zones",
		Change{Kind: ChangePatterns}.String():     "patterns",
		Change{Kind: ChangeAntiPatterns}.String(): "anti_patterns",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestParseChange(t *testing.T) {
	cases := []struct {
		in   string
		want Change
	}{
		{"module:cmd/api", Change{Kind: ChangeModule, Name: "cmd/api"}},
		{"section:autonomous_scope", Change{Kind: ChangeSection, Name: "autonomous_scope"}},
		{"patterns", Change{Kind: ChangePatterns}},
		{"anti_patterns", Change{Kind: ChangeAntiPatterns}},
		// Legacy un-prefixed values (before migration 060) are modules.
		{"cmd/api", Change{Kind: ChangeModule, Name: "cmd/api"}},
		{"codebase_architecture", Change{Kind: ChangeModule, Name: "codebase_architecture"}},
		{"codex_patterns", Change{Kind: ChangeModule, Name: "codex_patterns"}},
		// A prefix with nothing after it is not a typed value either.
		{"module:", Change{Kind: ChangeModule, Name: "module:"}},
		{"  module:x ", Change{Kind: ChangeModule, Name: "x"}},
	}
	for _, tc := range cases {
		if got := ParseChange(tc.in); got != tc.want {
			t.Errorf("ParseChange(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeChangesDedupesSortsAndTypesLegacy(t *testing.T) {
	got := NormalizeChanges([]string{"b", "module:a", "patterns", "module:b", "", "section:k", "patterns"})
	want := []string{"module:a", "module:b", "patterns", "section:k"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeChanges = %v, want %v", got, want)
	}
	if got := NormalizeChanges(nil); got == nil || len(got) != 0 {
		t.Fatalf("NormalizeChanges(nil) = %#v, want empty non-nil", got)
	}
}

func TestChangedModulesAndSections(t *testing.T) {
	in := []string{"module:b", "legacy", "section:risk_zones", "module:b", "anti_patterns", "section:autonomous_scope"}
	if got, want := ChangedModules(in), []string{"b", "legacy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedModules = %v, want %v", got, want)
	}
	if got, want := ChangedSectionKeys(in), []string{"autonomous_scope", "risk_zones"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedSectionKeys = %v, want %v", got, want)
	}
}

func TestTableChange(t *testing.T) {
	cases := []struct {
		table, name string
		want        string
		ok          bool
	}{
		{"codex_modules", "billing", "module:billing", true},
		{"codex_sections", "risk_zones", "section:risk_zones", true},
		{"codex_patterns", "anything", "patterns", true},
		{"codex_anti_patterns", "anything", "anti_patterns", true},
		{"codex_modules", "", "", false},
		{"codex_sections", "", "", false},
		{"users", "x", "", false},
	}
	for _, tc := range cases {
		got, ok := TableChange(tc.table, tc.name)
		if got != tc.want || ok != tc.ok {
			t.Errorf("TableChange(%q, %q) = %q, %v; want %q, %v", tc.table, tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTouches(t *testing.T) {
	changed := []string{"module:a", "b", "section:risk_zones"}
	if !Touches(changed, []string{"b"}) {
		t.Error("a legacy un-prefixed change touches the bare module name")
	}
	if !Touches(changed, []string{"module:a"}) {
		t.Error("typed dependency matches")
	}
	if Touches(changed, []string{"c", "section:autonomous_scope"}) {
		t.Error("no common entry")
	}
	if !Touches(changed, []string{"section:risk_zones"}) {
		t.Error("section dependency matches")
	}
}
