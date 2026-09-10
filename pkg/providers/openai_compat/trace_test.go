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

func TestFrameTail_ShortStreamLandsWhole(t *testing.T) {
	// The empty stream seen in prod: four frames, one of them past the old
	// 1024 per-frame cap. All of it has to survive to be usable as evidence.
	tail := newFrameTail()
	metadata := `{"choices":[{"delta":{"provider_metadata":` + strings.Repeat("x", 1400) + `}}]}`
	for _, f := range []string{`{"choices":[{"delta":{"role":"assistant"}}]}`, metadata, "[DONE]", "[DONE]"} {
		tail.add(f)
	}
	got := tail.String()
	if strings.Contains(got, "…") {
		t.Fatalf("short stream was truncated: %q", got[:120])
	}
	if !strings.Contains(got, metadata) {
		t.Fatal("the oversized metadata frame did not survive whole")
	}
}

func TestFrameTail_LongStreamDropsOldestAndSaysSo(t *testing.T) {
	tail := newFrameTail()
	big := strings.Repeat("y", frameTailFrameCeiling)
	for range 4 {
		tail.add(big)
	}
	got := tail.String()
	if len(got) > frameTailBudget+frameTailFrameCeiling {
		t.Fatalf("budget blown: %d chars", len(got))
	}
	if !strings.Contains(got, "earlier frames dropped") {
		t.Fatalf("dropped frames not reported: %q", got[:80])
	}
}

func TestFrameTail_KeepsOnlyTheLastFrames(t *testing.T) {
	tail := newFrameTail()
	for i := range frameTailSize + 3 {
		tail.add(string(rune('a' + i)))
	}
	if strings.Contains(tail.String(), "a |") {
		t.Fatalf("oldest frame kept: %q", tail.String())
	}
}

func TestFrameTail_SingleOversizedFrameStillCeilinged(t *testing.T) {
	tail := newFrameTail()
	tail.add(strings.Repeat("z", frameTailFrameCeiling*2))
	if len([]rune(tail.String())) > frameTailFrameCeiling+1 {
		t.Fatalf("frame not ceilinged: %d runes", len([]rune(tail.String())))
	}
}
