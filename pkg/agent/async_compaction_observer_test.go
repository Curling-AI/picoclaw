package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type asyncCompactionProbeKey struct{}

type asyncCompactionProbeProvider struct {
	observations *atomic.Int64
	calls        atomic.Int64
	wrongOrder   atomic.Bool
	wrongContext atomic.Bool
	release      <-chan struct{}
	fail         bool
}

func (p *asyncCompactionProbeProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if p.observations.Load() == 0 {
		p.wrongOrder.Store(true)
	}
	if ctx.Err() != nil || ctx.Value(asyncCompactionProbeKey{}) != nil {
		p.wrongContext.Store(true)
	}
	p.calls.Add(1)
	if p.release != nil {
		<-p.release
	}
	if p.fail {
		return nil, errors.New("synthetic summarization failure")
	}
	return &providers.LLMResponse{Content: "Synthetic summary"}, nil
}

func (*asyncCompactionProbeProvider) GetDefaultModel() string { return "test-model" }

func asyncCompactionFixture(
	t *testing.T,
	provider *asyncCompactionProbeProvider,
) (*legacyContextManager, *AgentInstance, []providers.Message) {
	t.Helper()
	al := NewAgentLoop(testConfig(t), bus.NewMessageBus(), provider)
	t.Cleanup(func() { al.Close() })
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("default agent missing")
	}
	agent.ContextWindow, agent.SummarizeTokenPercent = 8000, 75
	agent.SummarizeMessageThreshold, agent.KeepLastMessages = 2, 4
	history := []providers.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
	}
	return &legacyContextManager{al: al}, agent, history
}

func TestLegacyCompactAsyncObserverPrecedesReturnAndProviderWithoutDuplicateLaunches(t *testing.T) {
	var observations atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	provider := &asyncCompactionProbeProvider{observations: &observations, release: release}
	m, agent, history := asyncCompactionFixture(t, provider)
	const key = "async-observer-session"
	agent.Sessions.SetHistory(key, history)
	defer func() {
		unblock()
		waitFor(t, "asynchronous compaction cleanup", func() bool { return !m.CompactionState(key).Active })
	}()
	// A canceled turn and its values must not become the background provider's
	// lifetime or billing context. Only the synchronous observer sees them.
	base, cancel := context.WithCancel(context.WithValue(t.Context(), asyncCompactionProbeKey{}, true))
	cancel()
	ctx := WithAsyncCompactionObserver(base, func(sessionKey string) {
		if sessionKey != key || !m.CompactionState(key).Active {
			t.Error("observer ran before admission or with the wrong session")
		}
		observations.Add(1)
	})
	request := &CompactRequest{SessionKey: key, Reason: ContextCompressReasonSummarize}
	if err := m.Compact(ctx, request); err != nil {
		t.Fatal(err)
	}
	if observations.Load() != 1 {
		t.Fatal("Compact returned before the launch was observed", observations.Load())
	}
	waitFor(t, "blocked summarization provider", func() bool { return provider.calls.Load() == 1 })
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			if err := m.Compact(ctx, request); err != nil {
				t.Error(err)
			}
		})
	}
	callers.Wait()
	if observations.Load() != 1 || provider.calls.Load() != 1 {
		t.Fatal("deduplicated Compact calls manufactured launches", observations.Load(), provider.calls.Load())
	}
	unblock()
	waitFor(t, "first compaction completion", func() bool { return !m.CompactionState(key).Active })
	// A later actual launch must be observed again after the entry is released.
	agent.Sessions.SetHistory(key, history)
	if err := m.Compact(ctx, request); err != nil {
		t.Fatal(err)
	}
	if observations.Load() != 2 {
		t.Fatal("later launch observation lost", observations.Load())
	}
	waitFor(t, "second compaction completion", func() bool { return !m.CompactionState(key).Active })
	if provider.calls.Load() != 2 || provider.wrongOrder.Load() || provider.wrongContext.Load() {
		t.Fatal(
			"observer order or background context changed",
			provider.calls.Load(),
			provider.wrongOrder.Load(),
			provider.wrongContext.Load(),
		)
	}
}

func TestLegacyCompactAsyncObserverIgnoresBelowThresholdAndSynchronousCompression(t *testing.T) {
	var observations atomic.Int64
	provider := &asyncCompactionProbeProvider{observations: &observations}
	m, agent, history := asyncCompactionFixture(t, provider)
	const key = "async-observer-small"
	agent.Sessions.SetHistory(key, history[:2])
	ctx := WithAsyncCompactionObserver(t.Context(), func(string) { observations.Add(1) })
	if err := m.Compact(ctx, &CompactRequest{SessionKey: key, Reason: ContextCompressReasonSummarize}); err != nil {
		t.Fatal(err)
	}
	agent.Sessions.SetHistory(key, history)
	if err := m.Compact(ctx, &CompactRequest{SessionKey: key, Reason: ContextCompressReasonProactive}); err != nil {
		t.Fatal(err)
	}
	if observations.Load() != 0 || provider.calls.Load() != 0 || m.CompactionState(key).Active {
		t.Fatal("non-asynchronous compression was observed")
	}
}

func TestLegacyCompactAsyncObserverStillObservesFastProviderFailure(t *testing.T) {
	var observations atomic.Int64
	provider := &asyncCompactionProbeProvider{observations: &observations, fail: true}
	m, agent, history := asyncCompactionFixture(t, provider)
	const key = "async-observer-failing"
	agent.Sessions.SetHistory(key, history)
	ctx := WithAsyncCompactionObserver(t.Context(), func(string) { observations.Add(1) })
	if err := m.Compact(ctx, &CompactRequest{SessionKey: key, Reason: ContextCompressReasonSummarize}); err != nil {
		t.Fatal(err)
	}
	defer waitFor(t, "failed compaction cleanup", func() bool { return !m.CompactionState(key).Active })
	if observations.Load() != 1 {
		t.Fatal("fast asynchronous failure escaped observation", observations.Load())
	}
	waitFor(t, "failed compaction cleanup", func() bool { return !m.CompactionState(key).Active })
	if provider.calls.Load() == 0 || provider.wrongOrder.Load() || observations.Load() != 1 {
		t.Fatal("failure retries lost or duplicated the launch observation")
	}
}
