package toolshared

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// ExecutionPolicy is carried by one execution and its children. It never
// changes registrations or assistant-wide configuration.
type ExecutionPolicy struct {
	Skills      []string
	AllowsTool  func(Tool) bool
	BeforeTurn  func(context.Context) error
	BeforeTool  func(context.Context, Tool) (context.Context, error)
	AfterTool   func(context.Context, Tool, *ToolResult)
	SkillLoaded func(context.Context, string)
}

type executionPolicyKey struct{}
type executionInvocationKey struct{}
type executionInvocation struct {
	name string
	once sync.Once
}

func WithExecutionPolicy(ctx context.Context, policy ExecutionPolicy) context.Context {
	copy := policy
	copy.Skills = slices.Clone(policy.Skills)
	return protocoltypes.WithRestrictedExecution(context.WithValue(ctx, executionPolicyKey{}, copy))
}

func ExecutionPolicySkills(ctx context.Context) ([]string, bool) {
	policy, ok := ctx.Value(executionPolicyKey{}).(ExecutionPolicy)
	return slices.Clone(policy.Skills), ok
}

func ExecutionPolicyAllowsTool(ctx context.Context, tool Tool) bool {
	policy, ok := ctx.Value(executionPolicyKey{}).(ExecutionPolicy)
	return !ok || (policy.AllowsTool != nil && policy.AllowsTool(tool))
}

func BeforeExecutionTurn(ctx context.Context) error {
	policy, ok := ctx.Value(executionPolicyKey{}).(ExecutionPolicy)
	if ok && policy.BeforeTurn != nil {
		return policy.BeforeTurn(ctx)
	}
	return nil
}

func BeforeExecutionTool(ctx context.Context, tool Tool) (context.Context, error) {
	policy, ok := ctx.Value(executionPolicyKey{}).(ExecutionPolicy)
	if !ok {
		return ctx, nil
	}
	if !ExecutionPolicyAllowsTool(ctx, tool) {
		return ctx, fmt.Errorf("tool %q is not allowed for this execution", tool.Name())
	}
	if invocation, ok := ctx.Value(executionInvocationKey{}).(*executionInvocation); ok && invocation.name == tool.Name() {
		return ctx, nil
	}
	if policy.BeforeTool != nil {
		next, err := policy.BeforeTool(ctx, tool)
		if err != nil {
			return ctx, err
		}
		ctx = next
	}
	return context.WithValue(ctx, executionInvocationKey{}, &executionInvocation{name: tool.Name()}), nil
}

func AfterExecutionTool(ctx context.Context, tool Tool, result *ToolResult) {
	policy, ok := ctx.Value(executionPolicyKey{}).(ExecutionPolicy)
	if !ok || policy.AfterTool == nil {
		return
	}
	invocation, ok := ctx.Value(executionInvocationKey{}).(*executionInvocation)
	if ok && invocation.name == tool.Name() {
		invocation.once.Do(func() { policy.AfterTool(ctx, tool, result) })
	}
}

func ExecutionSkillLoaded(ctx context.Context, name string) {
	policy, ok := ctx.Value(executionPolicyKey{}).(ExecutionPolicy)
	if ok && policy.SkillLoaded != nil {
		policy.SkillLoaded(ctx, name)
	}
}
