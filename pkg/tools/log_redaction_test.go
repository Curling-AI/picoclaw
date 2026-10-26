package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// captureLog points the logger at a file for the duration of the test and
// returns a reader for what was written.
func captureLog(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "picoclaw.log")
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

func assertLogHasNoSecrets(t *testing.T, logs string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains %q:\n%s", secret, logs)
		}
	}
}

func TestToolRegistry_ExecuteWithContext_LogsMaskedArgsAndError(t *testing.T) {
	readLog := captureLog(t)
	r := NewToolRegistry()
	failing := newMockTool("exec", "runs a command")
	failing.result = ErrorResult("401 Unauthorized for Bearer abcdefghijklmnop1234")
	r.Register(failing)
	r.Register(newMockTool("memory", "keeps notes"))

	ctx := context.Background()
	r.ExecuteWithContext(ctx, "exec", map[string]any{"command": "curl -u admin:hunter2 https://x"}, "", "", nil)
	r.ExecuteWithContext(ctx, "memory", map[string]any{
		"action":  "update_section",
		"content": "Login: ana@example.com / hunter3",
	}, "", "", nil)

	logs := readLog()
	for _, line := range []string{"Tool execution started", "Tool execution failed", "update_section"} {
		if !strings.Contains(logs, line) {
			t.Fatalf("log is missing %q:\n%s", line, logs)
		}
	}
	assertLogHasNoSecrets(t, logs, "hunter2", "hunter3", "abcdefghijklmnop1234")
}

type scriptedToolCallProvider struct {
	calls    int
	toolCall providers.ToolCall
}

func (p *scriptedToolCallProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{p.toolCall}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *scriptedToolCallProvider) GetDefaultModel() string { return "test-model" }

func TestRunToolLoop_LogsMaskedToolCall(t *testing.T) {
	readLog := captureLog(t)
	registry := NewToolRegistry()
	registry.Register(newMockTool("memory", "keeps notes"))
	provider := &scriptedToolCallProvider{toolCall: providers.ToolCall{
		ID:        "call-1",
		Name:      "memory",
		Arguments: map[string]any{"action": "save_long_term", "content": "Senha do banco hunter4"},
	}}

	_, err := RunToolLoop(context.Background(), ToolLoopConfig{
		Provider:      provider,
		Model:         "test-model",
		Tools:         registry,
		MaxIterations: 3,
	}, []providers.Message{{Role: "user", Content: "save it"}}, "", "")
	if err != nil {
		t.Fatalf("RunToolLoop() error = %v", err)
	}

	logs := readLog()
	if !strings.Contains(logs, "Tool call: memory(") {
		t.Fatalf("log is missing the tool call line:\n%s", logs)
	}
	assertLogHasNoSecrets(t, logs, "hunter4")
}
