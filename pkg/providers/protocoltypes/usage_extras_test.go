package protocoltypes

import (
	"encoding/json"
	"testing"
)

// Vercel AI Gateway shape: reasoning nested under completion_tokens_details,
// billed cost as flat "cost". Both are provider-specific extras.
func TestUsageInfo_ParsesGatewayReasoningAndCost(t *testing.T) {
	raw := `{"prompt_tokens":23,"completion_tokens":1482,"total_tokens":1505,
		"cost":0.00652792,
		"prompt_tokens_details":{"cached_tokens":22},
		"completion_tokens_details":{"reasoning_tokens":743,"reasoning_tokens_estimated":true}}`

	var u UsageInfo
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.CompletionTokens != 1482 {
		t.Errorf("completion = %d, want 1482", u.CompletionTokens)
	}
	if u.CachedPromptTokens != 22 {
		t.Errorf("cached = %d, want 22", u.CachedPromptTokens)
	}
	if u.ReasoningTokens != 743 {
		t.Errorf("reasoning = %d, want 743", u.ReasoningTokens)
	}
	if u.CostUSD != 0.00652792 {
		t.Errorf("cost = %v, want 0.00652792", u.CostUSD)
	}
}

// Providers that don't send the extras (most of them) must yield zeros —
// consumers fall back to their own estimates.
func TestUsageInfo_FallbackWhenExtrasAbsent(t *testing.T) {
	raw := `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}`

	var u UsageInfo
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.ReasoningTokens != 0 || u.CostUSD != 0 {
		t.Errorf("extras = (%d, %v), want zeros when absent", u.ReasoningTokens, u.CostUSD)
	}
}

// Our own serialization round-trips through the flat field names.
func TestUsageInfo_RoundTrip(t *testing.T) {
	in := UsageInfo{
		PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
		CachedPromptTokens: 5, ReasoningTokens: 8, CostUSD: 0.12,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out UsageInfo
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}
