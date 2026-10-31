package api

import "testing"

func TestAPI(t *testing.T) {
	if API() != 4 {
		t.Fatal("api")
	}
}
