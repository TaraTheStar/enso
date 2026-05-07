package slowthing

// Dedupe returns xs with duplicates removed, preserving first-seen order.
// The current implementation is O(n^2) — there are several reasonable
// directions to take "make this faster" (map-backed set, sort+compact,
// concurrent worker pool for very large inputs, ...).
func Dedupe(xs []string) []string {
	out := []string{}
	for _, x := range xs {
		seen := false
		for _, y := range out {
			if x == y {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, x)
		}
	}
	return out
}
