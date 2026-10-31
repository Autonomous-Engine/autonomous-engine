package interactions

import (
	"encoding/json"
	"testing"
)

func TestActionMarshalsWithSemantic(t *testing.T) {
	a := Action{ID: "use_sendmail", Label: "Use sendMail", Semantic: SemanticContinue}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["semantic"] != "continue" {
		t.Errorf("semantic = %q, want %q", got["semantic"], "continue")
	}
}

func TestClassifiedAsIsCodex(t *testing.T) {
	cases := []struct {
		in   ClassifiedAs
		want bool
	}{
		{ClassifiedCodexPattern, true},
		{ClassifiedCodexAntipattern, true},
		{ClassifiedCodexModuleCorrection, true},
		{ClassifiedCodexDomainFact, true},
		{ClassifiedCodexStyleRule, true},
		{ClassifiedAdHoc, false},
		{ClassifiedEphemeral, false},
	}
	for _, c := range cases {
		if c.in.IsCodex() != c.want {
			t.Errorf("IsCodex(%s) = %v, want %v", c.in, c.in.IsCodex(), c.want)
		}
	}
}

func TestClassifiedAsCodexTable(t *testing.T) {
	// Every codex_* classification must map to an existing codex section
	// table name; non-codex classifications must return empty string.
	cases := []struct {
		in   ClassifiedAs
		want string
	}{
		{ClassifiedCodexModuleCorrection, "codex_modules"},
		{ClassifiedCodexPattern, "codex_patterns"},
		{ClassifiedCodexAntipattern, "codex_anti_patterns"},
		{ClassifiedCodexDomainFact, "codex_sections"},
		{ClassifiedCodexStyleRule, "codex_sections"},
		{ClassifiedAdHoc, ""},
		{ClassifiedEphemeral, ""},
	}
	for _, c := range cases {
		if got := c.in.CodexTable(); got != c.want {
			t.Errorf("CodexTable(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}
