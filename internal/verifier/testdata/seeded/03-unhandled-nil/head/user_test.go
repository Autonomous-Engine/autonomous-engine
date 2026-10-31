package user

import "testing"

func TestDisplayName(t *testing.T) {
	if got := DisplayName(&User{First: "Ada", Last: "Lovelace"}); got != "Ada Lovelace" {
		t.Fatalf("got %q", got)
	}
	if got := DisplayName(&User{}); got != "anonymous" {
		t.Fatalf("empty user: got %q", got)
	}
}
