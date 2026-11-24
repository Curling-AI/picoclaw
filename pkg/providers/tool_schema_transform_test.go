package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	providercommon "github.com/sipeed/picoclaw/pkg/providers/common"
)

type toolCaptureProvider struct {
	lastTools []ToolDefinition
}

func (p *toolCaptureProvider) Chat(
	_ context.Context,
	_ []Message,
	tools []ToolDefinition,
	_ string,
	_ map[string]any,
) (*LLMResponse, error) {
	p.lastTools = tools
	return &LLMResponse{Content: "ok"}, nil
}

func (p *toolCaptureProvider) GetDefaultModel() string {
	return "test"
}

func TestWrapProviderWithToolSchemaTransform_DisabledPassesToolsThrough(t *testing.T) {
	capture := &toolCaptureProvider{}
	wrapped, err := wrapProviderWithToolSchemaTransform(capture, "")
	if err != nil {
		t.Fatalf("wrapProviderWithToolSchemaTransform() error = %v", err)
	}

	tools := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:       "noop",
			Parameters: map[string]any{"type": "object"},
		},
	}}

	_, err = wrapped.Chat(t.Context(), nil, tools, "test", nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if !reflect.DeepEqual(capture.lastTools, tools) {
		t.Fatalf("tools mutated with transform off\n got: %#v\nwant: %#v", capture.lastTools, tools)
	}
}

func TestWrapProviderWithToolSchemaTransform_GoogleSanitizesSchemas(t *testing.T) {
	capture := &toolCaptureProvider{}
	wrapped, err := wrapProviderWithToolSchemaTransform(capture, "simple")
	if err != nil {
		t.Fatalf("wrapProviderWithToolSchemaTransform() error = %v", err)
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"parent": map[string]any{
				"anyOf": []any{
					map[string]any{"$ref": "#/$defs/pageParent"},
					map[string]any{"$ref": "#/$defs/databaseParent"},
				},
			},
		},
		"$defs": map[string]any{
			"pageParent": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"page_id": map[string]any{"type": "string"},
				},
			},
			"databaseParent": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"database_id": map[string]any{"type": "string"},
				},
			},
		},
	}
	tools := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:       "mcp_notion_create",
			Parameters: schema,
		},
	}}

	_, err = wrapped.Chat(t.Context(), nil, tools, "test", nil)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	want := providercommon.SanitizeSchemaForGoogle(schema)
	got := capture.lastTools[0].Function.Parameters
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized parameters mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// A pod whose model needs the simple tool schemas still asks the decision
// model: the transform used to hide the gateway's Decisions API, and the
// guards that ask it went off with no error.
func TestToolSchemaTransformKeepsTheDecisionsAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":[{"type":"predicate","name":"stopped_on_promise","probability":0.93}]}`))
	}))
	defer server.Close()
	cfg := &config.ModelConfig{
		ModelName:           "test-simple-schemas",
		Model:               "openai/gpt-4o",
		APIBase:             server.URL + "/v1",
		ToolSchemaTransform: "simple",
	}
	cfg.SetAPIKey("test-key")

	provider, _, err := CreateProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("CreateProviderFromConfig() error = %v", err)
	}
	decider, ok := provider.(Decider)
	if !ok {
		t.Fatalf("provider %T with the simple transform cannot decide", provider)
	}
	answers, err := decider.Decide(t.Context(), "typesafe-ai/jev-test", "the reply",
		[]DecisionQuestion{{Name: "stopped_on_promise", Instructions: "does it stop on a promise?"}})
	if err != nil || answers["stopped_on_promise"] != 0.93 {
		t.Fatalf("Decide() = %v, %v; want the gateway's 0.93", answers, err)
	}

	wrapped, err := wrapProviderWithToolSchemaTransform(&toolCaptureProvider{}, "simple")
	if err != nil {
		t.Fatalf("wrapProviderWithToolSchemaTransform() error = %v", err)
	}
	if _, err := wrapped.(Decider).Decide(t.Context(), "m", "in", nil); !errors.Is(err, errNoDecisions) {
		t.Errorf("Decide() over a provider without decisions = %v, want errNoDecisions", err)
	}
}
