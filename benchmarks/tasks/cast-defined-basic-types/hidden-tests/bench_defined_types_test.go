package cast_test

import (
	"testing"
	"time"

	"github.com/spf13/cast"
)

// Held-out acceptance tests: values of user-defined types whose underlying type is a basic
// type (bool, string, integer, float) convert like their underlying value.

type benchInt int
type benchInt16 int16
type benchUint8 uint8
type benchUint64 uint64
type benchFloat float64
type benchFloat32 float32
type benchBool bool
type benchString string

type benchLevel int

func (l benchLevel) String() string { return "level-" + cast.ToString(int(l)) }

type benchPoint struct{ X, Y int }

func TestBenchDefinedTypesToNumbers(t *testing.T) {
	if v, err := cast.ToIntE(benchInt(42)); err != nil || v != 42 {
		t.Errorf("ToIntE(benchInt(42)) = %v, %v; want 42, nil", v, err)
	}
	if v, err := cast.ToInt64E(benchInt16(-7)); err != nil || v != -7 {
		t.Errorf("ToInt64E(benchInt16(-7)) = %v, %v; want -7, nil", v, err)
	}
	if v, err := cast.ToInt32E(benchString("123")); err != nil || v != 123 {
		t.Errorf("ToInt32E(benchString(\"123\")) = %v, %v; want 123, nil", v, err)
	}
	if v, err := cast.ToUintE(benchUint8(200)); err != nil || v != 200 {
		t.Errorf("ToUintE(benchUint8(200)) = %v, %v; want 200, nil", v, err)
	}
	if v, err := cast.ToUint16E(benchUint64(65535)); err != nil || v != 65535 {
		t.Errorf("ToUint16E(benchUint64(65535)) = %v, %v; want 65535, nil", v, err)
	}
	if v, err := cast.ToFloat64E(benchFloat(1.25)); err != nil || v != 1.25 {
		t.Errorf("ToFloat64E(benchFloat(1.25)) = %v, %v; want 1.25, nil", v, err)
	}
	if v, err := cast.ToFloat32E(benchInt(3)); err != nil || v != 3 {
		t.Errorf("ToFloat32E(benchInt(3)) = %v, %v; want 3, nil", v, err)
	}
	if v, err := cast.ToIntE(benchBool(true)); err != nil || v != 1 {
		t.Errorf("ToIntE(benchBool(true)) = %v, %v; want 1, nil", v, err)
	}
	if _, err := cast.ToUintE(benchInt(-1)); err == nil {
		t.Errorf("ToUintE(benchInt(-1)) returned nil error; negative values must still be rejected")
	}
}

func TestBenchDefinedTypesToString(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{benchString("hello"), "hello"},
		{benchInt(-12), "-12"},
		{benchUint8(9), "9"},
		{benchFloat32(2.5), "2.5"},
		{benchBool(false), "false"},
	}
	for _, tc := range cases {
		if got, err := cast.ToStringE(tc.in); err != nil || got != tc.want {
			t.Errorf("ToStringE(%T(%v)) = %q, %v; want %q, nil", tc.in, tc.in, got, err, tc.want)
		}
	}
	// A defined type implementing fmt.Stringer keeps using its String method.
	if got := cast.ToString(benchLevel(3)); got != "level-3" {
		t.Errorf("ToString(benchLevel(3)) = %q, want %q (fmt.Stringer takes precedence)", got, "level-3")
	}
}

func TestBenchDefinedTypesToBoolAndDuration(t *testing.T) {
	if v, err := cast.ToBoolE(benchBool(true)); err != nil || !v {
		t.Errorf("ToBoolE(benchBool(true)) = %v, %v; want true, nil", v, err)
	}
	if v, err := cast.ToBoolE(benchString("true")); err != nil || !v {
		t.Errorf("ToBoolE(benchString(\"true\")) = %v, %v; want true, nil", v, err)
	}
	if v, err := cast.ToBoolE(benchInt(0)); err != nil || v {
		t.Errorf("ToBoolE(benchInt(0)) = %v, %v; want false, nil", v, err)
	}
	if v, err := cast.ToDurationE(benchString("90s")); err != nil || v != 90*time.Second {
		t.Errorf("ToDurationE(benchString(\"90s\")) = %v, %v; want 1m30s, nil", v, err)
	}
	if v, err := cast.ToDurationE(benchInt(1500)); err != nil || v != 1500*time.Nanosecond {
		t.Errorf("ToDurationE(benchInt(1500)) = %v, %v; want 1.5µs, nil", v, err)
	}
}

func TestBenchDefinedTypesPointersAndUnsupported(t *testing.T) {
	n := benchInt(8)
	if v, err := cast.ToIntE(&n); err != nil || v != 8 {
		t.Errorf("ToIntE(&benchInt(8)) = %v, %v; want 8, nil", v, err)
	}
	s := benchString("x")
	if v, err := cast.ToStringE(&s); err != nil || v != "x" {
		t.Errorf("ToStringE(&benchString(\"x\")) = %q, %v; want \"x\", nil", v, err)
	}
	if _, err := cast.ToIntE(benchPoint{1, 2}); err == nil {
		t.Errorf("ToIntE(struct) returned nil error, want an error")
	}
	if _, err := cast.ToStringE(benchPoint{1, 2}); err == nil {
		t.Errorf("ToStringE(struct) returned nil error, want an error")
	}
	// Built-in named types keep their existing handling.
	if v, err := cast.ToStringE(time.Duration(2 * time.Second)); err != nil || v != "2s" {
		t.Errorf("ToStringE(time.Duration(2s)) = %q, %v; want \"2s\", nil", v, err)
	}
}
