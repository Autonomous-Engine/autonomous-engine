package tools_test

import (
	"testing"

	"example.com/fixture/core"
)

func TestTools(t *testing.T) {
	if core.Core() != 2 {
		t.Fatal("tools")
	}
}
