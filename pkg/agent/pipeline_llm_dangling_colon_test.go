package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// danglingColonReply is verbatim from prod (2026-10-16): iteration 32 of a
// spec run came back with this, stop and no tool call, and the user had to
// type "Segue" for the edit it announced to happen.
const danglingColonReply = "Continuando a task 4 — agora as edições cirúrgicas: rota no `App.tsx` e botão " +
	"\"Catálogo\" no Balcão:"

const secondDanglingColonReply = "Agora removo o hook temporário e publico:"

const danglingColonNudgeMarker = "ended on a colon"

// scriptedProvider answers each call with the next scripted response and
// records the messages every call carried.
type scriptedProvider struct {
	mu       sync.Mutex
	script   []providers.LLMResponse
	received [][]providers.Message
}

func (p *scriptedProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.received = append(p.received, append([]providers.Message(nil), messages...))
	n := len(p.received)
	if n > len(p.script) {
		return &providers.LLMResponse{Content: "script exhausted"}, nil
	}
	resp := p.script[n-1]
	return &resp, nil
}

func (p *scriptedProvider) GetDefaultModel() string { return "mock-model" }

func (p *scriptedProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.received)
}

// callCarried reports whether the n-th call (1-based) had a message holding
// all of the given fragments.
func (p *scriptedProvider) callCarried(n int, fragments ...string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 1 || n > len(p.received) {
		return false
	}
	for _, m := range p.received[n-1] {
		all := true
		for _, f := range fragments {
			if !strings.Contains(m.Content, f) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func readFileCall(id string) providers.LLMResponse {
	return readFileCallAt(id, "README.md")
}

func readFileCallAt(id, path string) providers.LLMResponse {
	return providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID:        id,
		Type:      "function",
		Name:      "read_file",
		Arguments: map[string]any{"path": path},
	}}}
}

func newScriptedLoop(t *testing.T, script ...providers.LLMResponse) (*AgentLoop, *scriptedProvider) {
	t.Helper()
	return newScriptedLoopWithCap(t, 0, script...)
}

// newScriptedLoopWithCap sets max_tool_iterations when maxIter > 0.
func newScriptedLoopWithCap(
	t *testing.T,
	maxIter int,
	script ...providers.LLMResponse,
) (*AgentLoop, *scriptedProvider) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	if maxIter > 0 {
		cfg.Agents.Defaults.MaxToolIterations = maxIter
	}
	provider := &scriptedProvider{script: script}
	return NewAgentLoop(cfg, bus.NewMessageBus(), provider), provider
}

