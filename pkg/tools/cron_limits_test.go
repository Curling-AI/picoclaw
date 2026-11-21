package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

// The agent reads the budget and the platform's marks from 'list', so it can
// tell the user why a job stopped or slowed and free room before resuming one.
func TestCronToolListShowsTheRunBudgetAndThePlatformMarks(t *testing.T) {
	ms := func(d time.Duration) *int64 { v := d.Milliseconds(); return &v }
	job := func(id string, created int64, schedule cron.CronSchedule) cron.CronJob {
		return cron.CronJob{
			ID: id, Name: id, Enabled: true, CreatedAtMS: created, Schedule: schedule,
			Payload: cron.CronPayload{Kind: "agent_turn", Message: id, Channel: "telegram", To: "chat-1"},
		}
	}
	halfHour := cron.CronSchedule{Kind: "cron", Expr: "*/30 * * * *"}
	raw, err := json.Marshal(cron.CronStore{Version: 1, Jobs: []cron.CronJob{
		job("a", 1, halfHour),
		job("b", 2, cron.CronSchedule{Kind: "every", EveryMS: ms(15 * time.Minute)}),
		job("c", 3, halfHour),
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cs := cron.NewCronService(path, nil)
	cs.SetMinInterval(30 * time.Minute)
	cs.SetMaxRunsPerDay(100)
	if err = cs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Stop)
	tool, err := NewCronTool(cs, nil, bus.NewMessageBus(), t.TempDir(), true, 0, config.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	list := tool.Execute(
		WithToolContext(context.Background(), "telegram", "chat-1"),
		map[string]any{"action": "list"},
	).ForLLM
	for _, want := range []string{
		"Runs a day in use: 96 of 100.",
		"- b (id: b, every 1800s, 48 runs a day, slowed from every 900s to the minimum interval)",
		"- c (id: c, */30 * * * *, 48 runs a day, paused by the daily limit)",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
	if !strings.Contains(tool.Description(), "'paused by the daily limit' was paused by the platform") {
		t.Errorf("description does not explain the daily-limit pause:\n%s", tool.Description())
	}
}
