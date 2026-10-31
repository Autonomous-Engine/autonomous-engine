package version

import (
	"database/sql"
	"database/sql/driver"
	"testing"
)

// Held-out acceptance tests for the database/sql integration.
//
// The interfaces are asserted at run time (not with compile-time `var _ sql.Scanner = ...`
// guards) so this file compiles against the unchanged package and the base revision fails
// on the missing behaviour instead of on a build error.

func benchScanner(t *testing.T, v *Version) sql.Scanner {
	t.Helper()
	s, ok := interface{}(v).(sql.Scanner)
	if !ok {
		t.Fatalf("*Version does not implement database/sql.Scanner")
	}
	return s
}

func benchValuer(t *testing.T, v *Version) driver.Valuer {
	t.Helper()
	val, ok := interface{}(v).(driver.Valuer)
	if !ok {
		t.Fatalf("*Version does not implement database/sql/driver.Valuer")
	}
	return val
}

func TestBenchScanString(t *testing.T) {
	v := &Version{}
	if err := benchScanner(t, v).Scan("v1.2.3-beta+build"); err != nil {
		t.Fatalf("Scan(string) returned error: %v", err)
	}
	want := Must(NewVersion("v1.2.3-beta+build"))
	if !v.Equal(want) {
		t.Fatalf("scanned version %q is not Equal to %q", v, want)
	}
	if v.String() != want.String() {
		t.Fatalf("scanned version prints %q, want %q", v.String(), want.String())
	}
	if v.Prerelease() != "beta" || v.Metadata() != "build" {
		t.Fatalf("scanned version lost prerelease/metadata: %q / %q", v.Prerelease(), v.Metadata())
	}
}

func TestBenchScanReplacesExistingValue(t *testing.T) {
	v := Must(NewVersion("9.9.9-old+meta"))
	if err := benchScanner(t, v).Scan("1.0.0"); err != nil {
		t.Fatalf("Scan(string) returned error: %v", err)
	}
	if v.String() != "1.0.0" || v.Prerelease() != "" || v.Metadata() != "" {
		t.Fatalf("Scan did not replace the receiver: got %q (pre %q, meta %q)", v.String(), v.Prerelease(), v.Metadata())
	}
}

func TestBenchScanNilLeavesReceiverUnchanged(t *testing.T) {
	v := Must(NewVersion("1.0.0"))
	if err := benchScanner(t, v).Scan(nil); err != nil {
		t.Fatalf("Scan(nil) returned error: %v", err)
	}
	if v.String() != "1.0.0" {
		t.Fatalf("Scan(nil) changed the receiver to %q", v.String())
	}
}

func TestBenchScanRejectsOtherTypes(t *testing.T) {
	for _, src := range []interface{}{int64(7), 1.5, true} {
		v := Must(NewVersion("1.0.0"))
		if err := benchScanner(t, v).Scan(src); err == nil {
			t.Errorf("Scan(%T) returned nil error, want an error", src)
		}
		if v.String() != "1.0.0" {
			t.Errorf("Scan(%T) changed the receiver to %q", src, v.String())
		}
	}
}

func TestBenchScanRejectsUnparsableString(t *testing.T) {
	v := &Version{}
	if err := benchScanner(t, v).Scan("not a version"); err == nil {
		t.Fatal("Scan(\"not a version\") returned nil error, want a parse error")
	}
}

func TestBenchValueIsCanonicalString(t *testing.T) {
	v := Must(NewVersion("v2.0.1"))
	val, err := benchValuer(t, v).Value()
	if err != nil {
		t.Fatalf("Value() returned error: %v", err)
	}
	s, ok := val.(string)
	if !ok {
		t.Fatalf("Value() returned %T, want string", val)
	}
	if s != "2.0.1" {
		t.Fatalf("Value() = %q, want %q", s, "2.0.1")
	}
}

func TestBenchValueScanRoundTrip(t *testing.T) {
	for _, in := range []string{"1.2.3", "v0.1.0-rc.1", "3.4.5+meta.7"} {
		orig := Must(NewVersion(in))
		val, err := benchValuer(t, orig).Value()
		if err != nil {
			t.Fatalf("Value() for %q: %v", in, err)
		}
		back := &Version{}
		if err := benchScanner(t, back).Scan(val); err != nil {
			t.Fatalf("Scan(Value()) for %q: %v", in, err)
		}
		if !back.Equal(orig) {
			t.Fatalf("round trip of %q produced %q", in, back)
		}
	}
}
