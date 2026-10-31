package pricing

import "testing"

func TestDiscount(t *testing.T) {
	for _, total := range []float64{50, 99.99, 100, 250} {
		got := Discount(total)
		t.Logf("Discount(%v) = %v", total, got)
	}
}
