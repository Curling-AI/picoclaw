package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// oneToolCallProvider calls the note tool once, then answers.
type oneToolCallProvider struct{ calls atomic.Int32 }

func (p *oneToolCallProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        "child-call",
			Type:      "function",
			Name:      "note",
			Arguments: map[string]any{"text": "olá"},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *oneToolCallProvider) GetDefaultModel() string { return "test-model" }

type noteTool struct{}

func (noteTool) Name() string               { return "note" }
func (noteTool) Description() string        { return "Writes a note" }
func (noteTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (noteTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	return tools.NewToolResult("noted")
}

// The tool calls a sub-turn runs reach its OnStep, start and end, with the
// arguments the web shows for a tool call.
func TestSpawnSubTurn_ReportsItsToolCallsAsSteps(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &oneToolCallProvider{})
	defer cleanup()
	agent.Tools.Register(noteTool{})

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
}
