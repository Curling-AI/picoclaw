package openai_compat

import (
	"net/http"
	"strings"
)

// requestIDOption carries a caller-minted trace id. It never reaches the
// request body — buildRequestBody copies named keys only.
const requestIDOption = "request_id"

// requestIDHeader is what hulk reads for tracing and echoes back; other
// OpenAI-compatible gateways use the same name.
const requestIDHeader = "X-Request-Id"

const (
	frameTailSize = 12
	// 320 cut the gateway's provider_metadata frame exactly at the resolved
	// provider — the one field worth having when a stream comes back empty.
	frameTailFrameSize = 1024
)

func requestIDFromOptions(options map[string]any) string {
	id, _ := options[requestIDOption].(string)
	return strings.TrimSpace(id)
}

func applyRequestIDHeader(req *http.Request, options map[string]any) {
	if id := requestIDFromOptions(options); id != "" {
		req.Header.Set(requestIDHeader, id)
	}
}

func usageCompletionTokens(usage *UsageInfo) int {
	if usage == nil {
		return -1
	}
	return usage.CompletionTokens
}

// frameTail keeps the last few raw SSE frames so a stream that yielded nothing
// can still be read after the fact.
type frameTail struct {
	frames []string
}

func newFrameTail() *frameTail {
	return &frameTail{frames: make([]string, 0, frameTailSize)}
}

func (f *frameTail) add(data string) {
	if f == nil {
		return
	}
	if len(data) > frameTailFrameSize {
		data = strings.ToValidUTF8(data[:frameTailFrameSize], "") + "…"
	}
	if len(f.frames) == frameTailSize {
		f.frames = append(f.frames[:0], f.frames[1:]...)
	}
	f.frames = append(f.frames, data)
}

func (f *frameTail) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(f.frames, " | ")
}
