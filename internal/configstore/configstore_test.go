package configstore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/crypto"
	"github.com/google/uuid"
)

func TestCredentialEntryJSONRoundTrip(t *testing.T) {
	id := uuid.New()
	createdBy := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	original := CredentialEntry{
		ID:          id,
		Scope:       "global",
		ScopeTarget: "",
		KeyName:     "OPENAI_API_KEY",
		Value:       "sk-test-value",
		Category:    "llm",
		IsSecret:    true,
		Description: "OpenAI API key",
		CreatedBy:   &createdBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CredentialEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID mismatch: got %v, want %v", decoded.ID, original.ID)
	}
	if decoded.Scope != original.Scope {
		t.Errorf("Scope mismatch: got %v, want %v", decoded.Scope, original.Scope)
	}
	if decoded.KeyName != original.KeyName {
		t.Errorf("KeyName mismatch: got %v, want %v", decoded.KeyName, original.KeyName)
	}
	if decoded.IsSecret != original.IsSecret {
		t.Errorf("IsSecret mismatch: got %v, want %v", decoded.IsSecret, original.IsSecret)
	}
	if decoded.CreatedBy == nil || *decoded.CreatedBy != createdBy {
		t.Errorf("CreatedBy mismatch: got %v, want %v", decoded.CreatedBy, createdBy)
	}
}

func TestCredentialEntryOmitsNilCreatedBy(t *testing.T) {
	entry := CredentialEntry{
		ID:      uuid.New(),
		Scope:   "global",
		KeyName: "KEY",
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal to map failed: %v", err)
	}

	if v, ok := m["created_by"]; ok && v != nil {
		t.Errorf("created_by should be omitted or null when nil, got %v", v)
	}
}

func TestConfigEntryJSONRoundTrip(t *testing.T) {
	id := uuid.New()
	updatedBy := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)

	original := ConfigEntry{
		ID:          id,
		Scope:       "agent_type",
		ScopeTarget: "research-agent",
		KeyName:     "MAX_RETRIES",
		Value:       "5",
		ValueType:   "integer",
		Category:    "agent",
		Description: "Maximum number of retries",
		UpdatedBy:   &updatedBy,
		UpdatedAt:   now,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ConfigEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Value != "5" {
		t.Errorf("Value = %q, want %q", decoded.Value, "5")
	}
	if decoded.ValueType != "integer" {
		t.Errorf("ValueType = %q, want %q", decoded.ValueType, "integer")
	}
	if decoded.UpdatedBy == nil || *decoded.UpdatedBy != updatedBy {
		t.Errorf("UpdatedBy mismatch: got %v, want %v", decoded.UpdatedBy, updatedBy)
	}
}

func TestMaskValueMasksSecrets(t *testing.T) {
	masked := crypto.MaskValue("real-api-key-abc123", true)
	if masked == "real-api-key-abc123" {
		t.Error("MaskValue should not return the original value for secrets")
	}
	if masked == "" {
		t.Error("MaskValue should not return empty string for secrets")
	}
}

func TestMaskValueReturnsOriginalForNonSecrets(t *testing.T) {
	original := "some-non-secret-value"
	result := crypto.MaskValue(original, false)
	if result != original {
		t.Errorf("MaskValue(non-secret) = %q, want %q", result, original)
	}
}

func TestCacheKeyFormat(t *testing.T) {
	key := cacheKey("global", "", "OPENAI_KEY", "llm")
	expected := "global||OPENAI_KEY|llm"
	if key != expected {
		t.Errorf("cacheKey = %q, want %q", key, expected)
	}
}

func TestCacheKeyWithAllFields(t *testing.T) {
	key := cacheKey("agent_instance", "inst-123", "MODEL", "config")
	expected := "agent_instance|inst-123|MODEL|config"
	if key != expected {
		t.Errorf("cacheKey = %q, want %q", key, expected)
	}
}
