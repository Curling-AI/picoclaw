package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestVoiceSearchPermissionIsScopedToTheTurn(t *testing.T) {
	profile := config.EffectiveTurnProfile{DisallowedTools: []string{"exec"}}
	options := processOptions{TurnProfile: profile}
	definitions := []providers.ToolDefinition{
		{Function: providers.ToolFunctionDefinition{Name: "web_search"}},
		{Function: providers.ToolFunctionDefinition{Name: "web_fetch"}},
		{Function: providers.ToolFunctionDefinition{Name: "browser"}},
		{Function: providers.ToolFunctionDefinition{Name: "read_file"}},
	}
	restricted := voiceSearchOptions(WithVoiceSearchPolicy(t.Context(), VoiceSearchHistoryOnly), options)
	available := filterToolsByTurnProfile(definitions, restricted.TurnProfile)
	if len(available) != 1 || available[0].Function.Name != "read_file" {
		t.Fatalf("history-only turn exposes unexpected tools: %v", available)
	}
	for _, tool := range []string{"web_search", "web_fetch", "browser"} {
		if turnProfileToolAllowed(restricted.TurnProfile, tool) {
			t.Fatalf("history-only turn can call %s", tool)
		}
	}

	authorized := voiceSearchOptions(WithVoiceSearchPolicy(t.Context(), VoiceSearchAuthorized), options)
	if !turnProfileToolAllowed(authorized.TurnProfile, "web_search") ||
		turnProfileToolAllowed(authorized.TurnProfile, "exec") {
		t.Fatal("authorization should restore search while preserving existing restrictions")
	}
	if !turnProfileToolAllowed(options.TurnProfile, "web_search") {
		t.Fatal("voice policy modified the caller's profile")
	}
}
