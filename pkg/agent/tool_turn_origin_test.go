package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestToolTurnOrigin_DelegationKeepsRootAuthorizationAfterParentEnds(t *testing.T) {
	root := &turnState{sessionKey: "parent", chatID: "root-run", userMessage: "user connector decisions"}
	root.isFinished.Store(true)
	child := &turnState{sessionKey: "child", chatID: "child-run", userMessage: "model-generated prompt", parentTurnID: "root", parentTurnState: root, depth: 1}
	nested := &turnState{userMessage: "another generated prompt", parentTurnID: "child", parentTurnState: child, depth: 2}
	for _, ts := range []*turnState{root, child, nested} {
		ctx := withToolTurnOrigin(context.Background(), ts)
		message, ok := RootTurnUserMessage(ctx)
		origin := ToolOrigin(ctx)
		if !ok || message != root.userMessage || origin.SessionKey != root.sessionKey || origin.ChatID != root.chatID {
			t.Fatalf("delegated authorization was lost: %+v", origin)
		}
		if IsSubTurn(ctx) != (ts != root) {
			t.Fatalf("subturn marker = %v", IsSubTurn(ctx))
		}
	}
}

func TestToolTurnOrigin_MissingParentRemainsUnresolved(t *testing.T) {
	ts := &turnState{parentTurnID: "missing", depth: 1, userMessage: "forged root decisions"}
	ctx := withToolTurnOrigin(context.Background(), ts)
	if message, ok := RootTurnUserMessage(ctx); ok || message != "" || !IsSubTurn(ctx) {
		t.Fatalf("missing ancestry trusted the child: %q %v", message, ok)
	}
}

type subTurnRequestMetadataKey struct{}

type subTurnContextProvider struct {
	entered chan context.Context
	release chan struct{}
}

func (p *subTurnContextProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.entered <- ctx
	select {
	case <-p.release:
		return &providers.LLMResponse{Content: "sub-turn-complete"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *subTurnContextProvider) GetDefaultModel() string { return "fixture-model" }

func TestSpawnSubTurnPreservesRequestMetadataWithoutParentCancellation(t *testing.T) {
	provider := &subTurnContextProvider{
		entered: make(chan context.Context, 1),
		release: make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(provider.release) })
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "fixture-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	parentCtx, cancelParent := context.WithTimeout(
		context.WithValue(context.Background(), subTurnRequestMetadataKey{}, "connector-capability"),
		time.Minute,
	)
	defer cancelParent()
	parentDeadline, _ := parentCtx.Deadline()
	parent := &turnState{
		ctx:            parentCtx,
		turnID:         "parent-metadata",
		depth:          0,
		agent:          al.registry.GetDefaultAgent(),
		session:        newEphemeralSession(nil),
		pendingResults: make(chan *tools.ToolResult, 4),
		concurrencySem: make(chan struct{}, testMaxConcurrentSubTurns),
	}
	type outcome struct {
		result *tools.ToolResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		defer close(finished)
		result, err := spawnSubTurn(parentCtx, al, parent, SubTurnConfig{
			Model:        "fixture-model",
			SystemPrompt: "Run independent work with the parent's request metadata.",
			Timeout:      2 * time.Minute,
		})
		finished <- outcome{result: result, err: err}
	}()
	t.Cleanup(func() {
		release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("child did not stop during cleanup")
		}
	})

	var childCtx context.Context
	select {
	case childCtx = <-provider.entered:
	case got := <-finished:
		t.Fatalf("child finished before reaching the provider: %v", got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("child did not reach the provider")
	}
	if got := childCtx.Value(subTurnRequestMetadataKey{}); got != "connector-capability" {
		t.Errorf("child request metadata = %v, want connector-capability", got)
	}
	childDeadline, ok := childCtx.Deadline()
	if !ok || !childDeadline.After(parentDeadline) {
		t.Errorf("child must retain its own later deadline: child=%v parent=%v", childDeadline, parentDeadline)
	}

	// The provider is still in Chat: parent cancellation must leave this work active.
	cancelParent()
	if parentCtx.Err() != context.Canceled {
		t.Fatalf("parent cancellation was not observed: %v", parentCtx.Err())
	}
	if err := childCtx.Err(); err != nil {
		t.Errorf("parent cancellation reached the child: %v", err)
	}
	release()
	select {
	case got := <-finished:
		if got.err != nil {
			t.Fatalf("independent child failed after parent cancellation: %v", got.err)
		}
		if got.result == nil || got.result.ForLLM != "sub-turn-complete" {
			t.Errorf("independent child result = %#v", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("independent child did not finish after its provider was released")
	}
}
