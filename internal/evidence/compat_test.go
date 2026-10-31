package evidence

import "testing"

func TestParseCompatWindow(t *testing.T) {
	cases := []struct {
		in     string
		want   CompatWindow
		wantOK bool
	}{
		{"", CompatWindowLegacy, true},
		{"legacy", CompatWindowLegacy, true},
		{" Legacy ", CompatWindowLegacy, true},
		{"strict", CompatWindowStrict, true},
		{"STRICT", CompatWindowStrict, true},
		{"stric", CompatWindowStrict, false},
		{"off", CompatWindowStrict, false},
		{"permissive", CompatWindowStrict, false},
	}
	for _, tc := range cases {
		got, ok := ParseCompatWindow(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ParseCompatWindow(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestCompatWindowFromEnv(t *testing.T) {
	t.Setenv(CompatWindowEnv, "")
	if got := CompatWindowFromEnv(); got != CompatWindowLegacy {
		t.Errorf("unset: got %q, want legacy", got)
	}
	t.Setenv(CompatWindowEnv, "legacy")
	if got := CompatWindowFromEnv(); got != CompatWindowLegacy {
		t.Errorf("legacy: got %q, want legacy", got)
	}
	t.Setenv(CompatWindowEnv, "strict")
	if got := CompatWindowFromEnv(); got != CompatWindowStrict {
		t.Errorf("strict: got %q, want strict", got)
	}
	// An operator typo must fail closed: the gate can only narrow.
	t.Setenv(CompatWindowEnv, "bogus")
	if got := CompatWindowFromEnv(); got != CompatWindowStrict {
		t.Errorf("bogus: got %q, want strict", got)
	}
}
