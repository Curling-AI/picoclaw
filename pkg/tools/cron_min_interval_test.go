package tools

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"
)

func TestCronTool_StatesTheMinimumIntervalAndRefusesShorterSchedules(t *testing.T) {
	tool := newTestCronTool(t)
	if strings.Contains(tool.Description(), "apart") {
		t.Fatalf("without a minimum the description names none:\n%s", tool.Description())
	}

	tool.cronService.SetMinInterval(30 * time.Minute)
	if description := tool.Description(); !strings.Contains(description, "at least 30 minutes apart") ||
		!strings.Contains(description, "every_seconds of 1800 or more") {
		t.Fatalf("the description does not state the minimum:\n%s", description)
	}

	ctx := WithToolContext(context.Background(), "telegram", "chat-1")
	add := func(schedule map[string]any) *ToolResult {
		args := map[string]any{"action": "add", "message": "check the news"}
		maps.Copy(args, schedule)
		return tool.Execute(ctx, args)
	}
	for _, schedule := range []map[string]any{
		{"every_seconds": float64(300)},
		{"cron_expr": "*/5 * * * *"},
	} {
		result := add(schedule)
		if !result.IsError || !strings.Contains(result.ForLLM, "the minimum is 30 min") {
			t.Errorf("%v: got %q, want a refusal that names the minimum", schedule, result.ForLLM)
		}
	}

	if result := add(map[string]any{"every_seconds": float64(1800)}); result.IsError {
		t.Fatalf("every 30 min is the minimum itself: %s", result.ForLLM)
	}
	job := tool.cronService.ListJobs(true)[0]
	result := tool.Execute(ctx, map[string]any{"action": "update", "job_id": job.ID, "cron_expr": "*/10 * * * *"})
	if !result.IsError || !strings.Contains(result.ForLLM, "the minimum is 30 min") {
		t.Errorf("update to */10: got %q, want a refusal that names the minimum", result.ForLLM)
	}
}
