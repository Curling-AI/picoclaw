package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDuplicateExecutionPreservesPriorOutcomeAndSchedule(t *testing.T) {
	service := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	period := int64(time.Hour / time.Millisecond)
	job, err := service.AddJob(
		"Identity fixture",
		CronSchedule{Kind: "every", EveryMS: &period},
		"fixture",
		"cli",
		"fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	prior := time.Now().Add(-time.Hour).UnixMilli()
	job.Enabled = false
	job.State.LastRunAtMS, job.State.LastStatus, job.State.LastError = &prior, "error", "original fixture failure"
	if err := service.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	before, _ := service.GetJob(job.ID)
	service.SetOnExecution(
		t.Context(),
		func(context.Context, Execution) (string, error) { return "", ErrDuplicateExecution },
	)
	service.executeJobWithIdentity(job.ID, Manual, uuid.NewString(), time.Now().UTC())
	after, _ := service.GetJob(job.ID)
	if after.Enabled || after.State.LastStatus != before.State.LastStatus ||
		after.State.LastError != before.State.LastError ||
		after.State.LastRunAtMS == nil ||
		*after.State.LastRunAtMS != prior ||
		after.State.NextRunAtMS != nil {
		t.Fatal("duplicate execution replaced the original failure, watermark or paused schedule")
	}
}

func TestManualIdentityReachesHandlerWithoutUnpausingJob(t *testing.T) {
	service := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	period := int64(time.Hour / time.Millisecond)
	job, err := service.AddJob(
		"Identity fixture",
		CronSchedule{Kind: "every", EveryMS: &period},
		"fixture",
		"cli",
		"fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnableJob(job.ID, false); err != nil {
		t.Fatal(err)
	}
	run, at := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	var observed Execution
	service.SetOnExecution(
		t.Context(),
		func(_ context.Context, execution Execution) (string, error) { observed = execution; return "", nil },
	)
	service.executeJobWithIdentity(job.ID, Manual, run, at)
	current, _ := service.GetJob(job.ID)
	if observed.RunID != run || !observed.StartedAt.Equal(at) || !observed.ManualIdentity ||
		observed.Trigger != Manual ||
		current.Enabled {
		t.Fatal("manual identity changed or the paused job was unpaused")
	}
}

func TestManualRunOverridesIdleOptimizationWhileScheduledRunKeepsWatermark(t *testing.T) {
	for _, trigger := range []Trigger{Manual, Scheduled} {
		t.Run(string(trigger), func(t *testing.T) {
			service := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
			job, err := service.AddJob(
				"Memory refresh fixture",
				CronSchedule{Kind: "cron", Expr: "0 4 * * *"},
				"fixture",
				"cli",
				"fixture",
			)
			if err != nil {
				t.Fatal(err)
			}
			prior := time.Now().Add(-time.Hour).UnixMilli()
			job.SkipWhenIdle = true
			job.State.LastRunAtMS, job.State.LastStatus, job.State.LastError = &prior, "error", "previous fixture failure"
			if err := service.UpdateJob(job); err != nil {
				t.Fatal(err)
			}
			calls, probes := 0, 0
			service.SetIdleProbe(func(context.Context, time.Time) (bool, error) {
				probes++
				return false, nil
			})
			service.SetOnExecution(
				t.Context(),
				func(context.Context, Execution) (string, error) { calls++; return "fixture", nil },
			)
			service.executeJobWithIdentity(job.ID, trigger, uuid.NewString(), time.Now().UTC())
			current, _ := service.GetJob(job.ID)
			if trigger == Manual {
				if calls != 1 || probes != 0 || current.State.LastStatus != "ok" || current.State.LastRunAtMS == nil ||
					*current.State.LastRunAtMS == prior {
					t.Fatalf(
						"explicit manual run was skipped: calls=%d probes=%d status=%s",
						calls,
						probes,
						current.State.LastStatus,
					)
				}
			} else if calls != 0 || probes != 1 || current.State.LastRunAtMS == nil || *current.State.LastRunAtMS != prior || current.State.LastStatus != "error" || current.State.LastError != "previous fixture failure" {
				t.Fatalf(
					"scheduled idle skip changed the prior outcome or watermark: calls=%d probes=%d",
					calls,
					probes,
				)
			}
		})
	}
}
