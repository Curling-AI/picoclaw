package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// streamCutFragment is what reached the agent when the gateway closed a stream
// after its first delta (prod, 2026-10-16): text, no finish_reason, no [DONE].
const streamCutFragment = "Cenas geradas. Agora o roteiro completo — vou escr"

func TestInterruptedStreamIsRetriedAndTheFragmentNeverShips(t *testing.T) {
	al, provider := newScriptedLoop(
		t,
		readFileCall("call_1"),
		providers.LLMResponse{
			Content:             streamCutFragment,
			FinishReason:        "stop",
			FinishReasonMissing: true,
			StreamInterrupted:   true,
		},
		providers.LLMResponse{Content: "Roteiro completo salvo em roteiro.md."},
	)

	resp, err := al.ProcessDirect(context.Background(), "gera o roteiro", "stream-cut-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Roteiro completo salvo em roteiro.md." {
		t.Errorf("response = %q, want the answer from the retried call", resp)
	}
	if got := provider.calls(); got != 3 {
		t.Errorf("provider calls = %d, want 3 (call, cut stream, retry)", got)
	}
	for _, m := range directSessionHistory(t, al) {
		if strings.Contains(m.Content, "vou escr") {
			t.Errorf("the cut fragment was persisted: %q", m.Content)
		}
	}
}

func TestInterruptedStreamRetryIsCapped(t *testing.T) {
	al, provider := newScriptedLoop(t,
		providers.LLMResponse{Content: streamCutFragment, StreamInterrupted: true},
		providers.LLMResponse{Content: streamCutFragment, StreamInterrupted: true},
	)

	resp, err := al.ProcessDirect(context.Background(), "gera o roteiro", "stream-cut-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != streamCutFragment {
		t.Errorf("response = %q, want the fragment once the retry is spent", resp)
	}
	if got := provider.calls(); got != 1+maxInterruptedStreamRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxInterruptedStreamRetries)
	}
}

func TestInterruptedStreamRetryStartsOverAfterARealCall(t *testing.T) {
	al, provider := newScriptedLoop(t,
		readFileCall("call_1"),
		providers.LLMResponse{Content: streamCutFragment, StreamInterrupted: true},
		readFileCallAt("call_2", "roteiro.md"),
		providers.LLMResponse{Content: streamCutFragment, StreamInterrupted: true},
		providers.LLMResponse{Content: "Roteiro completo salvo em roteiro.md."},
	)

	resp, err := al.ProcessDirect(context.Background(), "gera o roteiro", "stream-cut-reset-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Roteiro completo salvo em roteiro.md." {
		t.Errorf("response = %q, want the answer after the second cut was retried", resp)
	}
	if got := provider.calls(); got != 5 {
		t.Errorf("provider calls = %d, want 5 (call, cut, retry with a call, cut, retry)", got)
	}
}

// The cut-stream retry covers replies without a call; a call that made it out
// runs as before.
func TestInterruptedStreamWithAToolCallIsNotRetried(t *testing.T) {
	cut := readFileCall("call_1")
	cut.StreamInterrupted = true
	al, provider := newScriptedLoop(t,
		cut,
		providers.LLMResponse{Content: "Li o README."},
	)

	resp, err := al.ProcessDirect(context.Background(), "lê o readme", "stream-cut-tool-call-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Li o README." {
		t.Errorf("response = %q, want the answer after the tool ran", resp)
	}
	if got := provider.calls(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (the call ran, no re-roll)", got)
	}
}

func TestInterruptedStreamAtTheIterationCapDeliversTheFragment(t *testing.T) {
	al, provider := newScriptedLoopWithCap(t, 2,
		readFileCall("call_1"),
		providers.LLMResponse{Content: streamCutFragment, StreamInterrupted: true},
	)

	resp, err := al.ProcessDirect(context.Background(), "gera o roteiro", "stream-cut-at-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != streamCutFragment {
		t.Errorf("response = %q, want the fragment instead of the tool-limit message", resp)
	}
	if got := provider.calls(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (no retry at the cap)", got)
	}
}
