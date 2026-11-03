package cron

import (
	"path/filepath"
	"testing"
)

func TestClaimedScheduledJobDoesNotRunAfterItIsDisabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		trigger Trigger
		want    int
	}{
		{name: "scheduled job claimed before disable", trigger: Scheduled, want: 0},
		{name: "explicit manual run remains available", trigger: Manual, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			service := NewCronService(
				filepath.Join(t.TempDir(), "jobs.json"),
				func(*CronJob) (string, error) { calls++; return "fixture", nil },
			)
			job, err := service.AddJob(
				"Synthetic fixture",
				CronSchedule{Kind: "cron", Expr: "0 0 1 1 *"},
				"fixture",
				"",
				"",
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.EnableJob(job.ID, false); err != nil {
				t.Fatal(err)
			}
			// dispatch already owns the id; waiting for an execution slot can
			// outlive the owner's later disable action. Re-read it at execution.
			service.executeJob(job.ID, test.trigger)
			if calls != test.want {
				t.Fatalf("provider/effect callback calls=%d; want=%d", calls, test.want)
			}
			persisted, found := service.GetJob(job.ID)
			if !found || persisted.Enabled || persisted.State.NextRunAtMS != nil {
				t.Fatal("execution changed the owner's disabled configuration")
			}
			if test.trigger == Scheduled && persisted.State.LastRunAtMS != nil {
				t.Fatal("a denied scheduled occurrence was recorded as a run")
			}
		})
	}
}
