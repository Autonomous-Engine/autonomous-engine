package slug

import "testing"

func TestSlugify(t *testing.T) {
	if got := Slugify("  Hello   World "); got != "hello-world" {
		t.Fatalf("got %q", got)
	}
}
