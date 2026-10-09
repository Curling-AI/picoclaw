package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const backgroundMarker = "sleep 30 # mst277-orphan"

// backgroundThenStallProvider starts a background job and then hangs until the
// sub-turn deadline, like the subagents that died at 20 minutes in prod.
type backgroundThenStallProvider struct {
	calls atomic.Int32
}

func (p *backgroundThenStallProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        "bg",
			Type:      "function",
			Name:      "exec",
			Arguments: map[string]any{"action": "run", "command": backgroundMarker, "background": "true"},
		}}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *backgroundThenStallProvider) GetDefaultModel() string { return "stall-model" }

func TestSpawnSubTurn_FailedChildStopsItsBackgroundProcesses(t *testing.T) {
	provider := &backgroundThenStallProvider{}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	execTool, err := tools.NewExecTool("", false)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}
	agent.Tools.Register(execTool)

	_, err = spawnSubTurn(context.Background(), al, newStopTestParent(agent), SubTurnConfig{
		Model:        "test-model",
		Tools:        []tools.Tool{},
		SystemPrompt: "import everything",
		Timeout:      1500 * time.Millisecond,
	})
	if !errors.Is(err, ErrSubTurnTimeout) {
		t.Fatalf("err = %v, want ErrSubTurnTimeout", err)
	}

	lister := execTool
	deadline := time.Now().Add(5 * time.Second)
	for {
		var listed struct {
			Sessions []tools.SessionInfo `json:"sessions"`
		}
		raw := lister.Execute(context.Background(), map[string]any{"action": "list"}).ForLLM
		if err := json.Unmarshal([]byte(raw), &listed); err != nil {
			t.Fatalf("list output %q: %v", raw, err)
		}
		running := false
		found := false
		for _, s := range listed.Sessions {
			if s.Command == backgroundMarker {
				found = true
				running = running || s.Status == "running"
			}
		}
		if !found {
			t.Fatal("the child's background job was never started")
		}
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead child's background job is still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

const handedOverMarker = "sleep 30 # mst277-handed-over"

type backgroundThenAnswerProvider struct {
	calls atomic.Int32
}

func (p *backgroundThenAnswerProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        "bg",
			Type:      "function",
			Name:      "exec",
			Arguments: map[string]any{"action": "run", "command": handedOverMarker, "background": "true"},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "servidor de pé em background"}, nil
}

func (p *backgroundThenAnswerProvider) GetDefaultModel() string { return "mock-model" }

// A child that finished may have handed its background job over in its answer.
func TestSpawnSubTurn_FinishedChildKeepsItsBackgroundProcesses(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &backgroundThenAnswerProvider{})
	defer cleanup()
	execTool, err := tools.NewExecTool("", false)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}
	agent.Tools.Register(execTool)

	if _, err = spawnSubTurn(context.Background(), al, newStopTestParent(agent), SubTurnConfig{
		Model:        "test-model",
		Tools:        []tools.Tool{},
		SystemPrompt: "suba o servidor",
		Timeout:      5 * time.Second,
	}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}

	var listed struct {
		Sessions []tools.SessionInfo `json:"sessions"`
	}
	raw := execTool.Execute(context.Background(), map[string]any{"action": "list"}).ForLLM
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		t.Fatalf("list output %q: %v", raw, err)
	}
	for _, s := range listed.Sessions {
		if s.Command == handedOverMarker {
			if s.Status != "running" {
				t.Fatalf("the finished child's job was stopped (status %q)", s.Status)
			}
			_ = execTool.Execute(context.Background(), map[string]any{"action": "kill", "sessionId": s.ID})
			return
		}
	}
	t.Fatal("the child's background job was never started")
}
