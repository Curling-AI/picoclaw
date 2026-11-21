package gateway

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
)

func newSeedTestService(t *testing.T) *cron.CronService {
	t.Helper()
	return cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
}

func countMemoryRefresh(cs *cron.CronService) int {
	n := 0
	for _, j := range cs.ListJobs(true) {
		if j.Name == memoryRefreshJobName {
			n++
		}
	}
	return n
}

func TestSeedMemoryRefreshJob_DisabledIsNoop(t *testing.T) {
	cs := newSeedTestService(t)
	cfg := config.DefaultConfig() // MemoryRefreshEnabled defaults false
	seedMemoryRefreshJob(cs, cfg)
	if countMemoryRefresh(cs) != 0 {
		t.Fatal("disabled should not seed a job")
	}
}

func TestSeedMemoryRefreshJob_EnabledSeedsOnceIdempotent(t *testing.T) {
	cs := newSeedTestService(t)
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.MemoryRefreshEnabled = true

	seedMemoryRefreshJob(cs, cfg)
	seedMemoryRefreshJob(cs, cfg) // second boot must not duplicate
	if got := countMemoryRefresh(cs); got != 1 {
		t.Fatalf("expected exactly 1 memory-refresh job, got %d", got)
	}
	// The seeded job carries the daily default schedule.
	for _, j := range cs.ListJobs(true) {
		if j.Name == memoryRefreshJobName && j.Schedule.Expr != "0 4 * * 1,4" {
			t.Fatalf("unexpected schedule: %q", j.Schedule.Expr)
		}
	}
}

// The automations fill the cap: the seed still lands, and Start under a lower
// cap pauses the user's job but never the seeded one.
func TestSeedMemoryRefreshJob_IsExemptFromTheRunCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	capped := func(limit int) *cron.CronService {
		cs := cron.NewCronService(path, nil)
		cs.SetMaxRunsPerDay(limit)
		cs.SetCapExempt(isSeededJob)
		return cs
	}
	cs := capped(48)
	dense := cron.CronSchedule{Kind: "cron", Expr: "*/30 * * * *"}
	if _, err := cs.AddJob("news", dense, "news", "telegram", "chat-1"); err != nil {
		t.Fatalf("48 runs a day fit a cap of 48: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Tools.Cron.MemoryRefreshEnabled = true
	seedMemoryRefreshJob(cs, cfg)
	if got := countMemoryRefresh(cs); got != 1 {
		t.Fatalf("the seed did not land under a full cap: %d memory-refresh jobs", got)
	}
	daily := cron.CronSchedule{Kind: "cron", Expr: "0 9 * * *"}
	if _, err := cs.AddJob(
		memoryRefreshJobName,
		daily,
		"x",
		"telegram",
		"chat-1",
	); !errors.Is(
		err,
		cron.ErrTooManyRuns,
	) {
		t.Errorf("an agent job named memory-refresh must count: got %v", err)
	}

	cs = capped(10)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)
	for _, job := range cs.ListJobs(true) {
		if seeded := job.Name == memoryRefreshJobName; job.Enabled != seeded {
			t.Errorf("job %q enabled=%v after Start under a cap of 10", job.Name, job.Enabled)
		}
	}
}
