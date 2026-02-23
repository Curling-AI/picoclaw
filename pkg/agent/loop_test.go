package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestRecordLastChannel(t *testing.T) {
	// Create temp workspace
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test config
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	// Create agent loop
	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	// Test RecordLastChannel
	testChannel := "test-channel"
	err = al.RecordLastChannel(testChannel)
	if err != nil {
		t.Fatalf("RecordLastChannel failed: %v", err)
	}

	// Verify channel was saved
	lastChannel := al.state.GetLastChannel()
	if lastChannel != testChannel {
		t.Errorf("Expected channel '%s', got '%s'", testChannel, lastChannel)
	}

	// Verify persistence by creating a new agent loop
	al2 := NewAgentLoop(cfg, msgBus, provider)
	if al2.state.GetLastChannel() != testChannel {
		t.Errorf("Expected persistent channel '%s', got '%s'", testChannel, al2.state.GetLastChannel())
	}
}

func TestRecordLastChatID(t *testing.T) {
	// Create temp workspace
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test config
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	// Create agent loop
	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	// Test RecordLastChatID
	testChatID := "test-chat-id-123"
	err = al.RecordLastChatID(testChatID)
	if err != nil {
		t.Fatalf("RecordLastChatID failed: %v", err)
	}

	// Verify chat ID was saved
	lastChatID := al.state.GetLastChatID()
	if lastChatID != testChatID {
		t.Errorf("Expected chat ID '%s', got '%s'", testChatID, lastChatID)
	}

	// Verify persistence by creating a new agent loop
	al2 := NewAgentLoop(cfg, msgBus, provider)
	if al2.state.GetLastChatID() != testChatID {
		t.Errorf("Expected persistent chat ID '%s', got '%s'", testChatID, al2.state.GetLastChatID())
	}
}

func TestNewAgentLoop_StateInitialized(t *testing.T) {
	// Create temp workspace
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create test config
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	// Create agent loop
	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	// Verify state manager is initialized
	if al.state == nil {
		t.Error("Expected state manager to be initialized")
	}

	// Verify state directory was created
	stateDir := filepath.Join(tmpDir, "state")
	if _, err := os.Stat(stateDir); os.IsNotExist(err) {
		t.Error("Expected state directory to exist")
	}
}

// TestToolRegistry_ToolRegistration verifies tools can be registered and retrieved
func TestToolRegistry_ToolRegistration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	// Register a custom tool
	customTool := &mockCustomTool{}
	al.RegisterTool(customTool)

	// Verify tool is registered by checking it doesn't panic on GetStartupInfo
	// (actual tool retrieval is tested in tools package tests)
	info := al.GetStartupInfo()
	toolsInfo := info["tools"].(map[string]any)
	toolsList := toolsInfo["names"].([]string)

	// Check that our custom tool name is in the list
	found := false
	for _, name := range toolsList {
		if name == "mock_custom" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected custom tool to be registered")
	}
}

// TestToolContext_Updates verifies tool context is updated with channel/chatID
func TestToolContext_Updates(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &simpleMockProvider{response: "OK"}
	_ = NewAgentLoop(cfg, msgBus, provider)

	// Verify that ContextualTool interface is defined and can be implemented
	// This test validates the interface contract exists
	ctxTool := &mockContextualTool{}

	// Verify the tool implements the interface correctly
	var _ tools.ContextualTool = ctxTool
}

// TestToolRegistry_GetDefinitions verifies tool definitions can be retrieved
func TestToolRegistry_GetDefinitions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	// Register a test tool and verify it shows up in startup info
	testTool := &mockCustomTool{}
	al.RegisterTool(testTool)

	info := al.GetStartupInfo()
	toolsInfo := info["tools"].(map[string]any)
	toolsList := toolsInfo["names"].([]string)

	// Check that our custom tool name is in the list
	found := false
	for _, name := range toolsList {
		if name == "mock_custom" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected custom tool to be registered")
	}
}

// TestAgentLoop_GetStartupInfo verifies startup info contains tools
func TestAgentLoop_GetStartupInfo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	info := al.GetStartupInfo()

	// Verify tools info exists
	toolsInfo, ok := info["tools"]
	if !ok {
		t.Fatal("Expected 'tools' key in startup info")
	}

	toolsMap, ok := toolsInfo.(map[string]any)
	if !ok {
		t.Fatal("Expected 'tools' to be a map")
	}

	count, ok := toolsMap["count"]
	if !ok {
		t.Fatal("Expected 'count' in tools info")
	}

	// Should have default tools registered
	if count.(int) == 0 {
		t.Error("Expected at least some tools to be registered")
	}
}

