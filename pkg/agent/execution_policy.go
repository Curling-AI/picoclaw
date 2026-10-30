package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func executionPolicyOptions(ctx context.Context, agent *AgentInstance, opts processOptions) (processOptions, error) {
	skills, restricted := tools.ExecutionPolicySkills(ctx)
	if !restricted {
		return opts, nil
	}
	if err := tools.BeforeExecutionTurn(ctx); err != nil {
		return opts, err
	}
	previous := opts.TurnProfile
	allowed := []string{}
	for _, name := range agent.Tools.List() {
		tool, ok := agent.Tools.GetRegistered(name)
		if ok && turnProfileToolAllowed(previous, name) && tools.ExecutionPolicyAllowsTool(ctx, tool) {
			allowed = append(allowed, name)
		}
	}
	if turnProfileSkillsOff(previous) {
		skills = nil
	} else if turnProfileCustomSkills(previous) {
		skills = filterNamesByTurnProfile(skills, previous.AllowedSkills)
	}
	opts.TurnProfile.Enabled = true
	opts.TurnProfile.ToolsMode = config.TurnProfileModeCustom
	opts.TurnProfile.AllowedTools = allowed
	opts.TurnProfile.SkillsMode = config.TurnProfileModeCustom
	opts.TurnProfile.AllowedSkills = skills
	if len(skills) == 0 {
		opts.TurnProfile.SkillsMode = config.TurnProfileModeOff
	}
	opts.ForcedSkills = skills
	opts.ObserveSkill = func(name string) { tools.ExecutionSkillLoaded(ctx, name) }
	return opts, nil
}
