package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Live, both decomposition attempts were valid task arrays except for code
// quoted inside a description: `WithCharsetAliases({"gb2312": GB18030})`
// — the inner quotes were not escaped, the array did not parse, and the
// item was blocked. Inner quotes in a string value are escaped before
// parsing; keys and real closing quotes are left alone.
func TestRepairJSONStringsEscapesInnerQuotesInValues(t *testing.T) {
	raw := `[
  {
    "title": "Add integration test",
    "description": "Parse an email with Subject '=?GB2312?B?...?=' using ` + "`WithCharsetAliases({\"gb2312\": GB18030})`" + ` and assert the subject is '공개'.",
    "criterion_ids": ["AC-4"],
    "files": [{"path": "parser_test.go", "evidence_ref": "hypothesis"}]
  }
]`
	if json.Valid([]byte(raw)) {
		t.Fatal("fixture must be invalid JSON, as the model's output was")
	}
	fixed, n := repairJSONStrings(raw)
	if n != 2 {
		t.Fatalf("repaired %d quotes, want 2", n)
	}
	var tasks []map[string]any
	if err := json.Unmarshal([]byte(fixed), &tasks); err != nil {
		t.Fatalf("repaired output does not parse: %v\n%s", err, fixed)
	}
	desc, _ := tasks[0]["description"].(string)
	if !strings.Contains(desc, `WithCharsetAliases({"gb2312": GB18030})`) {
		t.Fatalf("description lost the quoted code: %q", desc)
	}
	if tasks[0]["criterion_ids"].([]any)[0] != "AC-4" || tasks[0]["files"].([]any)[0].(map[string]any)["path"] != "parser_test.go" {
		t.Fatalf("keys or other values were damaged: %+v", tasks[0])
	}
	// Valid JSON passes through untouched.
	valid := `[{"a": "x, y: z", "b": ["c"]}]`
	if out, n := repairJSONStrings(valid); n != 0 || out != valid {
		t.Fatalf("valid JSON changed: n=%d %q", n, out)
	}
}
