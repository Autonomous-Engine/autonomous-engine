package configsnap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/controller"
)

func sampleBuilder() Builder {
	return Builder{
		AgentType: "implementation-agent",
		AgentDefinition: map[string]any{
			"id": "6f1c1d2e-0000-4000-8000-000000000001", "name": "full-stack-engineer",
			"provider": "anthropic", "model_id": "claude-x", "max_retries": json.Number("3"),
			"routing_tags": []any{"a", "b"},
		},
		AgentDefinitionPrompts: []map[string]any{
			{"intent_type": "generate_code", "prompt_type": "user", "content": "U"},
			{"intent_type": "generate_code", "prompt_type": "system", "content": "S"},
		},
		PromptTemplates: []map[string]any{
			{"intent_type": "z", "agent_type": "", "provider": "", "model_id": "", "operating_mode": "", "prompt_type": "system", "content": "Z <b>&"},
			{"intent_type": "a", "agent_type": "implementation-agent", "provider": "", "model_id": "", "operating_mode": "", "prompt_type": "system", "content": "A"},
		},
		Model:       Model{Provider: "anthropic", ModelID: "claude-x", Endpoint: "https://user:pw@api.example.com/v1?api_key=sk-1&region=eu", Source: SourceAgentDefinition},
		LLMProvider: map[string]any{"name": "Anthropic", "provider_type": "anthropic", "settings": map[string]any{"max_tokens": json.Number("4096"), "apiKey": "sk-live"}},
		EmbeddingProvider: map[string]any{
			"name": "ollama", "model": "nomic", "dimension": json.Number("768"), "settings": map[string]any{},
		},
	}
}

func TestHash_StableAcrossKeyOrderAndRowOrder(t *testing.T) {
	a := sampleBuilder()
	b := sampleBuilder()
	// Reverse the row order the "database" returned.
	b.AgentDefinitionPrompts[0], b.AgentDefinitionPrompts[1] = b.AgentDefinitionPrompts[1], b.AgentDefinitionPrompts[0]
	b.PromptTemplates[0], b.PromptTemplates[1] = b.PromptTemplates[1], b.PromptTemplates[0]
	ida, ca, err := Hash(a.Build())
	if err != nil {
		t.Fatal(err)
	}
	idb, cb, err := Hash(b.Build())
	if err != nil {
		t.Fatal(err)
	}
	if ida != idb {
		t.Fatalf("row order changed the hash:\n%s\n%s", ca, cb)
	}
	if !ValidID(ida) {
		t.Fatalf("id %q is not 64 lowercase hex", ida)
	}
	// Many runs of Go map iteration give the same bytes.
	for i := 0; i < 50; i++ {
		id, _, _ := Hash(a.Build())
		if id != ida {
			t.Fatalf("hash not stable on run %d", i)
		}
	}
}

func TestHash_ChangesWithEveryCapturedInput(t *testing.T) {
	base, _, _ := Hash(sampleBuilder().Build())
	mutations := map[string]func(*Builder){
		"prompt content":   func(b *Builder) { b.AgentDefinitionPrompts[0]["content"] = "U2" },
		"template content": func(b *Builder) { b.PromptTemplates[1]["content"] = "A2" },
		"model":            func(b *Builder) { b.Model.ModelID = "claude-y" },
		"definition field": func(b *Builder) { b.AgentDefinition["max_retries"] = json.Number("4") },
		"provider setting": func(b *Builder) {
			b.LLMProvider["settings"] = map[string]any{"max_tokens": json.Number("8192"), "apiKey": "sk-live"}
		},
		"embedding model": func(b *Builder) { b.EmbeddingProvider["model"] = "other" },
		"agent type":      func(b *Builder) { b.AgentType = "review-agent" },
	}
	for name, mutate := range mutations {
		b := sampleBuilder()
		mutate(&b)
		id, _, _ := Hash(b.Build())
		if id == base {
			t.Errorf("%s: hash did not change", name)
		}
	}
}

func TestHash_SecretRotationDoesNotChangeTheHash(t *testing.T) {
	a := sampleBuilder()
	b := sampleBuilder()
	a.AgentDefinition["model_endpoint"] = "https://defuser:defpw@llm.example.com/v1?api-key=sk-def-1&deployment=d1"
	b.AgentDefinition["model_endpoint"] = "https://other:rotated@llm.example.com/v1?api-key=sk-def-2&deployment=d1"
	b.LLMProvider["settings"] = map[string]any{"max_tokens": json.Number("4096"), "apiKey": "sk-rotated"}
	b.Model.Endpoint = "https://other:creds@api.example.com/v1?api_key=sk-2&region=eu"
	ida, ca, _ := Hash(a.Build())
	idb, _, _ := Hash(b.Build())
	if ida != idb {
		t.Fatal("rotating a credential changed the snapshot id")
	}
	if !strings.Contains(string(ca), "llm.example.com/v1?api-key=%5Bredacted%5D&deployment=d1") {
		t.Errorf("definition endpoint not kept in redacted form: %s", ca)
	}
	for _, leak := range []string{"sk-live", "sk-1", "user:pw", "pw@", "sk-def-1", "defuser", "defpw"} {
		if strings.Contains(string(ca), leak) {
			t.Errorf("canonical document leaks %q: %s", leak, ca)
		}
	}
	if !strings.Contains(string(ca), `"max_tokens":4096`) {
		t.Errorf("behavioural setting max_tokens missing: %s", ca)
	}
}

