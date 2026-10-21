package tools

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func runExec(t *testing.T, ctx context.Context, command string) *ToolResult {
	t.Helper()
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("NewExecTool: %v", err)
	}
	result := tool.Execute(ctx, map[string]any{"action": "run", "command": command})
	if result.IsError {
		t.Fatalf("exec failed: %s", result.ForLLM)
	}
	return result
}

// The host's variables reach the command, chosen per call from the turn's
// session, and the inherited environment stays.
func TestExecEnvProvider_ReachesTheCommandPerCall(t *testing.T) {
	SetExecEnvProvider(func(ctx context.Context) []string {
		return []string{"MST348_ORIGIN=" + ToolSessionKey(ctx)}
	})
	t.Cleanup(func() { SetExecEnvProvider(nil) })
	t.Setenv("MST348_INHERITED", "kept")

	ctx := WithToolSessionContext(context.Background(), "main", "agent:cron-job-run", nil)
	result := runExec(t, ctx, `printf '%s|%s' "$MST348_ORIGIN" "$MST348_INHERITED"`)

	if !strings.Contains(result.ForUser, "agent:cron-job-run|kept") {
		t.Fatalf("output = %q, want the turn's session and the inherited variable", result.ForUser)
	}
}

func TestExecEnvProvider_UnsetKeepsTheInheritedEnvironment(t *testing.T) {
	SetExecEnvProvider(nil)
	t.Setenv("MST348_INHERITED", "kept")

	result := runExec(t, context.Background(), `printf '%s' "$MST348_INHERITED"`)

	if !strings.Contains(result.ForUser, "kept") {
		t.Fatalf("output = %q, want the inherited variable", result.ForUser)
	}
}

// A variable the host sets wins over the inherited one with the same name.
func TestApplyExecEnv_HostValueWins(t *testing.T) {
	SetExecEnvProvider(func(context.Context) []string { return []string{"MST348_VAR=host"} })
	t.Cleanup(func() { SetExecEnvProvider(nil) })
	t.Setenv("MST348_VAR", "inherited")

	cmd := exec.Command("sh", "-c", `printf '%s' "$MST348_VAR"`)
	applyExecEnv(context.Background(), cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != "host" {
		t.Fatalf("MST348_VAR = %q, want the host's value", out)
	}
	if !slices.Contains(cmd.Env, "MST348_VAR=inherited") {
		t.Fatal("the inherited variable should still be listed before the host's")
	}
}

// The turn's id reaches the host with the session, so a send the script makes
// can be logged with the turn that started it.
func TestExecEnvProvider_SeesTheTurn(t *testing.T) {
	var turn string
	SetExecEnvProvider(func(ctx context.Context) []string {
		turn = ToolTurnID(ctx)
		return nil
	})
	t.Cleanup(func() { SetExecEnvProvider(nil) })

	ctx := WithToolTurnID(WithToolSessionContext(context.Background(), "main", "sk_v1_chat", nil), "turn-7")
	runExec(t, ctx, "true")

	if turn != "turn-7" {
		t.Fatalf("provider saw turn %q, want turn-7", turn)
	}
}
