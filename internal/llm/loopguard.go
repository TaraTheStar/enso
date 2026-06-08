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

	// Novelty tracking, fed only by addReasoning (the chain-of-thought
	// channel). The cyclic check above catches a model that collapses
	// into emitting the SAME short unit verbatim; it cannot see a model
	// that re-deliberates the same plan in fresh wording ("...let me
	// reconsider... actually, reviewing again..."), which is lexically
	// novel turn-to-turn yet semantically a loop. That shape grinds a
	// reasoning model toward max_tokens for minutes. The novelty signal
	// catches it by measuring how much of a long reasoning window is
	// re-tread phrasing: when distinct shingles collapse far below the
	// total over a sustained span, the model has stopped making progress.
	rbuf      []rune // rolling window of recent reasoning runes
	rsince    int    // reasoning runes since the last novelty scan
	lowStreak int    // consecutive scans below the novelty floor
}

const (
	rdMaxTail       = 1024 // runes of history retained
	rdCheckEvery    = 64   // run the scan once per this many runes
	rdMinRepeatLen  = 48   // a cycle must span at least this many runes to count
	rdMinReps       = 4    // ...repeated at least this many times
	rdMaxCycleRunes = 128  // longest repeating unit we look for
)

// Novelty-detector thresholds. Deliberately conservative: a model must
// emit a full ntWindowRunes of reasoning before it can trip at all, and
// then sustain low novelty across ntPersist consecutive scans (~ntPersist
// × ntCheckEvery further runes). Legitimate hard reasoning that revisits a
// concept stays well above the floor because fresh wording yields fresh
// shingles; only genuine re-tread (heavy verbatim phrase reuse, even when
// interleaved or lightly edited so the cyclic check misses it) collapses
// the distinct-shingle ratio this far. Rate-independent: keyed off rune
// counts, not wall-clock, so the same values hold on fast and slow boxes.
const (
	ntShingleRunes = 24   // length of each hashed shingle
	ntStride       = 8    // advance between successive shingles (overlapping)
	ntWindowRunes  = 3072 // reasoning must reach this before novelty can trip
	ntCheckEvery   = 256  // runes of new reasoning between novelty scans
	ntNoveltyFloor = 0.30 // trip threshold: distinct/total shingles below this
	ntPersist      = 3    // consecutive sub-floor scans required to trip
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

// addReasoning feeds a fragment from the reasoning channel. It applies the
// same cyclic check as add (verbatim spirals are degenerate anywhere) and,
// additionally, the novelty check that catches re-tread deliberation the
// cyclic check is blind to. Returns true if either trips.
func (d *repetitionDetector) addReasoning(s string) bool {
	if d.add(s) {
		return true
	}
	for _, r := range s {
		d.rbuf = append(d.rbuf, r)
		d.rsince++
	}
	if len(d.rbuf) > ntWindowRunes {
		d.rbuf = append(d.rbuf[:0], d.rbuf[len(d.rbuf)-ntWindowRunes:]...)
	}
	// Only judge once a full window has accumulated and a scan is due, so
	// short or just-started reasoning is never penalized.
	if len(d.rbuf) < ntWindowRunes || d.rsince < ntCheckEvery {
		return false
	}
	d.rsince = 0
	if distinctShingleRatio(d.rbuf) < ntNoveltyFloor {
		d.lowStreak++
	} else {
		d.lowStreak = 0
	}
	return d.lowStreak >= ntPersist
}

// distinctShingleRatio returns the fraction of overlapping ntShingleRunes-
// long shingles in buf that are distinct. Near 1.0 for varied text; it
// collapses toward 0 as the same phrasing recurs. Shingles are hashed
// (FNV-1a) so the scan stays cheap regardless of window size.
func distinctShingleRatio(buf []rune) float64 {
	if len(buf) < ntShingleRunes {
		return 1
	}
	const (
		offset64 = 1469598103934665603
		prime64  = 1099511628211
	)
	seen := make(map[uint64]struct{})
	total := 0
	for i := 0; i+ntShingleRunes <= len(buf); i += ntStride {
		var h uint64 = offset64
		for _, r := range buf[i : i+ntShingleRunes] {
			h ^= uint64(r)
			h *= prime64
		}
		seen[h] = struct{}{}
		total++
	}
	if total == 0 {
		return 1
	}
	return float64(len(seen)) / float64(total)
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
