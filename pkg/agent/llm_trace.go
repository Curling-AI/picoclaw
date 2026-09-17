package agent

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// llmRequestIDOption is read by the provider and sent as X-Request-Id.
const llmRequestIDOption = "request_id"

// newLLMRequestID mints one trace id per upstream call. It doubles as the
// gateway's idempotency key, so every attempt — including a retry of the same
// prompt — must get a fresh one.
func newLLMRequestID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return "pc-" + hex.EncodeToString(buf[:])
}

func optsWithFreshRequestID(opts map[string]any) (map[string]any, string) {
	id := newLLMRequestID()
	next := maps.Clone(opts)
	if next == nil {
		next = map[string]any{}
	}
	next[llmRequestIDOption] = id
	return next, id
}

func requestIDFromOptions(opts map[string]any) string {
	id, _ := opts[llmRequestIDOption].(string)
	return strings.TrimSpace(id)
}

func llmCallRecord(requestID string, resp *providers.LLMResponse) *providers.LLMCall {
	if resp == nil {
		if requestID == "" {
			return nil
		}
		return &providers.LLMCall{RequestID: requestID}
	}
	call := &providers.LLMCall{
		RequestID:           requestID,
		ProviderRequestID:   resp.ProviderRequestID,
		UpstreamID:          resp.UpstreamID,
		ResolvedProvider:    resp.ResolvedProvider,
		FinishReason:        resp.FinishReason,
		FinishReasonMissing: resp.FinishReasonMissing,
	}
	if *call == (providers.LLMCall{}) {
		return nil
	}
	return call
}

// responseCompletionTokens returns -1 when the provider reported no usage, so
// "billed nothing" stays distinguishable from "did not say".
func responseCompletionTokens(resp *providers.LLMResponse) int {
	if resp == nil || resp.Usage == nil {
		return -1
	}
	return resp.Usage.CompletionTokens
}
