// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"strings"
	"testing"
)

// feed streams s through the detector one small chunk at a time (mimicking
// token-sized SSE deltas) and reports whether it tripped.
func feed(d *repetitionDetector, s string) bool {
	const chunk = 7 // arbitrary small, unaligned to any cycle length
	for i := 0; i < len(s); i += chunk {
		end := i + chunk
		if end > len(s) {
			end = len(s)
		}
		if d.add(s[i:end]) {
			return true
		}
	}
	return false
}

func TestRepetitionDetector_TripsOnSingleCharLoop(t *testing.T) {
	d := newRepetitionDetector()
	// The classic "...the the the" / whitespace spiral.
	if !feed(d, strings.Repeat("a", 600)) {
		t.Fatal("expected detector to trip on a long single-char run")
	}
}

func TestRepetitionDetector_TripsOnPhraseLoop(t *testing.T) {
	d := newRepetitionDetector()
	if !feed(d, strings.Repeat("I cannot help with that. ", 60)) {
		t.Fatal("expected detector to trip on a repeated phrase")
	}
}

func TestRepetitionDetector_TripsOnRepeatedLine(t *testing.T) {
	d := newRepetitionDetector()
	line := "console.log(\"debugging here\");\n"
	if !feed(d, strings.Repeat(line, 40)) {
		t.Fatal("expected detector to trip on a duplicated line spiral")
	}
}

func TestRepetitionDetector_IgnoresLegitProse(t *testing.T) {
	// Varied natural text should never trip — no short unit repeats.
	prose := `The quick brown fox jumps over the lazy dog. Sphinx of black
quartz, judge my vow. Pack my box with five dozen liquor jugs. How
vexingly quick daft zebras jump! The five boxing wizards jump quickly.
Jackdaws love my big sphinx of quartz. Waltz, bad nymph, for quick jigs
vex. Bright vixens jump; dozy fowl quack. Crazy Fredrick bought many
very exquisite opal jewels for the woman. We promptly judged antique
ivory buckles for the next prize at the county fair this autumn season.`
	d := newRepetitionDetector()
	if feed(d, strings.Repeat(prose, 3)) {
		t.Fatal("detector tripped on legitimate varied prose")
	}
}

func TestRepetitionDetector_IgnoresStructuredCode(t *testing.T) {
	// A block of imports / struct fields is locally repetitive (newlines,
	// indentation) but each line differs — must not trip.
	code := `	Model           string    ` + "`json:\"model\"`" + `
	Messages        []Message ` + "`json:\"messages\"`" + `
	Stream          bool      ` + "`json:\"stream\"`" + `
	Tools           []ToolDef ` + "`json:\"tools,omitempty\"`" + `
	Temperature     float64   ` + "`json:\"temperature,omitempty\"`" + `
	TopK            int       ` + "`json:\"top_k,omitempty\"`" + `
	TopP            float64   ` + "`json:\"top_p,omitempty\"`" + `
	MinP            float64   ` + "`json:\"min_p,omitempty\"`" + `
	PresencePenalty float64   ` + "`json:\"presence_penalty,omitempty\"`" + `
`
	d := newRepetitionDetector()
	if feed(d, code) {
		t.Fatal("detector tripped on legitimate struct-field block")
	}
}

func TestRepetitionDetector_IgnoresShortRepeat(t *testing.T) {
	// A handful of reps is normal (a short list, "ha ha ha"); only a
	// sustained spiral should trip. Below rdMinReps worth of a longer unit.
	d := newRepetitionDetector()
	if feed(d, "yes. yes. yes. ") {
		t.Fatal("detector tripped on a short, benign repeat")
	}
}
