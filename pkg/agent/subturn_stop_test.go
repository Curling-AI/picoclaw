package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// stallingToolCallProvider asks for a tool call until stallAt, then blocks until
// the context ends, like a model call cut by the sub-turn deadline.
type stallingToolCallProvider struct {
	mu      sync.Mutex
	calls   int
	stallAt int
	stalled chan struct{}
}

func (p *stallingToolCallProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call >= p.stallAt {
		if call == p.stallAt && p.stalled != nil {
			close(p.stalled)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &providers.LLMResponse{
		ToolCalls: []providers.ToolCall{{
			ID: "c", Type: "function", Name: "nonexistent_tool",
			Function: &providers.FunctionCall{Name: "nonexistent_tool", Arguments: "{}"},
		}},
	}, nil
}

func (p *stallingToolCallProvider) GetDefaultModel() string { return "stall-model" }

func newStopTestParent(agent *AgentInstance) *turnState {
	return &turnState{
		ctx:            context.Background(),
		turnID:         "parent-stop",
		agent:          agent,
		pendingResults: make(chan *tools.ToolResult, 4),
		concurrencySem: make(chan struct{}, testMaxConcurrentSubTurns),
	}
}

// The reported case: the deadline cut the child's model call and the parent read
// "LLM call failed after retries: context deadline exceeded", blamed the provider
// and relaunched the same task, which hit the same limit.
func TestSpawnSubTurn_DeadlineNamesTheLimitAndIterations(t *testing.T) {
	provider := &stallingToolCallProvider{stallAt: 3}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	_, err := spawnSubTurn(context.Background(), al, newStopTestParent(agent), SubTurnConfig{
		Model:        "test-model",
		Tools:        []tools.Tool{},
		SystemPrompt: "rewrite the whole project",
		Timeout:      150 * time.Millisecond,
	})

	if !errors.Is(err, ErrSubTurnTimeout) {
		t.Fatalf("err = %v, want ErrSubTurnTimeout", err)
	}
	msg := err.Error()
	for _, want := range []string{"after 3 iterations", "split"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "LLM call failed") {
		t.Errorf("error %q still reads as a provider failure", msg)
	}
}

func TestSubTurnTimeoutError_StatesTheLimitInMinutes(t *testing.T) {
	for _, tc := range []struct {
		limit time.Duration
		want  string
	}{
		{limit: 5 * time.Minute, want: "5 min limit"},
		{limit: 20 * time.Minute, want: "20 min limit"},
		{limit: 90 * time.Second, want: "1m30s limit"},
	} {
		err := &subTurnTimeoutError{limit: tc.limit, iterations: 78}
		if got := err.Error(); !strings.Contains(got, tc.want) || !strings.Contains(got, "after 78 iterations") {
			t.Errorf("limit %v: error %q, want %q and the iteration count", tc.limit, got, tc.want)
		}
	}
}

// A synchronous child has the parent blocked on it: when the parent's turn is
// canceled (stop, or a new message after it), the child must stop too instead
// of holding the parent until its own deadline.
func TestSpawnSubTurn_SyncChildStopsWithTheParent(t *testing.T) {
	provider := &stallingToolCallProvider{stallAt: 1, stalled: make(chan struct{})}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	parentCtx, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	done := make(chan error, 1)
	go func() {
		_, err := spawnSubTurn(parentCtx, al, newStopTestParent(agent), SubTurnConfig{
			Model:        "test-model",
			Tools:        []tools.Tool{},
			SystemPrompt: "long task",
			Timeout:      10 * time.Second,
		})
		done <- err
	}()

	<-provider.stalled
	cancelParent()

	select {
	case err := <-done:
		if !errors.Is(err, ErrSubTurnParentCanceled) {
			t.Fatalf("err = %v, want ErrSubTurnParentCanceled", err)
		}
		if errors.Is(err, ErrSubTurnTimeout) || strings.Contains(err.Error(), "LLM call failed") {
			t.Fatalf("err = %v, want it to read as a cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the synchronous child kept running after the parent was canceled")
	}
}

// Background spawns are meant to outlive the turn that launched them; the end
// of that turn cancels its context, which must not reach the child.
func TestSpawnSubTurn_AsyncChildOutlivesTheParentContext(t *testing.T) {
	provider := &startedSlowProvider{delay: 200 * time.Millisecond, started: make(chan struct{})}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	parentCtx, cancelParent := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := spawnSubTurn(parentCtx, al, newStopTestParent(agent), SubTurnConfig{
			Model:        "test-model",
			Tools:        []tools.Tool{},
			SystemPrompt: "background task",
			Async:        true,
			Critical:     true,
		})
		done <- err
	}()

	<-provider.started
	cancelParent()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("background child failed after the parent context ended: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background child did not finish")
	}
}

// startedSlowProvider answers after delay and signals its first call.
type startedSlowProvider struct {
	delay   time.Duration
	once    sync.Once
	started chan struct{}
}

func (p *startedSlowProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.once.Do(func() { close(p.started) })
	select {
	case <-time.After(p.delay):
		return &providers.LLMResponse{Content: "background work done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *startedSlowProvider) GetDefaultModel() string { return "slow-model" }

// delegatingProvider has the parent call the synchronous subagent tool, then
// stalls the child's model call until its context ends.
type delegatingProvider struct {
	mu      sync.Mutex
	calls   int
	stalled chan struct{}
}

func (p *delegatingProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	switch call {
	case 1:
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID: "call_sub", Type: "function", Name: "subagent",
			Function:  &providers.FunctionCall{Name: "subagent", Arguments: `{"task":"rewrite the project"}`},
			Arguments: map[string]any{"task": "rewrite the project"},
		}}}, nil
	case 2:
		close(p.stalled)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *delegatingProvider) GetDefaultModel() string { return "mock-model" }

// The web stop cancels the turn's context without a hard abort; the turn must
// end then, not when the child's 5-minute deadline fires.
func TestSubagentTool_CancelledTurnDoesNotWaitForTheChild(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.SubTurn.DefaultTimeoutMinutes = 1
	cfg.Tools.Subagent.Enabled = true
	provider := &delegatingProvider{stalled: make(chan struct{})}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	turnCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = al.runAgentLoop(turnCtx, al.registry.GetDefaultAgent(), processOptions{
			SessionKey:      "sk_v1_web_conversation",
			Channel:         "grpc",
			ChatID:          "run-1",
			UserMessage:     "rewrite the project",
			DefaultResponse: defaultResponse,
		})
	}()

	select {
	case <-provider.stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the parent never delegated to the subagent")
	}
	stop()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled turn is still waiting for its subagent")
	}
}
