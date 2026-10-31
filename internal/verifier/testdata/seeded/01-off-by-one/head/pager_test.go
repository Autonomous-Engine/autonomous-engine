package pager

import "testing"

func TestPageFirst(t *testing.T) {
	got := Page([]string{"a", "b", "c", "d", "e"}, 1, 2)
	if len(got) == 0 {
		t.Fatal("first page is empty")
	}
	if got[0] != "a" {
		t.Fatalf("first item = %q, want a", got[0])
	}
}

func TestPageOutOfRange(t *testing.T) {
	if got := Page([]string{"a"}, 3, 2); got != nil {
		t.Fatalf("page 3 = %v, want nil", got)
	}
}
