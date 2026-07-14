package gateway

import (
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
