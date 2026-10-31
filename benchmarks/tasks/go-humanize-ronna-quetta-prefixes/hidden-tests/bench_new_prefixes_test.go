package humanize

import (
	"math"
	"math/big"
	"testing"
)

// Held-out acceptance tests for the ronna/quetta/ronto/quecto prefixes.
//
// These tests use only API that already exists at the base revision, so they compile there
// and fail on the missing behaviour. The new exported big.Int sizes are checked separately
// in the bench_bigsizes package (run after this package by the fixture's test command).

func benchPow(base, exp int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(base), big.NewInt(exp), nil)
}

func TestBenchSIFormatsNewPrefixes(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{2.2e27, "2.2 RF"},
		{1e27, "1 RF"},
		{2.2e30, "2.2 QF"},
		{1e-27, "1 rF"},
		{5e-30, "5 qF"},
		// Existing prefixes are unchanged.
		{2.2e24, "2.2 YF"},
		{1e-24, "1 yF"},
		{1e6, "1 MF"},
	}
	for _, tc := range cases {
		if got := SI(tc.in, "F"); got != tc.want {
			t.Errorf("SI(%g, \"F\") = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := SIWithDigits(1.234e27, 1, "F"); got != "1.2 RF" {
		t.Errorf("SIWithDigits(1.234e27, 1, \"F\") = %q, want %q", got, "1.2 RF")
	}
	if v, p := ComputeSI(3e30); p != "Q" || math.Abs(v-3) > 1e-9 {
		t.Errorf("ComputeSI(3e30) = (%g, %q), want (3, \"Q\")", v, p)
	}
	if v, p := ComputeSI(4e-27); p != "r" || math.Abs(v-4) > 1e-9 {
		t.Errorf("ComputeSI(4e-27) = (%g, %q), want (4, \"r\")", v, p)
	}
}

func TestBenchParseSINewPrefixes(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"2.2 RF", 2.2e27},
		{"2.2RF", 2.2e27},
		{"1 QF", 1e30},
		{"1 rF", 1e-27},
		{"4 qF", 4e-30},
	}
	for _, tc := range cases {
		got, unit, err := ParseSI(tc.in)
		if err != nil {
			t.Errorf("ParseSI(%q) returned error: %v", tc.in, err)
			continue
		}
		if unit != "F" {
			t.Errorf("ParseSI(%q) unit = %q, want %q", tc.in, unit, "F")
		}
		if math.Abs(got-tc.want) > math.Abs(tc.want)*1e-9 {
			t.Errorf("ParseSI(%q) = %g, want %g", tc.in, got, tc.want)
		}
	}
}

func TestBenchBigBytesFormatNewSuffixes(t *testing.T) {
	r := benchPow(1000, 9)
	q := benchPow(1000, 10)
	ri := benchPow(1024, 9)
	qi := benchPow(1024, 10)
	cases := []struct {
		in   *big.Int
		fn   func(*big.Int) string
		want string
	}{
		{r, BigBytes, "1.0 RB"},
		{q, BigBytes, "1.0 QB"},
		{new(big.Int).Mul(BigYByte, big.NewInt(1500)), BigBytes, "1.5 RB"},
		{new(big.Int).Mul(BigYByte, big.NewInt(16093)), BigBytes, "16 RB"},
		{ri, BigIBytes, "1.0 RiB"},
		{qi, BigIBytes, "1.0 QiB"},
		{new(big.Int).Mul(BigYiByte, big.NewInt(2048)), BigIBytes, "2.0 RiB"},
		// Existing behaviour below the new prefixes is unchanged.
		{BigYByte, BigBytes, "1.0 YB"},
		{new(big.Int).Mul(BigZByte, big.NewInt(999)), BigBytes, "999 ZB"},
		{big.NewInt(82854982), BigBytes, "83 MB"},
		{BigYiByte, BigIBytes, "1.0 YiB"},
	}
	for _, tc := range cases {
		if got := tc.fn(tc.in); got != tc.want {
			t.Errorf("formatting %s = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBenchParseBigBytesNewSuffixes(t *testing.T) {
	r := benchPow(1000, 9)
	cases := []struct {
		in   string
		want *big.Int
	}{
		{"1 RB", r},
		{"1 rb", r},
		{"1 QB", benchPow(1000, 10)},
		{"1 RiB", benchPow(1024, 9)},
		{"1 qib", benchPow(1024, 10)},
		{"2 R", new(big.Int).Mul(r, big.NewInt(2))},
		{"1 YB", BigYByte},
	}
	for _, tc := range cases {
		got, err := ParseBigBytes(tc.in)
		if err != nil {
			t.Errorf("ParseBigBytes(%q) returned error: %v", tc.in, err)
			continue
		}
		if got.Cmp(tc.want) != 0 {
			t.Errorf("ParseBigBytes(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
