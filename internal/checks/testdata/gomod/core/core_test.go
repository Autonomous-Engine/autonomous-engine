package core

import "testing"

func TestCore(t *testing.T) {
	if Core() != 2 {
		t.Fatal("core")
	}
}
