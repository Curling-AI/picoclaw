package protocoltypes

import (
	"encoding/json"
	"time"
)

type ToolCall struct {
	ID               string         `json:"id"`
	Type             string         `json:"type,omitempty"`
	Function         *FunctionCall  `json:"function,omitempty"`
	Name             string         `json:"-"`
	Arguments        map[string]any `json:"-"`
	ThoughtSignature string         `json:"-"` // Internal use only
	ExtraContent     *ExtraContent  `json:"extra_content,omitempty"`
}

type ExtraContent struct {
	Google                  *GoogleExtra `json:"google,omitempty"`
	ToolFeedbackExplanation string       `json:"tool_feedback_explanation,omitempty"`
}

type GoogleExtra struct {
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

type FunctionCall struct {
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

type LLMResponse struct {
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	FinishReason     string            `json:"finish_reason"`
	Usage            *UsageInfo        `json:"usage,omitempty"`
	Reasoning        string            `json:"reasoning"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details"`
}

type StreamChunk struct {
	Content          string
	ReasoningContent string
	// ToolCalls carries an in-progress snapshot of the tool calls being
	// assembled during streaming (cumulative; Function.Arguments may be a
	// partial JSON fragment). Empty unless the provider streams tool-call
	// deltas. Lets a UI show a tool call evolving instead of a generic spinner.
	ToolCalls []ToolCall
}

type ReasoningDetail struct {
	Format string `json:"format"`
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Text   string `json:"text"`
}

type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedPromptTokens is the subset of PromptTokens served from the
	// provider's prompt cache (billed at a fraction of the input rate). Parsed
	// from the OpenAI-style prompt_tokens_details.cached_tokens and the
	// Anthropic-style cache_read_input_tokens. Zero when the provider reports
	// no cache reuse. Included in PromptTokens, not additive.
	CachedPromptTokens int `json:"cached_prompt_tokens,omitempty"`
	// ReasoningTokens is the thinking share of CompletionTokens (included in
	// it, not additive), parsed from completion_tokens_details.reasoning_tokens.
	// Provider-specific (Vercel AI Gateway sends it, most providers do not) —
	// zero when absent.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// CostUSD is the billed cost of this call in USD as reported by the
	// provider (Vercel AI Gateway's "cost" field). Zero when the provider does
	// not report billing — consumers must fall back to their own price tables.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// UnmarshalJSON reads the flat token counts plus the cached-token count, which
// providers nest differently: OpenAI/openai-compat under
// prompt_tokens_details.cached_tokens, Anthropic-style gateways as a top-level
// cache_read_input_tokens. Either populates CachedPromptTokens.
func (u *UsageInfo) UnmarshalJSON(data []byte) error {
	var raw struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		TotalTokens         int     `json:"total_tokens"`
		CachedPromptTokens  int     `json:"cached_prompt_tokens"`
		CacheReadInputToken int     `json:"cache_read_input_tokens"`
		ReasoningTokens     int     `json:"reasoning_tokens"`
		Cost                float64 `json:"cost"`
		CostUSD             float64 `json:"cost_usd"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	u.PromptTokens = raw.PromptTokens
	u.CompletionTokens = raw.CompletionTokens
	u.TotalTokens = raw.TotalTokens
	switch {
	case raw.CachedPromptTokens > 0:
		u.CachedPromptTokens = raw.CachedPromptTokens
	case raw.PromptTokensDetails != nil && raw.PromptTokensDetails.CachedTokens > 0:
		u.CachedPromptTokens = raw.PromptTokensDetails.CachedTokens
	case raw.CacheReadInputToken > 0:
		u.CachedPromptTokens = raw.CacheReadInputToken
	}
	// Provider-specific extras with sensible fallbacks: reasoning share nests
	// under completion_tokens_details (OpenAI-style / Vercel gateway) or comes
	// flat on re-decode of our own serialization; billed cost is the gateway's
	// "cost" field. Absent → zero, and consumers fall back to estimates.
	switch {
	case raw.ReasoningTokens > 0:
		u.ReasoningTokens = raw.ReasoningTokens
	case raw.CompletionTokensDetails != nil && raw.CompletionTokensDetails.ReasoningTokens > 0:
		u.ReasoningTokens = raw.CompletionTokensDetails.ReasoningTokens
	}
	switch {
	case raw.CostUSD > 0:
		u.CostUSD = raw.CostUSD
	case raw.Cost > 0:
		u.CostUSD = raw.Cost
	}
	return nil
}

// CacheControl marks a content block for LLM-side prefix caching.
// Currently only "ephemeral" is supported (used by Anthropic).
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// ContentBlock represents a structured segment of a system message.
// Adapters that understand SystemParts can use these blocks to set
// per-block cache control (e.g. Anthropic's cache_control: ephemeral).
type ContentBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`

	// Prompt metadata is internal to the agent runtime. It records which
	// structured prompt segment produced this block without changing provider
	// JSON.
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type Attachment struct {
	Type        string `json:"type,omitempty"`
	Ref         string `json:"ref,omitempty"`
	URL         string `json:"url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type Message struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ModelName        string         `json:"model_name,omitempty"`
	CreatedAt        *time.Time     `json:"created_at,omitempty"`
	Media            []string       `json:"media,omitempty"`
	Attachments      []Attachment   `json:"attachments,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	SystemParts      []ContentBlock `json:"system_parts,omitempty"` // structured system blocks for cache-aware adapters
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`

	// Prompt metadata is internal to the agent runtime. It records where a
	// message or system part came from without changing provider/session JSON.
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type ToolDefinition struct {
	Type     string                 `json:"type"`
	Function ToolFunctionDefinition `json:"function"`

	// Prompt metadata is internal to the agent runtime. Tool definitions are
	// model-visible capability prompts even though providers send them outside
	// the system message.
	PromptLayer  string `json:"-"`
	PromptSlot   string `json:"-"`
	PromptSource string `json:"-"`
}

type ToolFunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
