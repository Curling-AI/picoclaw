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

func limitJob(id string, created int64, schedule cron.CronSchedule) cron.CronJob {
	return cron.CronJob{
		ID: id, Name: id, Enabled: true, CreatedAtMS: created, Schedule: schedule,
		Payload: cron.CronPayload{Kind: "agent_turn", Message: id, Channel: "telegram", To: "chat-1"},
	}
}

// limitedCronTool starts the stored jobs under a daily cap of 100 and the given
// minimum, as a pod booting with its plan's limits, and lists them from chat.
func limitedCronTool(t *testing.T, minimum time.Duration, jobs ...cron.CronJob) func(chat string) string {
	t.Helper()
	raw, err := json.Marshal(cron.CronStore{Version: 1, Jobs: jobs})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jobs.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cs := cron.NewCronService(path, nil)
	cs.SetMinInterval(minimum)
	cs.SetMaxRunsPerDay(100)
	if err = cs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Stop)
	tool, err := NewCronTool(cs, nil, bus.NewMessageBus(), t.TempDir(), true, 0, config.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tool.Description(), "'paused by the daily limit' was paused by the platform") {
		t.Errorf("description does not explain the daily-limit pause:\n%s", tool.Description())
	}
	return func(chat string) string {
		ctx := WithToolContext(context.Background(), "telegram", chat)
		return tool.Execute(ctx, map[string]any{"action": "list"}).ForLLM
	}
}

func wantLines(t *testing.T, list string, lines ...string) {
	t.Helper()
	for _, want := range lines {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
}

// The agent reads the budget and the platform's marks from 'list', so it can
// tell the user why a job stopped or slowed and free room before resuming one.
func TestCronToolListShowsTheRunBudgetAndThePlatformMarks(t *testing.T) {
	ms := func(d time.Duration) *int64 { v := d.Milliseconds(); return &v }
	halfHour := cron.CronSchedule{Kind: "cron", Expr: "*/30 * * * *"}
	list := limitedCronTool(t, 30*time.Minute,
		limitJob("a", 1, halfHour),
		limitJob("b", 2, cron.CronSchedule{Kind: "every", EveryMS: ms(15 * time.Minute)}),
		limitJob("c", 3, halfHour),
		limitJob("d", 4, cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}),
	)
	wantLines(
		t,
		list("chat-1"),
		"Runs a day in use: 96 of 100.",
		"- b (id: b, every 1800s, 48 runs a day, slowed from every 900s to the minimum interval)",
		"- c (id: c, */30 * * * *, 48 runs a day, paused by the daily limit)",
		"- d (id: d, */30 * * * *, 48 runs a day, paused by the daily limit, slowed from * * * * * to the minimum interval)",
	)
	// The cap counts every chat's jobs: a chat with none of its own still sees what is left.
	wantLines(t, list("chat-2"), "Runs a day in use: 96 of 100.\nNo scheduled jobs")
}

// Counting stops past the cap, so a job over it alone is not given an exact count.
func TestCronToolListDoesNotCountPastTheCap(t *testing.T) {
	list := limitedCronTool(t, 0, limitJob("x", 1, cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}))
	wantLines(t, list("chat-1"),
		"Runs a day in use: 0 of 100.",
		"- x (id: x, * * * * *, more than 100 runs a day, paused by the daily limit)",
	)
}
