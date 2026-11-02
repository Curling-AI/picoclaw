package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

func TestExecToolRestrictedInvocationCannotBypassRegistry(t *testing.T) {
	for _, scenario := range []string{"policy", "child", "authority_missing", "explicitly_allowed"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := t.TempDir()
			tool, err := NewExecTool(workspace, true)
			if err != nil {
				t.Fatal(err)
			}
			base := WithToolContext(t.Context(), "cli", "fixture")
			ctx := WithExecutionPolicy(base, ExecutionPolicy{})
			switch scenario {
			case "child":
				ctx = context.WithoutCancel(ctx)
			case "authority_missing":
				ctx = protocoltypes.WithRestrictedExecution(base)
			case "explicitly_allowed":
				ctx = WithExecutionPolicy(base, ExecutionPolicy{AllowsTool: func(Tool) bool { return true }})
			}
			if !tool.Execute(ctx, map[string]any{"command": "touch forbidden-marker"}).IsError {
				t.Fatal("restricted direct exec reached its operating-system effect")
			}
			if _, err := os.Stat(filepath.Join(workspace, "forbidden-marker")); !os.IsNotExist(err) {
				t.Fatal("restricted invocation created its marker")
			}
		})
	}
}

func TestExecToolUnrestrictedInvocationKeepsLegacyBehavior(t *testing.T) {
	workspace := t.TempDir()
	tool, err := NewExecTool(workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Execute(
		WithToolContext(t.Context(), "cli", "fixture"),
		map[string]any{"command": "touch allowed-marker"},
	).IsError {
		t.Fatal("ordinary exec lost its existing behavior")
	}
	if _, err := os.Stat(filepath.Join(workspace, "allowed-marker")); err != nil {
		t.Fatal("ordinary exec did not create its disposable marker")
	}
}
