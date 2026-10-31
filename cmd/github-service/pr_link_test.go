package main

import "testing"

func TestLinkedBody_AppendsOnce(t *testing.T) {
	line := "**Paired consumer PR:** https://github.com/acme/web/pull/3 (web)"
	got, changed := linkedBody("Body.\n", line)
	if !changed || got != "Body.\n\n"+line {
		t.Fatalf("got %q changed=%t", got, changed)
	}
	again, changed := linkedBody(got, line)
	if changed || again != got {
		t.Fatalf("second append changed the body: %q", again)
	}
	if b, changed := linkedBody("", line); !changed || b != line {
		t.Fatalf("empty body: %q changed=%t", b, changed)
	}
	if b, changed := linkedBody("x", "  "); changed || b != "x" {
		t.Fatalf("blank line: %q changed=%t", b, changed)
	}
}
