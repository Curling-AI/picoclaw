package agent

import (
	"context"
	"slices"
)

type VoiceSearchPolicy uint8

const (
	VoiceSearchHistoryOnly VoiceSearchPolicy = iota + 1
	VoiceSearchAuthorized
)

type voiceSearchKey struct{}

// WithVoiceSearchPolicy scopes web research to an admitted voice turn.
func WithVoiceSearchPolicy(ctx context.Context, policy VoiceSearchPolicy) context.Context {
	return context.WithValue(ctx, voiceSearchKey{}, policy)
}

func voiceSearchOptions(ctx context.Context, opts processOptions) processOptions {
	policy, ok := ctx.Value(voiceSearchKey{}).(VoiceSearchPolicy)
	if !ok {
		return opts
	}
	opts.voiceSearch = policy
	if policy != VoiceSearchAuthorized {
		opts.TurnProfile.DisallowedTools = slices.Concat(
			opts.TurnProfile.DisallowedTools,
			[]string{"web_search", "web_fetch", "browser"},
		)
	}
	return opts
}

func voiceSearchPrompt(policy VoiceSearchPolicy) PromptPart {
	instructions := "This request came from the user's voice conversation. Resolve references to the previous answer using this conversation's history. Reuse the available results and sources when they answer the question, and distinguish what was found earlier from what is current."
	if policy == VoiceSearchAuthorized {
		instructions += " The user requested or authorized a web lookup for this turn. Limit any lookup to that request."
	} else {
		instructions += " The user has not authorized a new web lookup for this turn. This overrides the general instruction to verify facts with tools. Do not use the web, fetch pages, or use other tools, commands or agents to search on your behalf. If the available results are insufficient and a web lookup is needed, say what is missing and ask the user for permission."
	}
	return PromptPart{
		ID: "instruction.voice_search", Layer: PromptLayerInstruction, Slot: PromptSlotWorkspace,
		Source: PromptSource{ID: PromptSourceVoiceSearch, Name: "voice search policy"},
		Title:  "Voice search policy", Content: instructions, Cache: PromptCacheNone,
	}
}
