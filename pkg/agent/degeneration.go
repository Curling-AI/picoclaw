package agent

import "unicode"

// A degenerate completion ends in the same block over and over — the model got
// stuck in a cycle and burned the rest of its budget on it. The thresholds are
// set so ordinary repetition never trips: a markdown rule or a table separator
// has no letters, and a real answer does not repeat a whole clause five times
// back to back.
const (
	degenerateTailWindow = 1600
	degenerateMinPeriod  = 12
	degenerateMaxPeriod  = 200
	degenerateMinRepeats = 5
	degenerateMinSpan    = 200
)

// looksDegenerate reports whether text ends in a block repeated back to back
// enough times to be a generation loop rather than prose.
func looksDegenerate(text string) bool {
	runes := []rune(text)
	if len(runes) > degenerateTailWindow {
		runes = runes[len(runes)-degenerateTailWindow:]
	}
	if len(runes) < degenerateMinPeriod*degenerateMinRepeats {
		return false
	}
	for period := degenerateMinPeriod; period <= degenerateMaxPeriod; period++ {
		if period*degenerateMinRepeats > len(runes) {
			break
		}
		block := runes[len(runes)-period:]
		if !hasLetter(block) {
			continue
		}
		repeats := 1
		for start := len(runes) - 2*period; start >= 0; start -= period {
			if !equalRunes(runes[start:start+period], block) {
				break
			}
			repeats++
		}
		if repeats >= degenerateMinRepeats && repeats*period >= degenerateMinSpan {
			return true
		}
	}
	return false
}

func hasLetter(rs []rune) bool {
	for _, r := range rs {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
