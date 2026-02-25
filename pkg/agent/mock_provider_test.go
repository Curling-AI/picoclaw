package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type mockProvider struct {
	response string // configurable response; defaults to "Mock response"
}

func (m *mockProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	content := m.response
	if content == "" {
		content = "Mock response"
	}
	return &providers.LLMResponse{
		Content:   content,
		ToolCalls: []providers.ToolCall{},
	}, nil
}

func (m *mockProvider) GetDefaultModel() string {
	return "mock-model"
}
