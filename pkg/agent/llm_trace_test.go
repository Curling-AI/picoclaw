package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// The gateway keys idempotency on the trace id, so a fallback call must not
// reuse the one the stream already spent — nor edit the caller's options.
func TestOptsWithFreshRequestID_DoesNotReuseOrMutate(t *testing.T) {
	base := map[string]any{llmRequestIDOption: "pc-original", "temperature": 0.7}

	next, id := optsWithFreshRequestID(base)

	if id == "" || id == "pc-original" {
		t.Fatalf("fresh id = %q, want a new one", id)
	}
	if next[llmRequestIDOption] != id {
		t.Fatalf("options carry %v, want %q", next[llmRequestIDOption], id)
	}
	if base[llmRequestIDOption] != "pc-original" {
		t.Fatalf("caller options mutated: %v", base[llmRequestIDOption])
	}
	if next["temperature"] != 0.7 {
		t.Fatalf("temperature lost in the clone: %v", next["temperature"])
	}
}

func TestNewLLMRequestID_IsUnique(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		id := newLLMRequestID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestResponseCompletionTokens_UnknownIsNegative(t *testing.T) {
	if got := responseCompletionTokens(nil); got != -1 {
		t.Fatalf("nil response = %d, want -1", got)
	}
	if got := responseCompletionTokens(&providers.LLMResponse{}); got != -1 {
		t.Fatalf("no usage = %d, want -1", got)
	}
	resp := &providers.LLMResponse{Usage: &providers.UsageInfo{CompletionTokens: 14}}
	if got := responseCompletionTokens(resp); got != 14 {
		t.Fatalf("completion tokens = %d, want 14", got)
	}
}
