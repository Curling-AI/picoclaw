// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package providers

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// stubProvider is a minimal LLMProvider for testing the registry.
type stubProvider struct{ name string }

func (s *stubProvider) Chat(_ context.Context, _ []Message, _ []ToolDefinition, _ string, _ map[string]any) (*LLMResponse, error) {
	return &LLMResponse{Content: s.name}, nil
}

func (s *stubProvider) GetDefaultModel() string { return s.name }

func TestProviderRegistry_Get_Found(t *testing.T) {
	fallback := &stubProvider{name: "fallback"}
	registered := &stubProvider{name: "registered"}

	reg := NewProviderRegistry(fallback)
	reg.Register("gemini", "gemini-2.5-flash", registered)

	got := reg.Get("gemini", "gemini-2.5-flash")
	resp, _ := got.Chat(context.Background(), nil, nil, "", nil)
	if resp.Content != "registered" {
		t.Errorf("Get() returned %q provider, want %q", resp.Content, "registered")
	}
}

func TestProviderRegistry_Get_Fallback(t *testing.T) {
	fallback := &stubProvider{name: "fallback"}

	reg := NewProviderRegistry(fallback)

	got := reg.Get("openai", "gpt-4o")
	resp, _ := got.Chat(context.Background(), nil, nil, "", nil)
	if resp.Content != "fallback" {
		t.Errorf("Get() returned %q provider, want %q", resp.Content, "fallback")
	}
}

func TestProviderRegistry_BuildFromConfig(t *testing.T) {
	cfg := &config.Config{
		ModelList: []config.ModelConfig{
			{
				ModelName: "flash",
				Model:     "gemini/gemini-2.5-flash",
				APIKey:    "test-key",
			},
			{
				ModelName: "gpt",
				Model:     "openai/gpt-4o",
				APIKey:    "test-key",
			},
			{
				// Invalid entry — no model field; should be skipped.
				ModelName: "bad",
				Model:     "",
			},
		},
	}

	fallback := &stubProvider{name: "fallback"}
	reg := BuildProviderRegistry(cfg, fallback)

	// Registered entries should return non-fallback providers.
	geminiProvider := reg.Get("gemini", "gemini-2.5-flash")
	if geminiProvider == fallback {
		t.Error("expected gemini provider to be registered, got fallback")
	}

	openaiProvider := reg.Get("openai", "gpt-4o")
	if openaiProvider == fallback {
		t.Error("expected openai provider to be registered, got fallback")
	}

	// Different providers for different entries.
	if geminiProvider == openaiProvider {
		t.Error("expected gemini and openai providers to be different instances")
	}

	// Unknown entry returns fallback.
	unknownProvider := reg.Get("anthropic", "claude-opus")
	if unknownProvider != fallback {
		t.Error("expected unknown provider to return fallback")
	}
}
