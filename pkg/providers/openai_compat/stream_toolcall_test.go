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
