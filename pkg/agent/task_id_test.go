package agent

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// taskIDRecorder answers each turn with one call to tool, then text, and keeps
// the task id every model call carried.
type taskIDRecorder struct {
	tool string
	mu   sync.Mutex
	ids  []string
}

func (p *taskIDRecorder) Chat(
	ctx context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, protocoltypes.TaskIDFromContext(ctx))
	if len(p.ids)%2 == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        fmt.Sprintf("call-%d", len(p.ids)),
			Type:      "function",
			Name:      p.tool,
			Arguments: map[string]any{},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *taskIDRecorder) GetDefaultModel() string { return "test-model" }

func (p *taskIDRecorder) taskIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...)
}

// Every model call between a message and its answer carries the same task id,
// and the next message starts another task.
func TestRunTurn_EachMessageIsOneTask(t *testing.T) {
	provider := &taskIDRecorder{tool: "note"}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	agent.Tools.Register(noteTool{})

	for _, message := range []string{"first", "second"} {
		opts := makeTestProcessOpts("agent:main:task-id")
		opts.UserMessage = message
		if _, err := al.runAgentLoop(context.Background(), agent, opts); err != nil {
			t.Fatalf("runAgentLoop(%q): %v", message, err)
		}
	}

	ids := provider.taskIDs()
	if len(ids) != 4 || ids[0] == "" || ids[0] != ids[1] || ids[2] != ids[3] || ids[0] == ids[2] {
		t.Fatalf("task ids = %q, want one per message, shared by all its calls", ids)
	}
}

// A sub-turn is part of the task of the turn that spawned it.
func TestSpawnSubTurn_KeepsTheParentTask(t *testing.T) {
	provider := &taskIDRecorder{tool: "note"}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	agent.Tools.Register(noteTool{})

	ctx := protocoltypes.WithTaskID(context.Background(), "task-parent")
	if _, err := spawnSubTurn(ctx, al, newStopTestParent(agent), SubTurnConfig{
		Model:        "test-model",
		SystemPrompt: "write a note",
	}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}

	if ids := provider.taskIDs(); len(ids) != 2 || ids[0] != "task-parent" || ids[1] != "task-parent" {
		t.Fatalf("task ids = %q, want the parent's on every call", ids)
	}
}

// The turn that reads a spawn's result answers the request that launched the
// spawn, so it stays in that task: only a new message starts another.
func TestAsyncResult_AnswersInTheTaskThatLaunchedIt(t *testing.T) {
	provider := &taskIDRecorder{tool: "late_async"}
	al, msgBus, _ := newSystemMessageTestLoop(t, provider)
	tool := &lateAsyncTool{}
	al.RegisterTool(tool)

	if _, err := al.runAgentLoop(context.Background(), al.registry.GetDefaultAgent(), processOptions{
		SessionKey:      conversationSession,
		Channel:         "grpc",
		ChatID:          "run-1",
		UserMessage:     "run async tool",
		DefaultResponse: defaultResponse,
	}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	tool.finish("background result")
	var result bus.InboundMessage
	select {
	case result = <-msgBus.InboundChan():
	case <-time.After(2 * time.Second):
		t.Fatal("async result was not published")
	}
	if _, err := al.processSystemMessage(context.Background(), result); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	ids := provider.taskIDs()
	if len(ids) != 4 || ids[0] == "" || slices.ContainsFunc(ids, func(id string) bool { return id != ids[0] }) {
		t.Fatalf("task ids = %q, want the launching turn's on every call", ids)
	}
}
