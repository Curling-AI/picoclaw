package openai_compat

import (
	"fmt"
	"net/http"
	"strings"
)

// frameSeparator joins captured SSE frames in one log field.
const frameSeparator = " | "

// requestIDOption carries a caller-minted trace id. It never reaches the
// request body — buildRequestBody copies named keys only.
const requestIDOption = "request_id"

// requestIDHeader is what hulk reads for tracing and echoes back; other
// OpenAI-compatible gateways use the same name.
const requestIDHeader = "X-Request-Id"

const (
	frameTailSize = 12
	// Per-frame ceiling is only a memory guard. What actually gets logged is
	// decided by the budget below, so a short stream lands verbatim.
	frameTailFrameCeiling = 8192
	// Budget for one log line. An empty stream is a handful of small frames and
	// fits whole — which is the point: a truncated frame cannot be handed to a
	// gateway team as evidence. Only a long stream loses its oldest frames.
	frameTailBudget = 16384
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
	if len(data) > frameTailFrameCeiling {
		data = strings.ToValidUTF8(data[:frameTailFrameCeiling], "") + "…"
	}
	if len(f.frames) == frameTailSize {
		f.frames = append(f.frames[:0], f.frames[1:]...)
	}
	f.frames = append(f.frames, data)
}

func (f *frameTail) String() string {
	if f == nil || len(f.frames) == 0 {
		return ""
	}
	// Newest frames first into the budget: the end of a stream is what explains
	// how it ended.
	kept := 0
	total := 0
	for i := len(f.frames) - 1; i >= 0; i-- {
		next := total + len(f.frames[i]) + len(frameSeparator)
		if kept > 0 && next > frameTailBudget {
			break
		}
		total = next
		kept++
	}
	out := strings.Join(f.frames[len(f.frames)-kept:], frameSeparator)
	if dropped := len(f.frames) - kept; dropped > 0 {
		out = fmt.Sprintf("…(%d earlier frames dropped)%s%s", dropped, frameSeparator, out)
	}
	return out
}
