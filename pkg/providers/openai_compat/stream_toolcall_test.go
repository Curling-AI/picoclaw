package openai_compat

import (
	"context"
	"strings"
	"testing"
)

// sse builds a minimal SSE stream body from data payloads.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// A lone tool call streamed at delta index 1 (no index 0) must still be
// assembled. The old positional loop (for i := 0; i < len(activeTools); i++)
// only looked up keys 0..n-1 and silently dropped it, turning a tool-use turn
// into a bare narration — the "announce and stop" seed of the dead-
// conversation bug.
func TestParseStreamResponse_ToolCallAtNonZeroIndex(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"content":"Vou escrever o arquivo."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"write_file","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"a.txt\"}"}}]}}],"finish_reason":null}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 (index-1 call dropped)", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "write_file" {
		t.Fatalf("tool name = %q, want write_file", tc.Name)
	}
	if got := tc.Arguments["path"]; got != "a.txt" {
		t.Fatalf("arguments path = %v, want a.txt", got)
	}
}

// Index gaps ({0, 2}) must keep every accumulated call, in index order.
func TestParseStreamResponse_ToolCallIndexGap(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":2,"id":"call_c","function":{"name":"exec","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2 (gap dropped a call)", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[1].Name != "exec" {
		t.Fatalf("tool order = %s,%s want read_file,exec", resp.ToolCalls[0].Name, resp.ToolCalls[1].Name)
	}
}

// The in-progress snapshot surfaced to onChunk must also include non-zero
// index accumulators (the UI card for the call would otherwise never appear).
func TestParseStreamResponse_SnapshotIncludesNonZeroIndex(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"write_file","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	var sawWriteFile bool
	_, err := parseStreamResponse(context.Background(), strings.NewReader(body), func(chunk StreamChunk) {
		for _, tc := range chunk.ToolCalls {
			if tc.Function != nil && tc.Function.Name == "write_file" {
				sawWriteFile = true
			}
		}
	})
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if !sawWriteFile {
		t.Fatal("streaming snapshot never surfaced the index-1 tool call")
	}
}

// A reasoning model can emit the whole call inside its THINKING and send no
// content at all. Without salvaging that channel the turn comes back empty,
// the agent loop promotes the thinking to the reply, and the user reads raw
// markup instead of the tool running (prod, 2026-08-19).
func TestParseStreamResponse_ToolCallInReasoningChannel(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"reasoning_content":"<function=read_file>\n<parameter=path>a.txt</parameter>\n</function>"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1 salvaged from reasoning", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool name = %q, want read_file", resp.ToolCalls[0].Name)
	}
	if got := resp.ToolCalls[0].Arguments["path"]; got != "a.txt" {
		t.Fatalf("arguments path = %v, want a.txt", got)
	}
}

// The same salvage must NOT fire when the model actually answered: musing
// "I could call read_file" in the scratchpad is not a call, and promoting it
// would run tools the model never asked for.
func TestParseStreamResponse_ReasoningNotSalvagedWhenContentExists(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"reasoning_content":"<function=read_file>\n<parameter=path>a.txt</parameter>\n</function>"}}]}`,
		`{"choices":[{"delta":{"content":"Já li o arquivo antes, não preciso ler de novo."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("tool calls = %d, want none — the model answered", len(resp.ToolCalls))
	}
}

// Markup that becomes a tool call must never reach the user's screen. The
// salvage runs at end-of-stream, so without withholding it the raw
// `<tool_call><function=…>` streams live and no later finalize retracts it
// (measured in the kind e2e: tool ran, answer arrived, markup still on screen).
func TestParseStreamResponse_ToolCallMarkupIsNotStreamed(t *testing.T) {
	body := sse(
		`{"choices":[{"delta":{"content":"Vou ler o arquivo.\n"}}]}`,
		`{"choices":[{"delta":{"content":"<tool_call>\n<function=read_file>\n"}}]}`,
		`{"choices":[{"delta":{"content":"<parameter=path>a.txt</parameter>\n</function>\n</tool_call>"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)

	var published []string
	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), func(c StreamChunk) {
		if c.Content != "" {
			published = append(published, c.Content)
		}
	})
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	for _, p := range published {
		if strings.Contains(p, "<tool_call>") || strings.Contains(p, "<function=") {
			t.Fatalf("markup foi publicado ao vivo: %q", p)
		}
	}
	// A narração que veio ANTES do markup continua streamando normalmente.
	if len(published) == 0 || !strings.Contains(published[0], "Vou ler o arquivo") {
		t.Fatalf("a narração legítima parou de streamar: %#v", published)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %+v, want one read_file", resp.ToolCalls)
	}
}