func TestCanonicalize(t *testing.T) {
	got, err := Canonicalize(map[string]any{"b": 1, "a": []any{"<x>&", json.Number("1.50")}, "c": map[string]any{"z": nil, "y": true}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":["<x>&",1.50],"b":1,"c":{"y":true,"z":null}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// A struct and the equivalent map canonicalize identically.
	type s struct {
		B int    `json:"b"`
		A string `json:"a"`
	}
	x, _ := Canonicalize(s{B: 2, A: "q"})
	y, _ := Canonicalize(map[string]any{"a": "q", "b": 2})
	if string(x) != string(y) {
		t.Fatalf("struct %s != map %s", x, y)
	}
	// Round trip through decoded JSON (as from JSONB) is a fixed point.
	again, _ := Canonicalize(json.RawMessage(got))
	if string(again) != want {
		t.Fatalf("not a fixed point: %s", again)
	}
}

func TestBuild_ToleratesMissingComponents(t *testing.T) {
	doc := Builder{AgentType: PipelineAgentType, Model: Model{Source: SourceGlobalDefault}}.Build()
	c, err := Canonicalize(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(c)
	for _, want := range []string{
		`"agent_definition":null`, `"llm_provider":null`, `"embedding_provider":null`,
		`"agent_definition_prompts":[]`, `"prompt_templates":[]`,
		`"checks":"absent"`, `"verifier":"` + VerifierVersion + `"`, `"controller":"` + controller.Version + `"`,
		`"schema_version":1`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document lacks %s: %s", want, s)
		}
	}
}

func TestIsSecretKey(t *testing.T) {
	secret := []string{"api_key", "apiKey", "API-Key", "client_secret", "access_token", "password", "token",
		"api_key_credential_id", "Authorization", "private_key", "refreshToken"}
	plain := []string{"max_tokens", "keep_alive", "model", "temperature", "num_ctx", "key_prefix", "token_budget", "monkey"}
	for _, k := range secret {
		if !IsSecretKey(k) {
			t.Errorf("%q should be secret", k)
		}
	}
	for _, k := range plain {
		if IsSecretKey(k) {
			t.Errorf("%q should not be secret", k)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"http://ollama:11434":                        "http://ollama:11434",
		"https://u:p@host/v1":                        "https://host/v1",
		"https://host/v1?key=abc&region=eu":          "https://host/v1?key=%5Bredacted%5D&region=eu",
		"not a url":                                  "not a url",
		"https://host/openai?api-version=2024-02-01": "https://host/openai?api-version=2024-02-01",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiff(t *testing.T) {
	a := sampleBuilder()
	b := sampleBuilder()
	b.Model.ModelID = "claude-y"
	b.PromptTemplates = append(b.PromptTemplates, map[string]any{"intent_type": "zz", "agent_type": "", "provider": "", "model_id": "", "operating_mode": "", "prompt_type": "system", "content": "new"})
	delete(b.AgentDefinition, "routing_tags")
	changes, err := Diff(a.Build(), b.Build())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Kind
	}
	want := map[string]string{
		"model.model_id":                ChangeChanged,
		"prompt_templates[2]":           ChangeAdded,
		"agent_definition.routing_tags": ChangeRemoved,
	}
	for p, k := range want {
		if got[p] != k {
			t.Errorf("change %s = %q, want %q (all: %+v)", p, got[p], k, changes)
		}
	}
	if len(changes) != len(want) {
		t.Errorf("got %d changes, want %d: %+v", len(changes), len(want), changes)
	}
	same, err := Diff(a.Build(), sampleBuilder().Build())
	if err != nil || len(same) != 0 {
		t.Fatalf("identical documents differ: %v %+v", err, same)
	}
	// A document read back from JSONB (generic tree) equals the built one.
	_, c, _ := Hash(a.Build())
	var back Document
	if err := decodeJSON(c, &back); err != nil {
		t.Fatal(err)
	}
	if d, _ := Diff(a.Build(), back); len(d) != 0 {
		t.Fatalf("round trip differs: %+v", d)
	}
}
