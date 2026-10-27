package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// A password written in prose has no label for a pattern to catch, so only
// the per-argument redaction at the call site keeps it out of the log.
const proseSecret = "hunter5"

type secretArgsProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *secretArgsProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        "call-1",
			Name:      "echo_text",
			Arguments: map[string]any{"text": "hi", "content": "minha senha " + proseSecret},
		}}}, nil
	}
	return &providers.LLMResponse{Content: messages[len(messages)-1].Content}, nil
}

func (p *secretArgsProvider) GetDefaultModel() string { return "test-model" }

func captureAgentLog(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.log")
	prevLevel := logger.GetLevel()
	logger.DisableFileLogging()
	logger.SetLevel(logger.INFO)
	if err := logger.EnableFileLogging(path); err != nil {
		t.Fatalf("EnableFileLogging() error = %v", err)
	}
	t.Cleanup(func() {
		logger.DisableFileLogging()
		logger.SetLevel(prevLevel)
	})
	return func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		return string(data)
	}
}

func runTurnWithSecretArgs(t *testing.T, mountRespondHook bool) string {
	t.Helper()
	readLog := captureAgentLog(t)
	al, agent, cleanup := newHookTestLoop(t, &secretArgsProvider{})
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if mountRespondHook {
		if err := al.MountHook(NamedHook("respond-hook", &respondHook{
			respondTools: map[string]bool{"echo_text": true},
		})); err != nil {
			t.Fatalf("MountHook failed: %v", err)
		}
	}

	if _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run tool",
		DefaultResponse: defaultResponse,
	}); err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	return readLog()
}

func TestAgentLoop_ToolCallLogHidesArgumentValues(t *testing.T) {
	logs := runTurnWithSecretArgs(t, false)
	if !strings.Contains(logs, "Tool call: echo_text(") {
		t.Fatalf("log is missing the tool call line:\n%s", logs)
	}
	if strings.Contains(logs, proseSecret) {
		t.Fatalf("log contains %q:\n%s", proseSecret, logs)
	}
}

func TestAgentLoop_HookRespondToolCallLogHidesArgumentValues(t *testing.T) {
	logs := runTurnWithSecretArgs(t, true)
	if !strings.Contains(logs, "Tool call (hook respond): echo_text(") {
		t.Fatalf("log is missing the hook-respond tool call line:\n%s", logs)
	}
	if strings.Contains(logs, proseSecret) {
		t.Fatalf("log contains %q:\n%s", proseSecret, logs)
	}
}

type plainReplyProvider struct{ reply string }

func (p *plainReplyProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: p.reply}, nil
}

func (p *plainReplyProvider) GetDefaultModel() string { return "test-model" }

func TestAgentLoop_MessageAndResponseLogsCarrySizeOnly(t *testing.T) {
	readLog := captureAgentLog(t)
	al, agent, cleanup := newHookTestLoop(t, &plainReplyProvider{reply: "anotei: sua senha é hunter6"})
	defer cleanup()

	if _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "error: anota minha senha hunter7",
		DefaultResponse: defaultResponse,
	}); err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if _, err := al.processMessageResult(context.Background(), bus.InboundMessage{
		Channel:  "cli",
		ChatID:   "direct",
		SenderID: "user-1",
		Content:  "error: anota minha senha hunter8",
	}, nil); err != nil {
		t.Fatalf("processMessageResult failed: %v", err)
	}

	logs := readLog()
	for _, secret := range []string{"hunter6", "hunter7", "hunter8"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains %q:\n%s", secret, logs)
		}
	}
	for _, line := range []string{"Processing message from cli:user-1", "Response ready"} {
		if !strings.Contains(logs, line) {
			t.Fatalf("log is missing %q:\n%s", line, logs)
		}
	}
}
