package tools

import toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"

type ExecutionPolicy = toolshared.ExecutionPolicy

var (
	WithExecutionPolicy       = toolshared.WithExecutionPolicy
	ExecutionPolicySkills     = toolshared.ExecutionPolicySkills
	ExecutionPolicyAllowsTool = toolshared.ExecutionPolicyAllowsTool
	BeforeExecutionTurn       = toolshared.BeforeExecutionTurn
	ExecutionSkillLoaded      = toolshared.ExecutionSkillLoaded
)
