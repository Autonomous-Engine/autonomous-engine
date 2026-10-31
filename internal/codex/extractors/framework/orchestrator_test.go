// internal/codex/extractors/framework/orchestrator_test.go
package framework_test

import (
	"context"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
	"github.com/google/uuid"
)

type countingExtractor struct {
	name  string
	kind  framework.Kind
	fullCalls, incCalls int
	cares bool
	blobs []*framework.Blob
}

func (c *countingExtractor) Name() string { return c.name }
func (c *countingExtractor) Family() string { return c.name }
func (c *countingExtractor) Kind() framework.Kind { return c.kind }
func (c *countingExtractor) CaresAbout(d *framework.GitDiff) bool { return c.cares }
func (c *countingExtractor) ExtractFull(_ context.Context, _ *framework.ExtractionContext) ([]*framework.Blob, error) {
	c.fullCalls++
	return c.blobs, nil
}
func (c *countingExtractor) ExtractIncremental(_ context.Context, _ *framework.ExtractionContext, _ []*framework.Blob) ([]*framework.Blob, error) {
	c.incCalls++
	return c.blobs, nil
}

func TestOrchestrator_FullPathOnFirstExtraction(t *testing.T) {
	e := &countingExtractor{name: "t1", kind: framework.KindStatic, cares: true, blobs: []*framework.Blob{
		{Section: "module", Payload: []byte(`{"a":1}`), Confidence: 0.95},
	}}
	registry := framework.NewTestRegistry(e)
	orch := framework.NewOrchestrator(nil, nil, nil, nil, registry)
	ec := &framework.ExtractionContext{
		RepoID: uuid.New(), CommitSHA: "abc", Branch: "main",
		ParentSHA: "",
	}
	_, err := orch.RunForCommit(context.Background(), ec)
	if err != nil {
		t.Fatalf("RunForCommit: %v", err)
	}
	if e.fullCalls != 1 {
		t.Errorf("fullCalls = %d, want 1", e.fullCalls)
	}
	if e.incCalls != 0 {
		t.Errorf("incCalls = %d, want 0 on first extraction", e.incCalls)
	}
}

func TestOrchestrator_SkipsUncaringExtractor(t *testing.T) {
	e := &countingExtractor{name: "t1", kind: framework.KindStatic, cares: false}
	registry := framework.NewTestRegistry(e)
	orch := framework.NewOrchestrator(nil, nil, nil, nil, registry)
	ec := &framework.ExtractionContext{
		RepoID: uuid.New(), CommitSHA: "abc", Branch: "main",
		ParentSHA: "prev", Diff: &framework.GitDiff{FilesChanged: []string{"README.md"}},
	}
	_, err := orch.RunForCommit(context.Background(), ec)
	if err != nil {
		t.Fatalf("RunForCommit: %v", err)
	}
	if e.fullCalls != 0 || e.incCalls != 0 {
		t.Errorf("uncaring extractor should not be called: full=%d inc=%d", e.fullCalls, e.incCalls)
	}
}

func TestRouter_StaticHighConfidenceWithTrustGoesDirect(t *testing.T) {
	dest := framework.Route(&framework.Blob{Confidence: 0.92}, framework.KindStatic, true)
	if dest != framework.DestDirect {
		t.Errorf("static+0.92+trust → %v, want direct", dest)
	}
}

func TestRouter_StaticHighConfidenceNoTrustGoesCandidate(t *testing.T) {
	dest := framework.Route(&framework.Blob{Confidence: 0.92}, framework.KindStatic, false)
	if dest != framework.DestCandidate {
		t.Errorf("static+0.92+no-trust → %v, want candidate", dest)
	}
}

func TestRouter_LLMAlwaysCandidate(t *testing.T) {
	dest := framework.Route(&framework.Blob{Confidence: 0.99}, framework.KindLLM, true)
	if dest != framework.DestCandidate {
		t.Errorf("LLM any-confidence → %v, want candidate", dest)
	}
}

func TestRouter_LowConfidenceAlwaysCandidate(t *testing.T) {
	dest := framework.Route(&framework.Blob{Confidence: 0.5}, framework.KindStatic, true)
	if dest != framework.DestCandidateLowConf {
		t.Errorf("confidence<0.7 → %v, want candidate_low_conf", dest)
	}
}
