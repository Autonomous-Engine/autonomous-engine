package gjson

import (
	"math"
	"testing"
)

// Held-out acceptance tests: out-of-range integers clamp to the nearest bound.

type benchIntCase struct {
	raw      string
	wantInt  int64
	wantUint uint64
}

var benchClampCases = []benchIntCase{
	// Exact bounds must be returned without float rounding.
	{"18446744073709551615", math.MaxInt64, math.MaxUint64},
	{"9223372036854775807", math.MaxInt64, 9223372036854775807},
	{"-9223372036854775808", math.MinInt64, 0},
	{"18446744073709551613", math.MaxInt64, 18446744073709551613},
	{"9223372036854775806", 9223372036854775806, 9223372036854775806},
	{"-9223372036854775807", -9223372036854775807, 0},
	// Out-of-range integers must clamp to the nearest bound.
	{"18446744073709551616", math.MaxInt64, math.MaxUint64},
	{"18446744073709551617", math.MaxInt64, math.MaxUint64},
	{"99999999999999999999", math.MaxInt64, math.MaxUint64},
	{"9223372036854775808", math.MaxInt64, 9223372036854775808},
	{"9223372036854775809", math.MaxInt64, 9223372036854775809},
	{"-9223372036854775809", math.MinInt64, 0},
	{"-99999999999999999999", math.MinInt64, 0},
}

func TestBenchNumberClamping(t *testing.T) {
	for _, tc := range benchClampCases {
		r := Get(`{"n":`+tc.raw+`}`, "n")
		if got := r.Int(); got != tc.wantInt {
			t.Errorf("Get(%s).Int() = %d, want %d", tc.raw, got, tc.wantInt)
		}
		if tc.raw[0] == '-' {
			// Uint of a negative number is not part of the specification.
			continue
		}
		if got := r.Uint(); got != tc.wantUint {
			t.Errorf("Get(%s).Uint() = %d, want %d", tc.raw, got, tc.wantUint)
		}
	}
}

func TestBenchNumericStringClamping(t *testing.T) {
	for _, tc := range benchClampCases {
		r := Get(`{"n":"`+tc.raw+`"}`, "n")
		if r.Type != String {
			t.Fatalf("expected a String result for %q, got %v", tc.raw, r.Type)
		}
		if got := r.Int(); got != tc.wantInt {
			t.Errorf("Get(%q).Int() = %d, want %d", tc.raw, got, tc.wantInt)
		}
		if tc.raw[0] == '-' {
			continue
		}
		if got := r.Uint(); got != tc.wantUint {
			t.Errorf("Get(%q).Uint() = %d, want %d", tc.raw, got, tc.wantUint)
		}
	}
}

func TestBenchSmallValuesUnchanged(t *testing.T) {
	cases := map[string]int64{"0": 0, "42": 42, "-42": -42, "1e3": 1000, "12.9": 12}
	for raw, want := range cases {
		if got := Get(`{"n":`+raw+`}`, "n").Int(); got != want {
			t.Errorf("Get(%s).Int() = %d, want %d", raw, got, want)
		}
	}
	if got := Get(`{"n":42}`, "n").Uint(); got != 42 {
		t.Errorf("Get(42).Uint() = %d, want 42", got)
	}
	if got := Get(`{"n":true}`, "n").Int(); got != 1 {
		t.Errorf("Get(true).Int() = %d, want 1", got)
	}
}
