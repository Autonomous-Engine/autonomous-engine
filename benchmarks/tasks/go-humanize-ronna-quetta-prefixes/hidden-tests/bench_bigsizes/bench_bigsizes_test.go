package bench_bigsizes_test

import (
	"math/big"
	"testing"

	humanize "github.com/dustin/go-humanize"
)

// Held-out acceptance test for the new exported big byte sizes. This package references
// the new identifiers, so it only compiles once they exist; the fixture's test command runs
// the root package (behaviour) first.

func TestBenchBigByteSizes(t *testing.T) {
	thousand := big.NewInt(1000)
	k1024 := big.NewInt(1024)

	wantR := new(big.Int).Mul(humanize.BigYByte, thousand)
	if humanize.BigRByte.Cmp(wantR) != 0 {
		t.Errorf("BigRByte = %s, want %s", humanize.BigRByte, wantR)
	}
	wantQ := new(big.Int).Mul(wantR, thousand)
	if humanize.BigQByte.Cmp(wantQ) != 0 {
		t.Errorf("BigQByte = %s, want %s", humanize.BigQByte, wantQ)
	}
	wantRi := new(big.Int).Mul(humanize.BigYiByte, k1024)
	if humanize.BigRiByte.Cmp(wantRi) != 0 {
		t.Errorf("BigRiByte = %s, want %s", humanize.BigRiByte, wantRi)
	}
	wantQi := new(big.Int).Mul(wantRi, k1024)
	if humanize.BigQiByte.Cmp(wantQi) != 0 {
		t.Errorf("BigQiByte = %s, want %s", humanize.BigQiByte, wantQi)
	}
	if got := humanize.BigBytes(humanize.BigQByte); got != "1.0 QB" {
		t.Errorf("BigBytes(BigQByte) = %q, want %q", got, "1.0 QB")
	}
	if got := humanize.BigIBytes(humanize.BigRiByte); got != "1.0 RiB" {
		t.Errorf("BigIBytes(BigRiByte) = %q, want %q", got, "1.0 RiB")
	}
}
