// internal/codex/extractors/framework/registry_test.go
package framework

import (
	"context"
	"testing"
)

type fakeExtractor struct {
	name, family string
	kind         Kind
	cares        bool
}

func (f *fakeExtractor) Name() string                  { return f.name }
func (f *fakeExtractor) Family() string                { return f.family }
func (f *fakeExtractor) Kind() Kind                    { return f.kind }
func (f *fakeExtractor) CaresAbout(_ *GitDiff) bool    { return f.cares }
func (f *fakeExtractor) ExtractFull(_ context.Context, _ *ExtractionContext) ([]*Blob, error) {
	return nil, nil
}
func (f *fakeExtractor) ExtractIncremental(_ context.Context, _ *ExtractionContext, _ []*Blob) ([]*Blob, error) {
	return nil, nil
}

func TestRegisterAndAll(t *testing.T) {
	registry = map[string]Extractor{}
	Register(&fakeExtractor{name: "a", family: "x", kind: KindStatic})
	Register(&fakeExtractor{name: "b", family: "y", kind: KindLLM})
	all := All()
	if len(all) != 2 {
		t.Fatalf("All() = %d extractors, want 2", len(all))
	}
}

func TestGetReturnsByName(t *testing.T) {
	registry = map[string]Extractor{}
	Register(&fakeExtractor{name: "a"})
	if got, ok := Get("a"); !ok || got.Name() != "a" {
		t.Errorf("Get(a) = (%v, %v), want (a, true)", got, ok)
	}
	if _, ok := Get("nope"); ok {
		t.Error("Get(nope) should return ok=false")
	}
}

func TestDoubleRegisterReplaces(t *testing.T) {
	registry = map[string]Extractor{}
	Register(&fakeExtractor{name: "a", family: "v1"})
	Register(&fakeExtractor{name: "a", family: "v2"})
	got, _ := Get("a")
	if got.Family() != "v2" {
		t.Errorf("double-register should replace; got family %q", got.Family())
	}
}
