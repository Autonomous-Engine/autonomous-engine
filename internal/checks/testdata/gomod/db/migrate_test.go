package db

import "testing"

func TestMigrationsApply(t *testing.T) {
	if err := Migrate(); err != nil {
		t.Fatal(err)
	}
}
