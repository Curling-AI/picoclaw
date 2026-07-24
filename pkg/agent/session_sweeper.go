package agent

import (
	"os"
	"strconv"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/memory"
)

const (
	// cronRetentionEnv overrides how many days of cron runs are kept. Set it to
	// 0 to keep every run forever (the pre-sweeper behavior).
	cronRetentionEnv = "PICOCLAW_SESSIONS_CRON_RETENTION_DAYS"

	// defaultCronRetentionDays keeps a couple of weeks of runs — long enough to
	// debug a misbehaving job from the Execuções panel, short enough that a
	// 5-minute job (~288 runs/day) cannot bury the session directory.
	defaultCronRetentionDays = 14

	// cronSweepInterval is how often the sweeper runs after the boot pass.
	// Sweeping is O(cron files) and entirely off any request path, but on EFS
	// it is still a directory scan, so once every few hours is plenty for a
	// backlog that grows by a few hundred files a day.
	cronSweepInterval = 6 * time.Hour
)

// startCronRunSweeper deletes cron-run sessions older than the retention window
// in the background, forever.
//
// Each cron execution persists a session of its own and nothing ever removed
// them, so the session directory grew without bound: one prod pod reached 7.7k
// files. That matters because listing sessions reads every .meta.json off EFS —
// the listing outgrew the gateway timeout and the web sidebar came back empty
// even though every conversation was intact. Conversations are never pruned;
// only runs, which the UI presents under Automações (Execuções) and which this
// turns into a retention window instead of an unbounded log.
//
// The first pass runs immediately so a pod that boots with a backlog clears it
// without waiting for the first tick.
func startCronRunSweeper(store *memory.JSONLStore) {
	retention := cronRunRetention()
	if retention <= 0 {
		logger.InfoCF("agent", "Cron run sweeper disabled", map[string]any{"env": cronRetentionEnv})
		return
	}
	go func() {
		for {
			sweepCronRuns(store, retention)
			time.Sleep(cronSweepInterval)
		}
	}()
}

// sweepCronRuns runs one pruning pass and logs its outcome.
func sweepCronRuns(store *memory.JSONLStore, retention time.Duration) {
	removed, err := store.PruneCronRuns(retention, time.Now())
	if err != nil {
		logger.WarnCF("agent", "Cron run sweep failed", map[string]any{"error": err.Error()})
		return
	}
	if removed > 0 {
		logger.InfoCF("agent", "Cron runs pruned", map[string]any{
			"sessions_removed": removed,
			"retention_days":   int(retention.Hours() / 24),
		})
	}
}

// cronRunRetention resolves the retention window, falling back to the default
// when the override is absent or unparseable.
func cronRunRetention() time.Duration {
	raw := os.Getenv(cronRetentionEnv)
	if raw == "" {
		return defaultCronRetentionDays * 24 * time.Hour
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		logger.WarnCF("agent", "Invalid cron retention; using default", map[string]any{
			"env": cronRetentionEnv, "value": raw, "default_days": defaultCronRetentionDays,
		})
		return defaultCronRetentionDays * 24 * time.Hour
	}
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}
