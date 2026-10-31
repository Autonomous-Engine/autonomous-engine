package semver

import "testing"

// Held-out acceptance tests: Equal must tolerate nil on either side.

func benchEqualNoPanic(t *testing.T, name string, a, b *Version) (result bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: Equal panicked: %v", name, r)
		}
	}()
	return a.Equal(b)
}

func TestBenchEqualNilVersions(t *testing.T) {
	var a, b *Version
	if !benchEqualNoPanic(t, "nil.Equal(nil)", a, b) {
		t.Fatal("nil.Equal(nil) = false, want true")
	}

	v := MustParse("1.2.3")
	if benchEqualNoPanic(t, "nil.Equal(v)", a, v) {
		t.Fatal("nil.Equal(1.2.3) = true, want false")
	}
	if benchEqualNoPanic(t, "v.Equal(nil)", v, b) {
		t.Fatal("(1.2.3).Equal(nil) = true, want false")
	}
}

func TestBenchEqualNonNilUnchanged(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.3", "1.2.3", true},
		{"1.2.3+build.1", "1.2.3+build.2", true},
		{"3.2-beta", "3.2-beta", true},
		{"1.2.3", "1.2.4", false},
		{"1.2.3-alpha", "1.2.3", false},
		{"2.0.0", "1.9.9", false},
	}
	for _, tc := range cases {
		a, b := MustParse(tc.a), MustParse(tc.b)
		if got := a.Equal(b); got != tc.want {
			t.Errorf("(%s).Equal(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := b.Equal(a); got != tc.want {
			t.Errorf("(%s).Equal(%s) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}