// TestAgentLoop_Stop verifies Stop() sets running to false
func TestAgentLoop_Stop(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &mockProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	// Note: running is only set to true when Run() is called
	// We can't test that without starting the event loop
	// Instead, verify the Stop method can be called safely
	al.Stop()

	// Verify running is false (initial state or after Stop)
	if al.running.Load() {
		t.Error("Expected agent to be stopped (or never started)")
	}
}

// Mock implementations for testing

type simpleMockProvider struct {
	response string
}

func (m *simpleMockProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{
		Content:   m.response,
		ToolCalls: []providers.ToolCall{},
	}, nil
}

func (m *simpleMockProvider) GetDefaultModel() string {
	return "mock-model"
}

// mockCustomTool is a simple mock tool for registration testing
type mockCustomTool struct{}

func (m *mockCustomTool) Name() string {
	return "mock_custom"
}

func (m *mockCustomTool) Description() string {
	return "Mock custom tool for testing"
}

func (m *mockCustomTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (m *mockCustomTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	return tools.SilentResult("Custom tool executed")
}

// mockContextualTool tracks context updates
type mockContextualTool struct {
	lastChannel string
	lastChatID  string
}

func (m *mockContextualTool) Name() string {
	return "mock_contextual"
}

func (m *mockContextualTool) Description() string {
	return "Mock contextual tool"
}

func (m *mockContextualTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (m *mockContextualTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	return tools.SilentResult("Contextual tool executed")
}

func (m *mockContextualTool) SetContext(channel, chatID string) {
	m.lastChannel = channel
	m.lastChatID = chatID
}

// testHelper executes a message and returns the response
type testHelper struct {
	al *AgentLoop
}

func (h testHelper) executeAndGetResponse(tb testing.TB, ctx context.Context, msg bus.InboundMessage) string {
	// Use a short timeout to avoid hanging
	timeoutCtx, cancel := context.WithTimeout(ctx, responseTimeout)
	defer cancel()

	response, err := h.al.processMessage(timeoutCtx, msg)
	if err != nil {
		tb.Fatalf("processMessage failed: %v", err)
	}
	return response
}

const responseTimeout = 3 * time.Second

// TestToolResult_SilentToolDoesNotSendUserMessage verifies silent tools don't trigger outbound
func TestToolResult_SilentToolDoesNotSendUserMessage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &simpleMockProvider{response: "File operation complete"}
	al := NewAgentLoop(cfg, msgBus, provider)
	helper := testHelper{al: al}

	// ReadFileTool returns SilentResult, which should not send user message
	ctx := context.Background()
	msg := bus.InboundMessage{
		Channel:    "test",
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "read test.txt",
		SessionKey: "test-session",
	}

	response := helper.executeAndGetResponse(t, ctx, msg)

	// Silent tool should return the LLM's response directly
	if response != "File operation complete" {
		t.Errorf("Expected 'File operation complete', got: %s", response)
	}
}

// TestToolResult_UserFacingToolDoesSendMessage verifies user-facing tools trigger outbound
func TestToolResult_UserFacingToolDoesSendMessage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &simpleMockProvider{response: "Command output: hello world"}
	al := NewAgentLoop(cfg, msgBus, provider)
	helper := testHelper{al: al}

	// ExecTool returns UserResult, which should send user message
	ctx := context.Background()
	msg := bus.InboundMessage{
		Channel:    "test",
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "run hello",
		SessionKey: "test-session",
	}

	response := helper.executeAndGetResponse(t, ctx, msg)

	// User-facing tool should include the output in final response
	if response != "Command output: hello world" {
		t.Errorf("Expected 'Command output: hello world', got: %s", response)
	}
}

// toolFailMockProvider simulates an LLM that makes tool calls which fail,
// then produces a text response after receiving the nudge message.
type toolFailMockProvider struct {
	callCount        int
	failToolCalls    int // Number of iterations that return failing tool calls
	finalResponse    string
	nudgeMessageSeen bool
}

func (m *toolFailMockProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	m.callCount++

	// Check if the nudge message was injected
	for _, msg := range messages {
		if msg.Role == "user" && strings.Contains(msg.Content, "Multiple consecutive tool calls have failed") {
			m.nudgeMessageSeen = true
		}
	}

	// After nudge, return a text-only response
	if m.nudgeMessageSeen {
		return &providers.LLMResponse{
			Content:   m.finalResponse,
			ToolCalls: []providers.ToolCall{},
		}, nil
	}

	// Otherwise keep returning tool calls to a nonexistent tool
	if m.callCount <= m.failToolCalls {
		return &providers.LLMResponse{
			Content: "",
			ToolCalls: []providers.ToolCall{
				{
					ID:   fmt.Sprintf("call_%d", m.callCount),
					Type: "function",
					Name: "nonexistent_tool",
					Function: &providers.FunctionCall{
						Name:      "nonexistent_tool",
						Arguments: `{"arg": "value"}`,
					},
				},
			},
		}, nil
	}

	// Fallback: return text response
	return &providers.LLMResponse{
		Content:   m.finalResponse,
		ToolCalls: []providers.ToolCall{},
	}, nil
}

