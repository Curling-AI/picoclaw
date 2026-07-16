package agent

import "testing"

func TestTurnContextWindow(t *testing.T) {
	ts := &turnState{agent: &AgentInstance{ContextWindow: 1_048_576}}

	// A media turn set the image-model window: it wins over the main window.
	if got := turnContextWindow(ts, &turnExecution{effectiveContextWindow: 131_072}); got != 131_072 {
		t.Errorf("effective window set: got %d, want 131072", got)
	}
	// No media routing: fall back to the agent's default context window.
	if got := turnContextWindow(ts, &turnExecution{}); got != 1_048_576 {
		t.Errorf("effective window unset: got %d, want 1048576", got)
	}
	// Nil-safe.
	if got := turnContextWindow(nil, nil); got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
}
