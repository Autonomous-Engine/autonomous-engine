package leaf

import "testing"

func TestLeaf(t *testing.T) {
	if Leaf() != 1 {
		t.Fatal("leaf")
	}
}
