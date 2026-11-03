package tools

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/llm/adapter"
)

// A LoopStuck travels in research.results, which the outbox stores as
// jsonb; Postgres rejects the \u0000 escape, so the fingerprint must not
// carry a NUL byte (F35).
func TestLoopStuckFingerprintIsJSONBSafe(t *testing.T) {
	ex := NewExecutor(t.TempDir())
	fp, _ := ex.callFingerprint(adapter.ToolCall{Name: "codex_read_file", Arguments: `{"path":"parser.go"}`})
	if fp == "" {
		t.Fatal("expected a fingerprint for a path-bearing call")
	}
	out, err := json.Marshal(LoopStuck{Tool: "codex_read_file", Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`\u0000`)) {
		t.Fatalf("fingerprint serialises with \\u0000: %s", out)
	}
	if got := strings.SplitN(fp, fingerprintSep, 2)[0]; got != "codex_read_file" {
		t.Fatalf("tool name split on fingerprintSep = %q, want codex_read_file", got)
	}
}
