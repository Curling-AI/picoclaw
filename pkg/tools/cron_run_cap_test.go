package tools

import (
	"context"
	"strings"
	"testing"
)

func TestCronTool_StatesTheDailyRunCapAndRefusesPastIt(t *testing.T) {
	tool := newTestCronTool(t)
	if strings.Contains(tool.Description(), "times a day") {
		t.Fatalf("without a cap the description names none:\n%s", tool.Description())
	}

	tool.cronService.SetMaxRunsPerDay(50)
	if description := tool.Description(); !strings.Contains(description, "may run at most 50 times a day") {
		t.Fatalf("the description does not state the cap:\n%s", description)
	}

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	result := tool.Execute(ctx, map[string]any{"action": "add", "message": "news", "every_seconds": float64(1800)})
	if result.IsError {
		t.Fatalf("48 runs a day fit a cap of 50: %s", result.ForLLM)
	}
	const refusal = "automations would run 51 times a day, the limit is 50"
	result = tool.Execute(ctx, map[string]any{"action": "add", "message": "mail", "cron_expr": "0 8,12,18 * * *"})
	if !result.IsError || !strings.Contains(result.ForLLM, refusal) {
		t.Errorf("add past the cap: got %q, want %q", result.ForLLM, refusal)
	}

	result = tool.Execute(ctx, map[string]any{"action": "add", "message": "mail", "cron_expr": "0 8,12 * * *"})
	if result.IsError {
		t.Fatalf("50 runs a day fit a cap of 50: %s", result.ForLLM)
	}
	jobs := tool.cronService.ListJobs(true)
	mail := jobs[len(jobs)-1]
	if result = tool.Execute(ctx, map[string]any{"action": "disable", "job_id": mail.ID}); result.IsError {
		t.Fatalf("disable: %s", result.ForLLM)
	}
	if result = tool.Execute(
		ctx,
		map[string]any{"action": "add", "message": "x", "cron_expr": "0 9 * * *"},
	); result.IsError {
		t.Fatalf("49 runs a day fit a cap of 50: %s", result.ForLLM)
	}
	result = tool.Execute(ctx, map[string]any{"action": "enable", "job_id": mail.ID})
	if !result.IsError || !strings.Contains(result.ForLLM, refusal) {
		t.Errorf("enable past the cap: got %q, want %q", result.ForLLM, refusal)
	}
}
