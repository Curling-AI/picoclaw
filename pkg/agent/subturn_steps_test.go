package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// oneToolCallProvider calls the note tool once, then answers.
type oneToolCallProvider struct{ calls atomic.Int32 }

func (p *oneToolCallProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{ReasoningContent: "which note?", ToolCalls: []providers.ToolCall{{
			ID:        "child-call",
			Type:      "function",
			Name:      "note",
			Arguments: map[string]any{"text": "olá"},
		}}}, nil
	}
	return &providers.LLMResponse{ReasoningContent: "noted, answering", Content: "done"}, nil
}

func (p *oneToolCallProvider) GetDefaultModel() string { return "test-model" }

// noteTool writes a note and keeps the id of the call it ran under.
type noteTool struct{ callID *string }

func (noteTool) Name() string               { return "note" }
func (noteTool) Description() string        { return "Writes a note" }
func (noteTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (n noteTool) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	if n.callID != nil {
		*n.callID = tools.ToolCallID(ctx)
	}
	return tools.NewToolResult("noted")
}

// The tool calls a sub-turn runs reach its OnStep, start and end, with the
// arguments the web shows for a tool call. A tool runs under its call's id:
// that is how the web finds the task a subagent call started.
func TestSpawnSubTurn_ReportsItsToolCallsAsSteps(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &oneToolCallProvider{})
	defer cleanup()
	var callID string
	agent.Tools.Register(noteTool{callID: &callID})

	var mu sync.Mutex
	var started []tools.SubTurnStep
	var ended []bool
	_, err := spawnSubTurn(context.Background(), al, newStopTestParent(agent), SubTurnConfig{
		Model:        "test-model",
		SystemPrompt: "write a note",
		OnStep: func(step tools.SubTurnStep) func(bool) {
			mu.Lock()
			started = append(started, step)
			mu.Unlock()
			return func(failed bool) {
				mu.Lock()
				ended = append(ended, failed)
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var args map[string]string
	if len(started) != 1 || started[0].Name != "note" ||
		json.Unmarshal([]byte(started[0].Arguments), &args) != nil || args["text"] != "olá" {
		t.Fatalf("steps = %+v, want the note call with its arguments", started)
	}
	if len(ended) != 1 || ended[0] {
		t.Fatalf("ends = %v, want one successful end", ended)
	}
	if callID != "child-call" {
		t.Fatalf("the tool ran under call %q, want child-call", callID)
	}
}

// A subagent's thoughts do not reach the web conversation as the assistant's:
// its steps show under the subagent card instead.
func TestSpawnSubTurn_KeepsItsReasoningOutOfTheConversation(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &oneToolCallProvider{})
	defer cleanup()
	agent.Tools.Register(noteTool{})
	parent := newStopTestParent(agent)
	parent.opts.Dispatch.InboundContext = &bus.InboundContext{Channel: "grpc", ChatID: "web-chat"}

	msgBus, ok := al.bus.(*bus.MessageBus)
	if !ok {
		t.Fatalf("bus is %T", al.bus)
	}
	ctx, stop := context.WithCancel(context.Background())
	var thoughts atomic.Int32
	read := make(chan struct{})
	go func() {
		defer close(read)
		for {
			select {
			case msg := <-msgBus.OutboundChan():
				if msg.Context.Raw[metadataKeyMessageKind] == messageKindThought {
					thoughts.Add(1)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	_, err := spawnSubTurn(context.Background(), al, parent, SubTurnConfig{
		Model:        "test-model",
		SystemPrompt: "write a note",
	})
	stop()
	<-read
	if err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}
	if n := thoughts.Load(); n != 0 {
		t.Fatalf("%d thoughts reached the conversation, want none", n)
	}
}
