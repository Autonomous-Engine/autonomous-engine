package svc

import "testing"

func TestSvc(t *testing.T) {
	if Svc() != 3 {
		t.Fatal("svc")
	}
}
