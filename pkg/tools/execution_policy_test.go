package tools

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

type policyCountTool struct {
	*mockRegistryTool
	calls atomic.Int64
}

func (t *policyCountTool) Execute(context.Context, map[string]any) *ToolResult {
	t.calls.Add(1)
	return SilentResult("executed")
}

func TestExecutionPoliciesIsolateConcurrentCallsAndSurviveDetachedChildren(t *testing.T) {
	registry := NewToolRegistry()
	first := &policyCountTool{mockRegistryTool: newMockTool("first", "first")}
	second := &policyCountTool{mockRegistryTool: newMockTool("second", "second")}
	unknown := &policyCountTool{mockRegistryTool: newMockTool("unknown_plugin", "unknown")}
	registry.RegisterHidden(first)
	registry.RegisterHidden(second)
	registry.Register(unknown)
	contexts := []context.Context{}
	for _, name := range []string{"first", "second"} {
		allowed := name
		policy := ExecutionPolicy{Skills: []string{"selected"}, AllowsTool: func(tool Tool) bool { return tool.Name() == allowed }}
		ctx := WithExecutionPolicy(context.Background(), policy)
		policy.Skills[0] = "mutated_after_capture"
		captured, restricted := ExecutionPolicySkills(ctx)
		if !restricted || captured[0] != "selected" {
			t.Fatal("execution snapshot mutated", captured)
		}
		contexts = append(contexts, context.WithoutCancel(ctx))
	}
	var group sync.WaitGroup
	for i, ctx := range contexts {
		group.Go(func() {
			for range 10 {
				for j, name := range []string{"first", "second"} {
					result := registry.Execute(ctx, name, map[string]any{})
					if result.IsError == (i == j) {
						t.Errorf("execution %d permission for %s was not isolated", i, name)
					}
				}
				if !registry.Execute(ctx, "unknown_plugin", map[string]any{}).IsError {
					t.Error("unknown plugin bypassed scope")
				}
			}
		})
	}
	group.Wait()
	if first.calls.Load() != 10 || second.calls.Load() != 10 || unknown.calls.Load() != 0 {
		t.Fatal("denied calls reached implementations")
	}
	if len(registry.VisibleHiddenNames()) != 0 {
		t.Fatal("execution policy mutated shared visibility")
	}
	if registry.Execute(context.Background(), "unknown_plugin", map[string]any{}).IsError {
		t.Fatal("restricted call leaked into interactive chat")
	}
}
