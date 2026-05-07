package bugfix

// Sum returns the sum of the given integers.
func Sum(xs []int) int {
	total := 0
	// BUG: starts at index 1 instead of 0, dropping the first element.
	for i := 1; i < len(xs); i++ {
		total += xs[i]
	}
	return total
}