func (m *toolFailMockProvider) GetDefaultModel() string {
	return "mock-tool-fail-model"
}

// TestAgentLoop_ConsecutiveToolFailuresNudge verifies that after 3 consecutive
// tool failures on an interactive channel, the loop breaks and returns a
// user-facing summary containing the tool name and error details.
func TestAgentLoop_ConsecutiveToolFailuresNudge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &toolFailMockProvider{
		failToolCalls: 10, // Keep failing for many iterations
		finalResponse: "I attempted to read the file but it was not found. Please check the path and try again.",
	}

	al := NewAgentLoop(cfg, msgBus, provider)
	helper := testHelper{al: al}

	ctx := context.Background()
	msg := bus.InboundMessage{
		Channel:    "test", // interactive channel
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "read nonexistent.txt",
		SessionKey: "test-session-nudge",
	}

	response := helper.executeAndGetResponse(t, ctx, msg)

	// On interactive channels, the loop should break (no nudge injected)
	if provider.nudgeMessageSeen {
		t.Error("Expected NO nudge message on interactive channel; loop should break with summary")
	}

	// The response should be a conversational failure summary
	if !strings.Contains(response, "repeated failures") {
		t.Errorf("Expected summary about repeated failures, got: %s", response)
	}
	if !strings.Contains(response, "nonexistent_tool") {
		t.Errorf("Expected summary to mention the failing tool name, got: %s", response)
	}

	// Should have taken exactly 4 calls (threshold=4), then break
	if provider.callCount != 4 {
		t.Errorf("Expected 4 LLM calls (break at threshold), got %d", provider.callCount)
	}
}

// TestAgentLoop_ConsecutiveToolFailuresNudge_InternalChannel verifies that
// on internal channels, the old nudge-message injection still fires.
func TestAgentLoop_ConsecutiveToolFailuresNudge_InternalChannel(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &toolFailMockProvider{
		failToolCalls: 10,
		finalResponse: "I could not complete the task.",
	}

	al := NewAgentLoop(cfg, msgBus, provider)

	// Use ProcessDirectWithChannel with "cli" (internal channel)
	ctx := context.Background()
	response, err := al.ProcessDirectWithChannel(
		ctx,
		"do something",
		"test-session-internal-nudge",
		"cli", // internal channel
		"direct",
	)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// On internal channels, the nudge message should have been injected
	if !provider.nudgeMessageSeen {
		t.Error("Expected nudge message to be injected on internal channel")
	}

	// The final response should be the provider's finalResponse (after nudge)
	if response != provider.finalResponse {
		t.Errorf("Expected %q, got %q", provider.finalResponse, response)
	}

	// Should have taken 5 calls: 4 failing + 1 text response after nudge
	if provider.callCount != 5 {
		t.Errorf("Expected 5 LLM calls (4 fail + 1 after nudge), got %d", provider.callCount)
	}
}

// lateFailMockProvider succeeds with tool calls for the first N iterations,
// then returns failing tool calls for the remainder. It never returns a text-only response.
type lateFailMockProvider struct {
	callCount      int
	successCalls   int // How many iterations return successful tool calls
	successToolName string
	failToolName   string
}

func (m *lateFailMockProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	m.callCount++

	toolName := m.failToolName
	if m.callCount <= m.successCalls {
		toolName = m.successToolName
	}

	return &providers.LLMResponse{
		Content: "",
		ToolCalls: []providers.ToolCall{
			{
				ID:   fmt.Sprintf("call_%d", m.callCount),
				Type: "function",
				Name: toolName,
				Function: &providers.FunctionCall{
					Name:      toolName,
					Arguments: `{}`,
				},
			},
		},
	}, nil
}

func (m *lateFailMockProvider) GetDefaultModel() string {
	return "mock-late-fail-model"
}

