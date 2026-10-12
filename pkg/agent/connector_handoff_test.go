package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type handoffProvider struct{ calls int }

func TestConnectorHandoff_CancelledTurnDoesNotPersist(t *testing.T) {
	storage := t.TempDir()
	sessions := session.NewSessionManager(storage)
	ts := &turnState{agent: &AgentInstance{Sessions: sessions}, sessionKey: "cancelled"}
	exec := &turnExecution{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	control := finishToolHandoff(ctx, ts, exec, nil, []providers.ToolCall{{ID: "skipped"}})
	if control != ToolControlBreak || !errors.Is(exec.handoffError, context.Canceled) {
		t.Fatalf("cancelled handoff = %v, %v", control, exec.handoffError)
	}
	files, err := os.ReadDir(storage)
	if err != nil || len(files) != 0 || len(sessions.ListSessions()) != 0 {
		t.Fatalf("cancelled handoff wrote history: files=%v error=%v", files, err)
	}
}

func (p *handoffProvider) GetDefaultModel() string { return "test-model" }

func (p *handoffProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls > 1 {
		return nil, fmt.Errorf("LLM resumed after connector handoff")
	}
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{
		{ID: "before", Name: "before_card", Arguments: map[string]any{}},
		{ID: "card", Name: "request_connectors", Arguments: map[string]any{}},
		{ID: "after", Name: "after_card", Arguments: map[string]any{}},
	}}, nil
}

type handoffTool struct {
	name     string
	calls    int
	loop     *AgentLoop
	steering bool
	fail     bool
}

func (t *handoffTool) Name() string        { return t.name }
func (t *handoffTool) Description() string { return "Test handoff tool" }
func (t *handoffTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *handoffTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	t.calls++
	if t.name != "request_connectors" {
		return tools.NewToolResult("ordinary result")
	}
	if t.fail {
		result := tools.ErrorResult("Could not verify required connectors. No automation was saved.")
		result.EndTurn = true
		result.ForUser = "Nenhuma automação foi salva. Tente novamente depois."
		return result
	}
	if t.steering {
		if err := t.loop.Steer(providers.Message{Role: "user", Content: "queued user message"}); err != nil {
			return tools.ErrorResult(err.Error())
		}
	}
	return &tools.ToolResult{ForLLM: `{"type":"connectors_requested"}`, ResponseHandled: true, EndTurn: true}
}

func TestConnectorHandoff_ErrorStopsMixedBatch(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	p := &handoffProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), p)
	defer al.Close()
	al.registry.GetDefaultAgent().Sessions = session.NewSessionManager(
		filepath.Join(cfg.Agents.Defaults.Workspace, "sessions"),
	)
	before := &handoffTool{name: "before_card"}
	after := &handoffTool{name: "after_card"}
	blocked := &handoffTool{name: "request_connectors", fail: true}
	al.RegisterTool(before)
	al.RegisterTool(after)
	al.RegisterTool(blocked)
	_, err := al.ProcessDirect(context.Background(), "create a connector automation", "blocked-conversation")
	if err == nil || p.calls != 1 || before.calls != 1 || blocked.calls != 1 || after.calls != 0 {
		t.Fatalf(
			"error handoff continued: err=%v llm=%d before=%d blocked=%d after=%d",
			err,
			p.calls,
			before.calls,
			blocked.calls,
			after.calls,
		)
	}
	stored := session.NewSessionManager(filepath.Join(cfg.Agents.Defaults.Workspace, "sessions"))
	keys := stored.ListSessions()
	if len(keys) != 1 || len(stored.GetHistory(keys[0])) == 0 {
		t.Fatal("error handoff was not persisted to disk")
	}
	history := stored.GetHistory(keys[0])
	if last := history[len(history)-1]; last.Role != "assistant" ||
		last.Content != "Nenhuma automação foi salva. Tente novamente depois." {
		t.Fatalf("visible prerequisite error was not persisted: %+v", last)
	}
}

func TestConnectorHandoff_SaveFailureIsRetryable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	p := &handoffProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), p)
	defer al.Close()
	storage := filepath.Join(t.TempDir(), "blocked-storage")
	if err := os.WriteFile(storage, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	al.registry.GetDefaultAgent().Sessions = session.NewSessionManager(storage)
	after := &handoffTool{name: "after_card"}
	al.RegisterTool(&handoffTool{name: "before_card"})
	al.RegisterTool(&handoffTool{name: "request_connectors"})
	al.RegisterTool(after)
	_, err := al.ProcessDirect(context.Background(), "connect gmail", "save-failure")
	if err == nil || !strings.Contains(err.Error(), "save tool handoff") || p.calls != 1 || after.calls != 0 {
		t.Fatalf("failed persistence was reported as completed: err=%v llm=%d after=%d", err, p.calls, after.calls)
	}
}

func TestConnectorHandoff_StopsMixedBatchAndPreservesReceipt(t *testing.T) {
	for _, steering := range []bool{false, true} {
		t.Run(fmt.Sprint(steering), func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Agents.Defaults.Workspace = t.TempDir()
			cfg.Agents.Defaults.ModelName = "test-model"
			p := &handoffProvider{}
			al := NewAgentLoop(cfg, bus.NewMessageBus(), p)
			defer al.Close()
			al.registry.GetDefaultAgent().Sessions = session.NewSessionManager(
				filepath.Join(cfg.Agents.Defaults.Workspace, "sessions"),
			)
			before := &handoffTool{name: "before_card"}
			after := &handoffTool{name: "after_card"}
			card := &handoffTool{name: "request_connectors", loop: al, steering: steering}
			al.RegisterTool(before)
			al.RegisterTool(card)
			al.RegisterTool(after)
			response, err := al.ProcessDirect(context.Background(), "connect gmail", "conversation")
			if err != nil || response != "" {
				t.Fatalf("handoff: %q %v", response, err)
			}
			if p.calls != 1 || before.calls != 1 || card.calls != 1 || after.calls != 0 {
				t.Fatalf("calls: llm=%d before=%d card=%d after=%d", p.calls, before.calls, card.calls, after.calls)
			}
			stored := session.NewSessionManager(filepath.Join(cfg.Agents.Defaults.Workspace, "sessions"))
			keys := stored.ListSessions()
			if len(keys) != 1 {
				t.Fatalf("handoff session was not persisted to disk: %v", keys)
			}
			history := stored.GetHistory(keys[0])
			receipt, skipped := false, false
			for _, m := range history {
				if m.Role == "tool" && m.ToolCallID == "card" {
					receipt = m.Content == `{"type":"connectors_requested"}`
				}
				if m.Role == "tool" && m.ToolCallID == "after" {
					skipped = true
				}
			}
			if !receipt || !skipped {
				t.Fatalf("receipt or skipped tool absent: %+v", history)
			}
		})
	}
}
