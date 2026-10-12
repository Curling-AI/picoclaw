package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestExecutionHandlerDistinguishesManualAndScheduledRuns(t *testing.T) {
	service := NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
	t.Cleanup(func() {
		for range maxConcurrentJobs {
			service.execSlots <- struct{}{}
		}
		for range maxConcurrentJobs {
			<-service.execSlots
		}
	})
	job, err := service.AddJob(
		"fixture",
		CronSchedule{Kind: "every", EveryMS: int64Ptr(60000), AtMS: nil, Expr: "", TZ: ""},
		"run",
		"cli",
		"direct",
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := make(chan Execution, 3)
	service.SetOnExecution(ctx, func(got context.Context, run Execution) (string, error) {
		if got != ctx {
			t.Error("lost execution context")
		}
		runs <- run
		return "ok", nil
	})
	service.executeJobByID(job.ID)
	if !service.RunNow(job.ID) || !service.RunNow(job.ID) {
		t.Fatal("manual dispatch rejected")
	}
	ids := map[string]bool{}
	for i := range 3 {
		select {
		case run := <-runs:
			if ids[run.RunID] || uuid.Validate(run.RunID) != nil || run.Job.ID != job.ID {
				t.Fatal("invalid or reused run identity", run)
			}
			ids[run.RunID] = true
			if i == 0 && run.Trigger != Scheduled || i > 0 && run.Trigger != Manual {
				t.Fatal("wrong trigger", run.Trigger)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("execution was not dispatched")
		}
	}
}