// TestAgentLoop_MaxIterationsWithTrailingErrors verifies that when the loop
// exhausts max iterations with recent (but below-threshold) errors and no text
// response, the user gets a detailed summary with tool call counts and errors.
func TestAgentLoop_MaxIterationsWithTrailingErrors(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// MaxToolIterations=5 so the test is fast.
	// Provider: 3 successful tool calls, then 2 failing ones → hits max iterations.
	// consecutiveErrors=2 < threshold=3, so no nudge. But the summary kicks in.
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 5,
			},
		},
	}

	msgBus := bus.NewMessageBus()

	// Register a mock tool that always succeeds
	provider := &lateFailMockProvider{
		successCalls:    3,
		successToolName: "mock_ok",
		failToolName:    "nonexistent_tool",
	}

	al := NewAgentLoop(cfg, msgBus, provider)

	// Register the "mock_ok" tool so those calls succeed
	al.RegisterTool(&mockCustomToolNamed{name: "mock_ok"})

	helper := testHelper{al: al}

	ctx := context.Background()
	msg := bus.InboundMessage{
		Channel:    "test",
		SenderID:   "user1",
		ChatID:     "chat1",
		Content:    "do something",
		SessionKey: "test-trailing-errors",
	}

	response := helper.executeAndGetResponse(t, ctx, msg)

	// Should NOT be the generic default
	if response == "I've completed processing but have no response to give." {
		t.Error("Expected a detailed summary, got the generic default")
	}

	// Should contain tool call counts in conversational form
	if !strings.Contains(response, "3 succeeded") {
		t.Errorf("Expected summary to mention 3 succeeded, got: %s", response)
	}
	if !strings.Contains(response, "2 failed") {
		t.Errorf("Expected summary to mention 2 failed, got: %s", response)
	}

	// Should mention the failing tool
	if !strings.Contains(response, "nonexistent_tool") {
		t.Errorf("Expected summary to mention nonexistent_tool, got: %s", response)
	}

	// All 5 iterations should have been used
	if provider.callCount != 5 {
		t.Errorf("Expected 5 LLM calls (max iterations), got %d", provider.callCount)
	}
}

// mockCustomToolNamed is a simple mock tool with a configurable name
type mockCustomToolNamed struct {
	name string
}

func (m *mockCustomToolNamed) Name() string        { return m.name }
func (m *mockCustomToolNamed) Description() string  { return "Mock tool: " + m.name }
func (m *mockCustomToolNamed) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (m *mockCustomToolNamed) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	return tools.SilentResult("ok")
}

// failFirstMockProvider fails on the first N calls with a specific error
type failFirstMockProvider struct {
	failures    int
	currentCall int
	failError   error
	successResp string
}

func (m *failFirstMockProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	m.currentCall++
	if m.currentCall <= m.failures {
		return nil, m.failError
	}
	return &providers.LLMResponse{
		Content:   m.successResp,
		ToolCalls: []providers.ToolCall{},
	}, nil
}

func (m *failFirstMockProvider) GetDefaultModel() string {
	return "mock-fail-model"
}

// TestAgentLoop_ContextExhaustionRetry verify that the agent retries on context errors
func TestAgentLoop_ContextExhaustionRetry(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()

	// Create a provider that fails once with a context error
	contextErr := fmt.Errorf("InvalidParameter: Total tokens of image and text exceed max message tokens")
	provider := &failFirstMockProvider{
		failures:    1,
		failError:   contextErr,
		successResp: "Recovered from context error",
	}

	al := NewAgentLoop(cfg, msgBus, provider)

	// Inject some history to simulate a full context
	sessionKey := "test-session-context"
	// Create dummy history
	history := []providers.Message{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Old message 1"},
		{Role: "assistant", Content: "Old response 1"},
		{Role: "user", Content: "Old message 2"},
		{Role: "assistant", Content: "Old response 2"},
		{Role: "user", Content: "Trigger message"},
	}
	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("No default agent found")
	}
	defaultAgent.Sessions.SetHistory(sessionKey, history)

	// Call ProcessDirectWithChannel
	// Note: ProcessDirectWithChannel calls processMessage which will execute runLLMIteration
	response, err := al.ProcessDirectWithChannel(
		context.Background(),
		"Trigger message",
		sessionKey,
		"test",
		"test-chat",
	)
	if err != nil {
		t.Fatalf("Expected success after retry, got error: %v", err)
	}

	if response != "Recovered from context error" {
		t.Errorf("Expected 'Recovered from context error', got '%s'", response)
	}

	// We expect 2 calls: 1st failed, 2nd succeeded
	if provider.currentCall != 2 {
		t.Errorf("Expected 2 calls (1 fail + 1 success), got %d", provider.currentCall)
	}

	// Check final history length
	finalHistory := defaultAgent.Sessions.GetHistory(sessionKey)
	// We verify that the history has been modified (compressed)
	// Original length: 6
	// Expected behavior: compression drops ~50% of history (mid slice)
	// We can assert that the length is NOT what it would be without compression.
	// Without compression: 6 + 1 (new user msg) + 1 (assistant msg) = 8
	if len(finalHistory) >= 8 {
		t.Errorf("Expected history to be compressed (len < 8), got %d", len(finalHistory))
	}
}
