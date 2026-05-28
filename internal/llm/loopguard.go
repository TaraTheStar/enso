// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

// repetitionDetector is a streaming guard against degeneration loops —
// the failure mode where a model stops emitting EOS and instead repeats a
// short cycle ("...the the the", a duplicated line, a JSON fragment) until
// the context window fills. It watches a rolling tail of the generated
// text and trips when that tail collapses into a short unit repeated back
// to back.
//
// The check is intentionally cheap and rate-independent: it inspects at
// most maxTail runes and only every checkEvery runes, so cost per token is
// negligible regardless of how fast the server streams. Thresholds are
// tuned to ignore legitimately repetitive code (a column of imports, a
// table) — a real loop repeats the SAME short unit many times with no
// variation, which natural text and code do not sustain.
type repetitionDetector struct {
	tail  []rune
	since int
}

const (
	rdMaxTail       = 1024 // runes of history retained
	rdCheckEvery    = 64   // run the scan once per this many runes
	rdMinRepeatLen  = 48   // a cycle must span at least this many runes to count
	rdMinReps       = 4    // ...repeated at least this many times
	rdMaxCycleRunes = 128  // longest repeating unit we look for
)

func newRepetitionDetector() *repetitionDetector {
	return &repetitionDetector{tail: make([]rune, 0, rdMaxTail)}
}

// add feeds a streamed text fragment and reports whether the tail has
// degenerated into a repeating cycle.
func (d *repetitionDetector) add(s string) bool {
	for _, r := range s {
		d.tail = append(d.tail, r)
		d.since++
	}
	if len(d.tail) > rdMaxTail {
		d.tail = append(d.tail[:0], d.tail[len(d.tail)-rdMaxTail:]...)
	}
	if d.since < rdCheckEvery {
		return false
	}
	d.since = 0
	return d.cyclic()
}

// cyclic scans for a unit of length [1, rdMaxCycleRunes] whose back-to-back
// repetition forms the tail. A 1-rune unit must repeat rdMinRepeatLen times;
// a longer unit needs at least rdMinReps reps and rdMinRepeatLen total runes.
func (d *repetitionDetector) cyclic() bool {
	n := len(d.tail)
	if n < rdMinRepeatLen {
		return false
	}
	maxUnit := n / rdMinReps
	if maxUnit > rdMaxCycleRunes {
		maxUnit = rdMaxCycleRunes
	}
	for unit := 1; unit <= maxUnit; unit++ {
		reps := rdMinReps
		if unit*reps < rdMinRepeatLen {
			reps = (rdMinRepeatLen + unit - 1) / unit
		}
		span := unit * reps
		if span > n {
			continue
		}
		if isCyclicSuffix(d.tail, unit, span) {
			return true
		}
	}
	return false
}

// isCyclicSuffix reports whether the last `span` runes of b are the final
// `unit`-rune block repeated to fill `span`.
func isCyclicSuffix(b []rune, unit, span int) bool {
	suf := b[len(b)-span:]
	for i := unit; i < span; i++ {
		if suf[i] != suf[i-unit] {
			return false
		}
	}
	return true
}
