package openai_compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseStreamResponse_CarriesUpstreamIDAndFinishReason(t *testing.T) {
	body := sse(
		`{"id":"chatcmpl-abc","choices":[{"delta":{"content":"oi"}}]}`,
		`{"id":"chatcmpl-abc","choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if resp.UpstreamID != "chatcmpl-abc" {
		t.Fatalf("upstream id = %q, want chatcmpl-abc", resp.UpstreamID)
	}
	if resp.FinishReasonMissing {
		t.Fatal("finish reason reported as missing, but the stream sent stop")
	}
}

// A stream that never reports a finish_reason was interrupted. FinishReason
// still defaults to "stop" for existing readers, so the flag is the only way to
// tell the two apart.
func TestParseStreamResponse_MissingFinishReasonIsFlagged(t *testing.T) {
	resp, err := parseStreamResponse(
		context.Background(),
		strings.NewReader(sse(`{"id":"chatcmpl-x","choices":[{"delta":{"content":"oi"}}]}`)),
		nil,
	)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if !resp.FinishReasonMissing {
		t.Fatal("finish reason missing = false, want true")
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want the stop default kept", resp.FinishReason)
	}
}

func TestChat_SendsRequestIDAndReadsItBack(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(requestIDHeader)
		w.Header().Set(requestIDHeader, "request_gateway")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := NewProvider("k", srv.URL, "")
	resp, err := p.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "oi"}},
		nil,
		"m",
		map[string]any{requestIDOption: "pc-deadbeef"},
	)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got != "pc-deadbeef" {
		t.Fatalf("sent %s = %q, want pc-deadbeef", requestIDHeader, got)
	}
	if resp.ProviderRequestID != "request_gateway" {
		t.Fatalf("provider request id = %q, want request_gateway", resp.ProviderRequestID)
	}
	if resp.UpstreamID != "chatcmpl-1" {
		t.Fatalf("upstream id = %q, want chatcmpl-1", resp.UpstreamID)
	}
}

func TestApplyRequestIDHeader_NoOptionLeavesHeaderUnset(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://x/v1/chat/completions", nil)
	applyRequestIDHeader(req, map[string]any{"temperature": 0.7})
	if req.Header.Get(requestIDHeader) != "" {
		t.Fatalf("%s set without the option", requestIDHeader)
	}
}

func TestFrameTail_KeepsLastFramesAndTruncates(t *testing.T) {
	tail := newFrameTail()
	for i := range frameTailSize + 3 {
		tail.add(string(rune('a' + i)))
	}
	if strings.Contains(tail.String(), "a") {
		t.Fatalf("oldest frame kept: %q", tail.String())
	}
	if tail.seen != frameTailSize+3 || tail.omitted != 3 || tail.truncated != 0 {
		t.Fatalf("incorrect event retention counts: %+v", tail)
	}
	tail = newFrameTail()
	tail.add(strings.Repeat("x", frameTailFrameSize*2))
	if len([]rune(tail.String())) > frameTailFrameSize+1 {
		t.Fatalf("frame not truncated: %d runes", len([]rune(tail.String())))
	}
	if tail.seen != 1 || tail.omitted != 0 || tail.truncated != 1 {
		t.Fatalf("truncation was not explicit: %+v", tail)
	}
	tail.add("  \n\t")
	if tail.seen != 1 {
		t.Fatal("empty keepalive was counted as a data event")
	}
}

// The resolved provider must land on every response, not just empty ones:
// without a base rate an empty stream cannot be blamed on a provider.
func TestParseStreamResponse_CarriesResolvedProvider(t *testing.T) {
	body := sse(
		`{"id":"gen_1","choices":[{"delta":{"provider_metadata":{"baseten":{"acceptedPredictionTokens":0},`+
			`"gateway":{"routing":{"originalModelId":"zai/glm-5.3-flash","resolvedProvider":"baseten"}}}}}]}`,
		`{"id":"gen_1","choices":[{"delta":{"content":"oi"},"finish_reason":"stop"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if resp.ResolvedProvider != "baseten" {
		t.Fatalf("resolved provider = %q, want baseten", resp.ResolvedProvider)
	}
}

// The empty stream seen in prod: role chunk, provider_metadata chunk, DONE.
// No content, tool call or reasoning delta anywhere — nothing for the parser to
// drop, and the provider name is the whole point of capturing it.
func TestParseStreamResponse_EmptyStreamStillNamesTheProvider(t *testing.T) {
	body := sse(
		`{"id":"gen_2","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"gen_2","choices":[{"index":0,"delta":{"provider_metadata":{"gateway":{"routing":`+
			`{"resolvedProvider":"baseten"}}}},"finish_reason":"stop"}]}`,
	)

	resp, err := parseStreamResponse(context.Background(), strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("parseStreamResponse: %v", err)
	}
	if resp.Content != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("expected an empty response, got content=%q tools=%d", resp.Content, len(resp.ToolCalls))
	}
	if resp.ResolvedProvider != "baseten" {
		t.Fatalf("resolved provider = %q, want baseten", resp.ResolvedProvider)
	}
}
