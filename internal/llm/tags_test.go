package llm

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/autonomousengine/autonomous-engine/internal/agentregistry"
	"github.com/autonomousengine/autonomous-engine/internal/config"
	"github.com/autonomousengine/autonomous-engine/internal/llm/strategy"
)

func TestRoleForAgentType(t *testing.T) {
	cases := map[string]string{
		"implementation-agent":   RoleAuthor,
		"review-agent":           RoleReviewer,
		"qa-agent":               RoleQA,
		"implementation-planner": RolePlanner,
		"product-agent":          RolePlanner,
		"research-agent":         RoleResearch,
		"research-coordinator":   RoleResearch,
		"change-reconciler":      RoleOther,
		"knowledge-indexer":      RoleOther,
		"":                       "",
	}
	for at, want := range cases {
		if got := RoleForAgentType(at); got != want {
			t.Errorf("RoleForAgentType(%q) = %q, want %q", at, got, want)
		}
	}
}

func TestInferenceTags_ContextOverridesAndMerges(t *testing.T) {
	wid := uuid.New()
	ctx := WithInferenceTags(context.Background(), InferenceTags{Role: RoleOther, AgentType: "chat", WorkItemID: &wid})
	ctx = WithInferenceTags(ctx, InferenceTags{Role: RoleAuthor, ConfigSnapshotID: "abc"})
	got := InferenceTagsFrom(ctx)
	if got.Role != RoleAuthor || got.AgentType != "chat" || got.ConfigSnapshotID != "abc" || got.WorkItemID == nil || *got.WorkItemID != wid {
		t.Fatalf("merged tags = %+v", got)
	}
	svc := InferenceTags{Role: RolePlanner, AgentType: "product-agent", Provider: "anthropic", ModelID: "m"}
	eff := InferenceTagsFrom(ctx).merge(svc)
	if eff.Role != RoleAuthor || eff.Provider != "anthropic" || eff.ModelID != "m" {
		t.Fatalf("context tags must win over service tags, service fills gaps: %+v", eff)
	}
	if (InferenceTagsFrom(context.Background()) != InferenceTags{}) {
		t.Fatal("empty context carries tags")
	}
}

func TestTagsForResolution(t *testing.T) {
	wid := uuid.New()
	tags := TagsForResolution(&agentregistry.Resolution{AgentType: "review-agent", Provider: "ollama", ModelID: "qwen", SnapshotID: "s1"}, wid)
	if tags.Role != RoleReviewer || tags.AgentType != "review-agent" || tags.ConfigSnapshotID != "s1" ||
		tags.Provider != "ollama" || tags.ModelID != "qwen" || tags.WorkItemID == nil || *tags.WorkItemID != wid {
		t.Fatalf("tags = %+v", tags)
	}
	if (TagsForResolution(nil, wid) != InferenceTags{}) {
		t.Fatal("nil resolution must give empty tags")
	}
}

func TestInferenceRowIdentity(t *testing.T) {
	wid := uuid.New()
	other := uuid.New()
	intent := strategy.Intent{Context: map[string]string{}}

	// Untagged registry service: the adapter name is split (pre-M4-05 the
	// whole name went to both columns and no price could match).
	p, m, w := inferenceRowIdentity(nil, "anthropic/claude-x", intent, InferenceTags{})
	if p != "anthropic" || m != "claude-x" || w != nil {
		t.Fatalf("untagged = %q %q %v", p, m, w)
	}
	// Tags win; the tagged work item is used when the intent names none —
	// the fix for product-agent / planner / reconciler calls whose intents
	// omit work_item_id.
	p, m, w = inferenceRowIdentity(&config.ModelConfig{Provider: "openai", Name: "gpt"}, "openai/gpt", intent,
		InferenceTags{Provider: "vllm", ModelID: "llama", WorkItemID: &wid})
	if p != "vllm" || m != "llama" || w == nil || *w != wid {
		t.Fatalf("tagged = %q %q %v", p, m, w)
	}
	// The intent's work item wins over the tags'.
	intent.Context["work_item_id"] = other.String()
	_, _, w = inferenceRowIdentity(nil, "x/y", intent, InferenceTags{WorkItemID: &wid})
	if w == nil || *w != other {
		t.Fatalf("intent work item = %v", w)
	}
	// Static config (NewService) when untagged.
	p, m, _ = inferenceRowIdentity(&config.ModelConfig{Provider: "openai", Name: "gpt"}, "openai/gpt", strategy.Intent{}, InferenceTags{})
	if p != "openai" || m != "gpt" {
		t.Fatalf("config = %q %q", p, m)
	}
}

func TestModelIdentity(t *testing.T) {
	if p, m := ModelIdentity("openai/qwen", InferenceTags{}); p != "openai" || m != "qwen" {
		t.Fatalf("%q %q", p, m)
	}
	if p, m := ModelIdentity("openai/qwen", InferenceTags{Provider: "lmstudio", ModelID: "qwen"}); p != "lmstudio" || m != "qwen" {
		t.Fatalf("%q %q", p, m)
	}
}
