package openai_compat

import (
	"context"
	"strings"
	"testing"
)

// rawSSE builds an SSE body exactly as given: unlike sse, it adds no [DONE].
func rawSSE(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

const narrationDelta = `{"choices":[{"delta":{"content":"Skill carregada. Agora escrevo o script de geração:"}}]}`

func TestParseStreamResponse_StreamInterrupted(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			"finished: finish_reason and [DONE]",
			sse(narrationDelta, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
			false,
		},
		{
			"finished: finish_reason, connection closed without [DONE]",
			rawSSE(narrationDelta, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`),
			false,
		},
		{
			"finished: [DONE] from a server that never reports finish_reason",
			sse(narrationDelta),
			false,
		},
		// Prod shape (HULK_STREAM_ERROR_EVENTS_ENABLED off): the attempt failed
		// after the first delta and the gateway just closed the stream.
		{
			"cut: closed after the first delta",
			rawSSE(narrationDelta),
			true,
		},
		// Dev shape: one terminal error frame, then close, no [DONE].
		{
			"cut: terminal error frame",
			rawSSE(
				narrationDelta,
				`{"error":{"message":"The response stream could not be completed","type":"server_error","code":"provider_stream_error"}}`,
			),
			true,
		},
		{
			"cut: error frame even when [DONE] would follow",
			sse(
				narrationDelta,
				`{"error":{"message":"The response stream could not be completed","type":"server_error","code":"stream_interrupted"}}`,
			),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := parseStreamResponse(context.Background(), strings.NewReader(tt.body), nil, nil)
			if err != nil {
				t.Fatalf("parseStreamResponse: %v", err)
			}
			if resp.StreamInterrupted != tt.want {
				t.Errorf("StreamInterrupted = %v, want %v", resp.StreamInterrupted, tt.want)
			}
			// The fragment stays available to the caller, and the error frame
			// never becomes part of the answer.
			if !strings.HasPrefix(resp.Content, "Skill carregada.") {
				t.Errorf("content = %q, want the streamed text", resp.Content)
			}
			if strings.Contains(resp.Content, "could not be completed") {
				t.Errorf("the error frame leaked into the content: %q", resp.Content)
			}
		})
	}
}
