package pricing

// Discount returns the price to charge for an order total: 10% off from
// 100 up.
func Discount(total float64) float64 {
	if total >= 100 {
		return total * 0.9
	}
	return total
}
