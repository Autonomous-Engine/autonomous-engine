package crypto

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// generateTestKey produces a valid 32-byte master key for use in tests.
func generateTestKey(t *testing.T) []byte {
	t.Helper()
	key, _, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	masterKey := generateTestKey(t)
	credID := uuid.New()
	plaintext := "super-secret-api-key-12345"

	ciphertext, nonce, err := Encrypt(masterKey, credID, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("expected non-empty ciphertext")
	}
	if len(nonce) != nonceSize {
		t.Fatalf("expected nonce length %d, got %d", nonceSize, len(nonce))
	}

	got, err := Decrypt(masterKey, credID, ciphertext, nonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("plaintext mismatch: got %q, want %q", got, plaintext)
	}
}

func TestDifferentCredIDProducesDifferentCiphertext(t *testing.T) {
	masterKey := generateTestKey(t)
	plaintext := "shared-secret-value"

	credID1 := uuid.New()
	credID2 := uuid.New()

	ct1, _, err := Encrypt(masterKey, credID1, plaintext)
	if err != nil {
		t.Fatalf("Encrypt credID1: %v", err)
	}
	ct2, _, err := Encrypt(masterKey, credID2, plaintext)
	if err != nil {
		t.Fatalf("Encrypt credID2: %v", err)
	}

	// Different credential IDs must produce different ciphertexts (different
	// derived keys), even for the same plaintext.
	if bytes.Equal(ct1, ct2) {
		t.Fatal("ciphertexts should differ for different credential IDs")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	masterKey := generateTestKey(t)
	wrongKey := generateTestKey(t)
	credID := uuid.New()

	ct, nonce, err := Encrypt(masterKey, credID, "secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, err = Decrypt(wrongKey, credID, ct, nonce)
	if err == nil {
		t.Fatal("expected decryption with wrong key to fail")
	}
}

func TestDecryptWithWrongCredIDFails(t *testing.T) {
	masterKey := generateTestKey(t)
	credID := uuid.New()
	wrongCredID := uuid.New()

	ct, nonce, err := Encrypt(masterKey, credID, "secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	_, err = Decrypt(masterKey, wrongCredID, ct, nonce)
	if err == nil {
		t.Fatal("expected decryption with wrong credential ID to fail")
	}
}

func TestLoadMasterKeyFromBase64(t *testing.T) {
	_, b64, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}

	key, err := LoadMasterKey(b64)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(key) != keySize {
		t.Fatalf("expected key length %d, got %d", keySize, len(key))
	}
}

func TestLoadMasterKeyRejectsShortKey(t *testing.T) {
	// Base64-encode a 16-byte (too short) key.
	import64 := "AAAAAAAAAAAAAAAAAAAAAA==" // 16 zero bytes in base64
	_, err := LoadMasterKey(import64)
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestGenerateMasterKey(t *testing.T) {
	raw, b64, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	if len(raw) != keySize {
		t.Fatalf("expected raw key length %d, got %d", keySize, len(raw))
	}
	if b64 == "" {
		t.Fatal("expected non-empty base64 key")
	}

	// Two calls must produce different keys.
	raw2, _, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey second call: %v", err)
	}
	if bytes.Equal(raw, raw2) {
		t.Fatal("two generated keys should not be equal")
	}
}

func TestMaskValue(t *testing.T) {
	cases := []struct {
		value    string
		isSecret bool
		want     string
	}{
		{"mysecrettoken", true, "••••••••"},
		{"public-value", false, "public-value"},
		{"", true, "••••••••"},
		{"", false, ""},
	}

	for _, tc := range cases {
		got := MaskValue(tc.value, tc.isSecret)
		if got != tc.want {
			t.Errorf("MaskValue(%q, %v) = %q, want %q", tc.value, tc.isSecret, got, tc.want)
		}
	}
}
