package agent

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// The progress title of a tool call. (seucaranguejo fork — MST-957)

// recordingTitleTool records the arguments it runs with; declaresTitle gives
// it a progress_title argument of its own.
type recordingTitleTool struct {
	name          string
	declaresTitle bool
	got           []map[string]any
}

func (t *recordingTitleTool) Name() string        { return t.name }
func (t *recordingTitleTool) Description() string { return "Records its arguments" }

func (t *recordingTitleTool) Parameters() map[string]any {
	properties := map[string]any{"value": map[string]any{"type": "string"}}
	if t.declaresTitle {
		properties[tools.ProgressTitleParam] = map[string]any{"type": "string", "description": "the tool's own"}
	}
	return map[string]any{"type": "object", "properties": properties}
}

func (t *recordingTitleTool) Execute(_ context.Context, args map[string]any) *tools.ToolResult {
	t.got = append(t.got, maps.Clone(args))
	return tools.SilentResult("recorded")
}

func offersProgressTitle(def providers.ToolDefinition) bool {
	properties, _ := def.Function.Parameters["properties"].(map[string]any)
	_, ok := properties[tools.ProgressTitleParam]
	return ok
}

func toolCallsPayload(t *testing.T, msgs []bus.OutboundMessage) []utils.VisibleToolCall {
	t.Helper()
	for _, msg := range msgs {
		if msg.Context.Raw[metadataKeyMessageKind] != messageKindToolCalls {
			continue
		}
		var calls []utils.VisibleToolCall
		if err := json.Unmarshal([]byte(msg.Context.Raw[metadataKeyToolCalls]), &calls); err != nil {
			t.Fatalf("tool_calls payload: %v", err)
		}
		return calls
	}
	t.Fatalf("no tool_calls among %v", outboundKinds(msgs))
	return nil
}

func TestProgressTitleOfferedToEveryToolWhereTheChannelRendersCards(t *testing.T) {
	provider := &toolEventsProvider{}
	delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
	al, _ := newToolEventsTestLoop(t, delegate, provider)
	mcpSchema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	mcpTool := tools.NewMCPTool(nil, "github", &sdkmcp.Tool{
		Name: "search_issues", Description: "Search issues", InputSchema: mcpSchema,
	})
	al.GetRegistry().GetDefaultAgent().Tools.Register(mcpTool)

	runWebhookTurn(t, al, "slack", "C1")

	offered := map[string]bool{}
	for _, def := range provider.requests[0] {
		offered[def.Function.Name] = offersProgressTitle(def)
	}
	if !offered[mcpTool.Name()] {
		t.Fatalf("MCP tool %s offered without %s (offered: %v)", mcpTool.Name(), tools.ProgressTitleParam, offered)
	}
	for name, ok := range offered {
		if !ok {
			t.Errorf("%s offered without %s", name, tools.ProgressTitleParam)
		}
	}
	if _, changed := mcpSchema["properties"].(map[string]any)[tools.ProgressTitleParam]; changed {
		t.Fatal("the MCP tool's own schema got the argument")
	}
}

func TestProgressTitleNotOfferedElsewhere(t *testing.T) {
	for _, target := range [][2]string{{"slack", "C2"}, {"pico", "C1"}, {"grpc", "C1"}} {
		t.Run(target[0]+"/"+target[1], func(t *testing.T) {
			provider := &toolEventsProvider{
				toolNames: []string{"tool_limit_test_tool"},
				args:      map[string]any{"value": "x"},
			}
			// The structured web channels never get it, even when the manager says yes.
			delegate := toolEventsDelegate{
				toolEvents: map[string]bool{"slack:C1": true, "pico:C1": true, "grpc:C1": true},
			}
			al, msgBus := newToolEventsTestLoop(t, delegate, provider)

			runWebhookTurn(t, al, target[0], target[1])

			for _, request := range provider.requests {
				for _, def := range request {
					if offersProgressTitle(def) {
						t.Fatalf("%s offered with %s", def.Function.Name, tools.ProgressTitleParam)
					}
				}
			}
			if target[0] == "pico" {
				if raw := toolCallsPayload(t, drainOutboundMessages(msgBus)); raw[0].Title != "" {
					t.Fatalf("pico tool call title = %q, want none", raw[0].Title)
				}
			}
		})
	}
}

func TestProgressTitleTakenOutOfTheCall(t *testing.T) {
	title := "Conferindo a previsão do tempo em Lisboa para o fim de semana inteiro, dia a dia"
	provider := &toolEventsProvider{
		toolNames: []string{"record_args"},
		args:      map[string]any{"value": "x", tools.ProgressTitleParam: title},
	}
	delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
	al, msgBus := newToolEventsTestLoop(t, delegate, provider)
	tool := &recordingTitleTool{name: "record_args"}
	al.GetRegistry().GetDefaultAgent().Tools.Register(tool)
	al.cfg.Agents.Defaults.ToolFeedback.MaxArgsLength = 10 // the arguments preview is cut, the title is not

	runWebhookTurn(t, al, "slack", "C1")

	if len(tool.got) != 1 || !reflect.DeepEqual(tool.got[0], map[string]any{"value": "x"}) {
		t.Fatalf("tool ran with %v, want only value", tool.got)
	}
	out := drainOutboundMessages(msgBus)
	calls := toolCallsPayload(t, out)
	wantTitle := string([]rune(title)[:tools.ProgressTitleMaxRunes])
	if calls[0].Title != wantTitle {
		t.Fatalf("tool call title = %q, want %q", calls[0].Title, wantTitle)
	}
	if calls[0].Function == nil || strings.Contains(calls[0].Function.Arguments, tools.ProgressTitleParam) {
		t.Fatalf("arguments preview = %+v, want it without %s", calls[0].Function, tools.ProgressTitleParam)
	}
	for _, msg := range out {
		if msg.Context.Raw[metadataKeyToolIsError] == "true" {
			t.Fatalf("the call failed: %v", msg.Context.Raw)
		}
	}
	for _, msg := range provider.messages[1] {
		for _, tc := range msg.ToolCalls {
			if tc.Function != nil && strings.Contains(tc.Function.Arguments, tools.ProgressTitleParam) {
				t.Fatalf("next request carries the title in the call: %s", tc.Function.Arguments)
			}
		}
	}
}

