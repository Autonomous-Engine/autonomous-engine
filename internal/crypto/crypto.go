// Package crypto provides AES-256-GCM encryption with per-credential HKDF key
// derivation for the credential registry.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
)

const (
	keySize   = 32 // AES-256
	nonceSize = 12 // GCM standard nonce
)

// deriveKey derives a 32-byte AES key from masterKey using HKDF-SHA256 with
// credentialID as the info parameter, ensuring each credential uses a unique
// encryption key.
func deriveKey(masterKey []byte, credentialID uuid.UUID) ([]byte, error) {
	info := []byte("credential-key:" + credentialID.String())
	reader := hkdf.New(sha256.New, masterKey, nil, info)
	key := make([]byte, keySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("hkdf derive key: %w", err)
	}
	return key, nil
}

// Encrypt encrypts plaintext using AES-256-GCM with a key derived from
// masterKey and credentialID. It returns the ciphertext and a random 12-byte
// nonce. The nonce must be stored alongside the ciphertext for later decryption.
func Encrypt(masterKey []byte, credentialID uuid.UUID, plaintext string) (ciphertext []byte, nonce []byte, err error) {
	key, err := deriveKey(masterKey, credentialID)
	if err != nil {
		return nil, nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("create aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create gcm: %w", err)
	}

	nonce = make([]byte, nonceSize)
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext = gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// Decrypt decrypts ciphertext using AES-256-GCM with a key derived from
// masterKey and credentialID. The nonce must match the one used during
// encryption.
func Decrypt(masterKey []byte, credentialID uuid.UUID, ciphertext []byte, nonce []byte) (string, error) {
	key, err := deriveKey(masterKey, credentialID)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("gcm decrypt: %w", err)
	}

	return string(plaintext), nil
}

// LoadMasterKey decodes a base64-encoded master key string and validates that
// it is exactly 32 bytes (suitable for AES-256).
func LoadMasterKey(base64Key string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("decode base64 master key: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", keySize, len(key))
	}
	return key, nil
}

// GenerateMasterKey generates a cryptographically random 32-byte master key
// and returns both the raw bytes and its base64-encoded representation.
func GenerateMasterKey() ([]byte, string, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, "", fmt.Errorf("generate master key: %w", err)
	}
	return key, base64.StdEncoding.EncodeToString(key), nil
}

// MaskValue returns a fixed masking string for secret values, or the original
// value for non-secret ones. This is used when listing credentials so that
// secret values are never exposed in list responses.
func MaskValue(value string, isSecret bool) string {
	if isSecret {
		return "••••••••"
	}
	return value
}
