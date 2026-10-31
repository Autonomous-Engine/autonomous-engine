package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/evidence"
)

// TestFinalizeTypeScriptESMImportsResolve reproduces a live run (attempt 6):
// src/server/index.ts imports "../config/env.js" (the TypeScript ESM
// convention) while the source is src/config/env.ts. The suite passes, so
// the final gate must verify and push HEAD instead of failing with
// "unresolved imports remain at HEAD".
func TestFinalizeTypeScriptESMImportsResolve(t *testing.T) {
	requireSh(t)
	f := newFinalizeFixture(t)
	for _, dir := range []string{"src/config", "src/server", "tests/unit"} {
		if err := os.MkdirAll(filepath.Join(f.worktree, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, f.worktree, "package.json", `{"name":"demo-app","type":"module","devDependencies":{"vitest":"1.0.0"}}`+"\n")
	writeFile(t, f.worktree, "src/config/env.ts", "export const config = { tilerApiKey: process.env.TILER_API_KEY || '' };\n")
	writeFile(t, f.worktree, "src/server/places.ts", "export const places = [];\n")
	writeFile(t, f.worktree, "src/server/index.ts",
		"import { config } from '../config/env.js';\nimport { places } from './places.js';\nexport const app = { config, places };\n")
	writeFile(t, f.worktree, "tests/unit/server.test.ts",
		"import { describe } from 'vitest';\nimport { app } from '../../src/server/index.js';\ndescribe('x', () => app);\n")
	head := f.commit(t, "feature.txt", "value=2\n", "feat: tiler key")

	in := f.inputs()
	sink := &recordingSink{}
	if !finalizeImplementation(context.Background(), in, sink) {
		t.Fatalf("final gate failed on TypeScript ESM .js specifiers: %+v", sink.published)
	}
	ev := publishedEvidence(t, f, sink)
	if ev.Outcome != evidence.OutcomeVerified {
		t.Fatalf("outcome = %q, want verified (checks %+v)", ev.Outcome, ev.Checks)
	}
	diag := checkOfKind(t, ev, evidence.CheckKindDiagnostic)
	if diag.Status != evidence.CheckPassed {
		t.Fatalf("diagnostics check = %+v, want passed", diag)
	}
	if got := f.remoteSHA(t); got != head {
		t.Fatalf("pushed %q, want %s", got, head)
	}
}
