package main

import (
	"strings"
	"testing"
)

func envWith(v string) func(string) string {
	return func(k string) string {
		if k == "JWT_SECRET" {
			return v
		}
		return ""
	}
}

func TestLoadJWTSecretRequiresValue(t *testing.T) {
	_, err := loadJWTSecret(envWith(""))
	if err == nil || !strings.Contains(err.Error(), "JWT_SECRET") || !strings.Contains(err.Error(), "32") {
		t.Fatalf("want an error naming JWT_SECRET and the 32-byte minimum, got %v", err)
	}
}

func TestLoadJWTSecretRejectsShort(t *testing.T) {
	if _, err := loadJWTSecret(envWith(strings.Repeat("x", 31))); err == nil {
		t.Fatal("31-byte secret accepted")
	}
	// The placeholder shipped in .env.example must not be usable as-is.
	if _, err := loadJWTSecret(envWith("change-me-to-a-long-random-string-in-production")); err == nil {
		t.Fatal(".env.example placeholder accepted")
	}
}

func TestLoadJWTSecretAccepts32(t *testing.T) {
	want := strings.Repeat("k", 32)
	got, err := loadJWTSecret(envWith(want))
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v", got, err)
	}
}