func TestProgressTitleStaysWithAToolThatDeclaresIt(t *testing.T) {
	provider := &toolEventsProvider{
		toolNames: []string{"titled_tool"},
		args:      map[string]any{tools.ProgressTitleParam: "the tool's value"},
	}
	delegate := toolEventsDelegate{toolEvents: map[string]bool{"slack:C1": true}}
	al, msgBus := newToolEventsTestLoop(t, delegate, provider)
	tool := &recordingTitleTool{name: "titled_tool", declaresTitle: true}
	al.GetRegistry().GetDefaultAgent().Tools.Register(tool)

	runWebhookTurn(t, al, "slack", "C1")

	for _, def := range provider.requests[0] {
		if def.Function.Name != "titled_tool" {
			continue
		}
		own := def.Function.Parameters["properties"].(map[string]any)[tools.ProgressTitleParam]
		if !reflect.DeepEqual(own, map[string]any{"type": "string", "description": "the tool's own"}) {
			t.Fatalf("titled_tool %s schema = %v, want the tool's own", tools.ProgressTitleParam, own)
		}
	}
	if len(tool.got) != 1 || tool.got[0][tools.ProgressTitleParam] != "the tool's value" {
		t.Fatalf("tool ran with %v, want its own %s", tool.got, tools.ProgressTitleParam)
	}
	if calls := toolCallsPayload(t, drainOutboundMessages(msgBus)); calls[0].Title != "" {
		t.Fatalf("tool call title = %q, want none", calls[0].Title)
	}
}

// The calls come from the provider's response, which still shares their maps:
// the title leaves copies; a tool's own progress_title and a call without one
// keep what the model wrote, formatting included.
func TestTakeProgressTitlesCopiesWhatTheResponseShares(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.Register(&recordingTitleTool{name: "record_args"})
	registry.Register(&recordingTitleTool{name: "titled_tool", declaresTitle: true})
	response := []providers.ToolCall{
		{
			ID:        " call_1 ",
			Name:      "record_args",
			Arguments: map[string]any{"value": "x", tools.ProgressTitleParam: "First"},
		},
		{ID: "call_2", Name: "titled_tool", Arguments: map[string]any{tools.ProgressTitleParam: "own"}},
		{
			ID: "call_3", Name: "record_args", Arguments: map[string]any{"value": "y"},
			Function: &providers.FunctionCall{Name: "record_args", Arguments: `{"value": "y"}`},
		},
		{ID: "call_4", Name: "record_args", Arguments: map[string]any{tools.ProgressTitleParam: " "}},
		{
			ID: "call_5", Name: "record_args", Arguments: map[string]any{tools.ProgressTitleParam: "Second"},
			Function: &providers.FunctionCall{Name: "record_args", Arguments: `{"progress_title":"Second"}`},
		},
	}
	calls := make([]providers.ToolCall, 0, len(response))
	for _, tc := range response {
		calls = append(calls, providers.NormalizeToolCall(tc))
	}
	firstFn := calls[0].Function
	fifthFn := calls[4].Function

	calls, titles := takeProgressTitles(registry, calls)

	if !reflect.DeepEqual(titles, map[string]string{"call_1": "First", "call_5": "Second"}) {
		t.Fatalf("titles = %v, want call_1 and call_5", titles)
	}
	if !reflect.DeepEqual(calls[0].Arguments, map[string]any{"value": "x"}) ||
		calls[0].Function.Arguments != `{"value":"x"}` {
		t.Fatalf("call_1 = %v / %s, want only value", calls[0].Arguments, calls[0].Function.Arguments)
	}
	if response[0].Arguments[tools.ProgressTitleParam] != "First" || response[0].Function != nil ||
		!strings.Contains(firstFn.Arguments, tools.ProgressTitleParam) {
		t.Fatal("the response's call changed")
	}
	if fifthFn.Arguments != `{"progress_title":"Second"}` ||
		response[4].Function.Arguments != `{"progress_title":"Second"}` ||
		response[4].Arguments[tools.ProgressTitleParam] != "Second" {
		t.Fatalf("call_5 response changed: %+v / %v", response[4].Function, response[4].Arguments)
	}
	if calls[4].Function == fifthFn || calls[4].Function.Arguments != "{}" {
		t.Fatalf("call_5 = %+v, want a copied FunctionCall without arguments", calls[4].Function)
	}
	if calls[1].Arguments[tools.ProgressTitleParam] != "own" {
		t.Fatalf("call_2 = %v, want the tool's own argument", calls[1].Arguments)
	}
	if calls[2].Function.Arguments != `{"value": "y"}` {
		t.Fatalf("call_3 arguments = %s, want them as the model wrote", calls[2].Function.Arguments)
	}
	if len(calls[3].Arguments) != 0 || calls[3].Function.Arguments != "{}" {
		t.Fatalf("call_4 = %v / %s, want no arguments", calls[3].Arguments, calls[3].Function.Arguments)
	}
}
