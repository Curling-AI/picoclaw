package protocoltypes

import (
	"encoding/json"
	"testing"
)

func TestUsageInfoParsesCachedTokens(t *testing.T) {
	cases := map[string]struct {
		body       string
		wantPrompt int
		wantCached int
	}{
		"openai prompt_tokens_details": {
			`{"prompt_tokens":56000,"completion_tokens":300,"prompt_tokens_details":{"cached_tokens":48000}}`,
			56000, 48000,
		},
		"anthropic cache_read_input_tokens": {
			`{"prompt_tokens":56000,"completion_tokens":300,"cache_read_input_tokens":40000}`,
			56000, 40000,
		},
		"flat cached_prompt_tokens": {
			`{"prompt_tokens":10,"completion_tokens":2,"cached_prompt_tokens":7}`,
			10, 7,
		},
		"no cache": {
			`{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}`,
			100, 0,
		},
		"details present but zero": {
			`{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}`,
			100, 0,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var u UsageInfo
			if err := json.Unmarshal([]byte(tc.body), &u); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if u.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", u.PromptTokens, tc.wantPrompt)
			}
			if u.CachedPromptTokens != tc.wantCached {
				t.Errorf("CachedPromptTokens = %d, want %d", u.CachedPromptTokens, tc.wantCached)
			}
		})
	}
}
