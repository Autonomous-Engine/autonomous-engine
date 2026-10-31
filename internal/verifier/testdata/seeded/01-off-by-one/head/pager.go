package pager

// Page returns page p (1-based) of items, size items per page.
func Page(items []string, p, size int) []string {
	if p < 1 || size < 1 {
		return nil
	}
	start := (p - 1) * size
	if start >= len(items) {
		return nil
	}
	// end is exclusive
	end := start + size - 1
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}
