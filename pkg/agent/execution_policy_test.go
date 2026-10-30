package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestExecutionPolicyFiltersPromptAndSkillsWithoutChangingAssistant(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	writeTurnProfileSkill(t, cfg.Agents.Defaults.Workspace, "selected", "---\nname: selected\ndescription: Selected instructions\n---\nSELECTED_BODY")
	writeTurnProfileSkill(t, cfg.Agents.Defaults.Workspace, "excluded", "---\nname: excluded\ndescription: Excluded instructions\n---\nEXCLUDED_BODY")
	provider := &turnProfileCaptureProvider{}
	loop := newTurnProfileAgentLoop(t, cfg, provider)
	agent := loop.GetRegistry().GetDefaultAgent()
	observed := []string{}
	policy := tools.ExecutionPolicy{Skills: []string{"selected"}, AllowsTool: func(tools.Tool) bool { return false }, SkillLoaded: func(_ context.Context, name string) { observed = append(observed, name) }}
	ctx := tools.WithExecutionPolicy(t.Context(), policy)
	if _, err := loop.runAgentLoop(ctx, agent, processOptions{SessionKey: "restricted", UserMessage: "do the task", ForcedSkills: []string{"excluded"}}); err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, message := range provider.messages {
		text += message.Content
	}
	if !strings.Contains(text, "SELECTED_BODY") || strings.Contains(text, "EXCLUDED_BODY") || len(provider.tools) != 0 || len(observed) == 0 {
		t.Fatal("prompt or observation escaped execution scope", observed)
	}
	if _, err := loop.runAgentLoop(t.Context(), agent, processOptions{SessionKey: "interactive", UserMessage: "do another task", ForcedSkills: []string{"excluded"}}); err != nil {
		t.Fatal(err)
	}
	text = ""
	for _, message := range provider.messages {
		text += message.Content
	}
	if !strings.Contains(text, "EXCLUDED_BODY") {
		t.Fatal("execution restriction changed assistant-wide skills")
	}
	childOptions, err := executionPolicyOptions(context.WithoutCancel(ctx), agent, processOptions{})
	if err != nil || len(childOptions.TurnProfile.AllowedSkills) != 1 || childOptions.TurnProfile.AllowedSkills[0] != "selected" {
		t.Fatal("detached child lost policy", childOptions.TurnProfile, err)
	}
}
