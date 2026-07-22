package agent

import "testing"

func TestEstimateTokensFromRunes(t *testing.T) {
	cases := map[int]int{
		0:    0,
		1:    1, // sub-4 runes still counts as at least 1 token
		3:    1,
		4:    1,
		400:  100,
		1000: 250,
	}
	for runes, want := range cases {
		if got := estimateTokensFromRunes(runes); got != want {
			t.Errorf("estimateTokensFromRunes(%d) = %d, want %d", runes, got, want)
		}
	}
	if got := estimateTokensFromRunes(-5); got != 0 {
		t.Errorf("negative runes should give 0, got %d", got)
	}
}

// The streamed-output counter must reflect only the current call: reset clears
// it, set records the cumulative length, and it survives concurrent reads.
func TestStreamedOutputRunesCounter(t *testing.T) {
	ts := &turnState{}
	if got := ts.getStreamedOutputRunes(); got != 0 {
		t.Fatalf("fresh counter = %d, want 0", got)
	}
	ts.setStreamedOutputRunes(120) // onChunk delivers cumulative length
	ts.setStreamedOutputRunes(340)
	if got := ts.getStreamedOutputRunes(); got != 340 {
		t.Errorf("counter = %d, want the latest cumulative 340", got)
	}
	ts.resetStreamedOutputRunes()
	if got := ts.getStreamedOutputRunes(); got != 0 {
		t.Errorf("after reset = %d, want 0", got)
	}
}