func TestDanglingColonAfterAToolIsRetriedAndEachOccurrenceGetsItsOwnRetry(t *testing.T) {
	al, provider := newScriptedLoop(t,
		readFileCall("call_1"),
		providers.LLMResponse{Content: danglingColonReply},
		readFileCall("call_2"),
		providers.LLMResponse{Content: secondDanglingColonReply},
		providers.LLMResponse{Content: "Edições aplicadas e publicadas."},
	)

	resp, err := al.ProcessDirect(context.Background(), "segue com a task 4", "dangling-colon-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Edições aplicadas e publicadas." {
		t.Errorf("response = %q, want the answer after both retries", resp)
	}
	if got := provider.calls(); got != 5 {
		t.Fatalf("provider calls = %d, want 5 (call, colon, retry with a call, colon, retry)", got)
	}
	// The nudge quotes what was discarded: the model never sees that reply
	// again, and the quote is how it knows which step it was on.
	if !provider.callCarried(3, danglingColonNudgeMarker, "no Balcão:") {
		t.Error("the first retry went out without the nudge quoting the discarded reply")
	}
	if provider.callCarried(4, danglingColonNudgeMarker) {
		t.Error("the nudge kept riding along after the call it asked for went through")
	}
	if !provider.callCarried(5, danglingColonNudgeMarker, "publico:") {
		t.Error("the second occurrence was not retried with its own nudge")
	}

	history := directSessionHistory(t, al)
	if len(history) == 0 {
		t.Fatal("session history is empty — the loop below would be vacuous")
	}
	for _, m := range history {
		if strings.Contains(m.Content, danglingColonNudgeMarker) {
			t.Errorf("the corrective nudge was persisted: %q", m.Content)
		}
		if strings.Contains(m.Content, "no Balcão:") || strings.Contains(m.Content, "publico:") {
			t.Errorf("a discarded dangling reply was persisted: %q", m.Content)
		}
	}
}

// The turn already did real work: when the retry also stops on a colon, the
// reply is delivered instead of being swapped for the empty-response fallback.
func TestDanglingColonOutOfRetriesDeliversTheReply(t *testing.T) {
	al, provider := newScriptedLoop(t,
		readFileCall("call_1"),
		providers.LLMResponse{Content: danglingColonReply},
		providers.LLMResponse{Content: secondDanglingColonReply},
	)

	resp, err := al.ProcessDirect(context.Background(), "segue com a task 4", "dangling-colon-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != secondDanglingColonReply {
		t.Errorf("response = %q, want the retried reply delivered as is", resp)
	}
	if got := provider.calls(); got != 2+maxDanglingColonRetries {
		t.Errorf("provider calls = %d, want %d (call + original + capped retries)", got, 2+maxDanglingColonRetries)
	}
}

// Before any tool ran, a colon with no action verb is left alone: that stretch
// belongs to looksLikeUndeliveredAnnouncement, which wants the verb.
func TestDanglingColonBeforeAnyToolIsNotRetried(t *testing.T) {
	al, provider := newScriptedLoop(t,
		providers.LLMResponse{Content: "Os números batem com o dashboard:"},
	)

	resp, err := al.ProcessDirect(context.Background(), "os números batem?", "dangling-colon-no-tool-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Os números batem com o dashboard:" {
		t.Errorf("response = %q, want the reply untouched", resp)
	}
	if got := provider.calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (no retry before a tool ran)", got)
	}
}

func TestAnnouncementNudgeIsDroppedOnceTheCallArrives(t *testing.T) {
	al, provider := newScriptedLoop(t,
		providers.LLMResponse{Content: announcedToolCall},
		readFileCall("call_1"),
		providers.LLMResponse{Content: "o Fenix 9 traz mapas offline e ECG"},
	)

	if _, err := al.ProcessDirect(
		context.Background(),
		"resuma esse artigo pra mim",
		"announce-drop-nudge-session",
	); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := provider.calls(); got != 3 {
		t.Fatalf("provider calls = %d, want 3 (promise, retry with a call, answer)", got)
	}
	if !provider.callCarried(2, "announced an action") {
		t.Error("the retry went out without the announcement nudge")
	}
	if provider.callCarried(3, "announced an action") {
		t.Error("the announcement nudge kept telling the final call to perform an action that already ran")
	}
}

// A retry from the last allowed iteration would end the loop with no content,
// and the tool-limit fallback would replace the reply.
func TestDanglingColonAtTheIterationCapDeliversTheReply(t *testing.T) {
	al, provider := newScriptedLoopWithCap(t, 2,
		readFileCall("call_1"),
		providers.LLMResponse{Content: danglingColonReply},
	)

	resp, err := al.ProcessDirect(context.Background(), "segue com a task 4", "dangling-colon-at-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != danglingColonReply {
		t.Errorf("response = %q, want the reply delivered at the cap", resp)
	}
	if got := provider.calls(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (no retry at the cap)", got)
	}
}

// After the 85% wrap-up warning the model was told to deliver; a nudge to carry
// on with the next step would contradict it.
func TestDanglingColonStandsDownAfterTheWrapUpWarning(t *testing.T) {
	script := make([]providers.LLMResponse, 0, 9)
	for i := range 8 {
		script = append(script, readFileCallAt(fmt.Sprintf("call_%d", i), fmt.Sprintf("notes-%d.md", i)))
	}
	script = append(script, providers.LLMResponse{Content: "Entregue o que deu. Falta:"})
	al, provider := newScriptedLoopWithCap(t, 10, script...)

	resp, err := al.ProcessDirect(context.Background(), "segue com a task 4", "dangling-colon-wrap-up-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Entregue o que deu. Falta:" {
		t.Errorf("response = %q, want the wrap-up reply untouched", resp)
	}
	if got := provider.calls(); got != 9 {
		t.Errorf("provider calls = %d, want 9 (no retry after the wrap-up warning)", got)
	}
	if !provider.callCarried(9, "Tool-budget warning") {
		t.Fatal("the wrap-up warning never went out — the test would not cover the stand-down")
	}
}
