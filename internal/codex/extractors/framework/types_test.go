// internal/codex/extractors/framework/types_test.go
package framework

import (
	"encoding/json"
	"testing"
)

func TestBlobHashIsStableAcrossReorderedKeys(t *testing.T) {
	b1 := &Blob{Section: "module", Payload: []byte(`{"a":1,"b":2}`)}
	b2 := &Blob{Section: "module", Payload: []byte(`{"b":2,"a":1}`)}
	h1 := CanonicalHash(b1.Payload)
	h2 := CanonicalHash(b2.Payload)
	if h1 != h2 {
		t.Errorf("hash differs on reordered keys: %s vs %s", h1, h2)
	}
}

func TestKindString(t *testing.T) {
	if string(KindStatic) != "static" || string(KindLLM) != "llm" || string(KindHybrid) != "hybrid" {
		t.Error("Kind constants don't match spec")
	}
}

func TestGitDiffHasTouched(t *testing.T) {
	d := &GitDiff{FilesChanged: []string{"app/page.tsx", "tailwind.config.ts"}}
	if !d.HasTouched([]string{"tailwind.config.*"}) {
		t.Error("glob match should succeed")
	}
	if d.HasTouched([]string{"go.mod"}) {
		t.Error("non-matching glob should return false")
	}
	_ = json.RawMessage{}
}
