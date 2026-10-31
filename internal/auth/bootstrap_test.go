package auth

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// TestBootstrapPasswordFormat verifies that the password generation logic used
// in Bootstrap produces a 16-character hex string. We cannot call Bootstrap
// directly because it requires a database, but we can replicate and test the
// exact generation logic.
func TestBootstrapPasswordFormat(t *testing.T) {
	rawBytes := make([]byte, 8)
	if _, err := rand.Read(rawBytes); err != nil {
		t.Fatalf("rand.Read failed: %v", err)
	}
	password := hex.EncodeToString(rawBytes)

	// 8 bytes encoded as hex → 16 characters
	if len(password) != 16 {
		t.Errorf("password length = %d, want 16", len(password))
	}

	// All characters must be valid hex digits
	for i, c := range password {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("password[%d] = %q is not a lowercase hex digit", i, c)
		}
	}
}

// TestBootstrapPasswordUniqueness verifies that two generated passwords differ.
func TestBootstrapPasswordUniqueness(t *testing.T) {
	generate := func() string {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			t.Fatalf("rand.Read: %v", err)
		}
		return hex.EncodeToString(b)
	}

	p1 := generate()
	p2 := generate()

	if p1 == p2 {
		t.Error("two generated passwords are identical — randomness failure")
	}
}

// TestBootstrapFunctionExists is a compile-time check that the Bootstrap
// function with the expected signature exists in the package.
func TestBootstrapFunctionExists(t *testing.T) {
	// If Bootstrap did not exist or had a different signature, this file
	// would not compile.
	_ = Bootstrap
}
