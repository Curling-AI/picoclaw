package cron

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func everySchedule(d time.Duration) CronSchedule {
	ms := d.Milliseconds()
	return CronSchedule{Kind: "every", EveryMS: &ms}
}

func TestDailyRuns(t *testing.T) {
	at := time.Now().UnixMilli()
	for _, tc := range []struct {
		schedule CronSchedule
		want     int
	}{
		{CronSchedule{Kind: "cron", Expr: "*/30 * * * *"}, 48},
		{CronSchedule{Kind: "cron", Expr: "0 8 * * *"}, 1},
		{CronSchedule{Kind: "cron", Expr: "0,10 8 * * *"}, 2},
		{CronSchedule{Kind: "cron", Expr: "*/30 9-17 * * 1-5"}, 18},
		{CronSchedule{Kind: "cron", Expr: "0 8 1 * *"}, 1},
		{CronSchedule{Kind: "cron", Expr: "0 8,20 1,2 * *"}, 2},
		{CronSchedule{Kind: "cron", Expr: "* * * * * *"}, 101}, // counting stops past the limit
		{CronSchedule{Kind: "cron", Expr: "not a cron"}, 0},
		{CronSchedule{Kind: "cron", Expr: "0 */30 * * * * 2027"}, 48}, // first run a year after the reference
		{CronSchedule{Kind: "cron", Expr: "0 */30 * * * * 2025"}, 0},  // its year is over
		{everySchedule(30 * time.Minute), 48},
		{everySchedule(7 * time.Hour), 4},
		{CronSchedule{Kind: "at", AtMS: &at}, 0},
	} {
		if got := dailyRuns(tc.schedule, time.UTC, 100); got != tc.want {
			t.Errorf("dailyRuns(%s %s) = %d, want %d", tc.schedule.Kind, scheduleText(tc.schedule), got, tc.want)
		}
	}
}

func TestRunCapRefusesAddUpdateAndResumePastIt(t *testing.T) {
	cs := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	cs.SetMaxRunsPerDay(100)
	cs.SetCapExempt(func(job *CronJob) bool { return job.Name == MemoryRefreshJobName })
	add := func(name string, schedule CronSchedule) (*CronJob, error) {
		return cs.AddJob(name, schedule, "hello", "cli", "direct")
	}
	for _, name := range []string{"a", "b"} {
		if _, err := add(name, CronSchedule{Kind: "cron", Expr: "*/30 * * * *"}); err != nil {
			t.Fatalf("add %s (48 a day): %v", name, err)
		}
	}
	exact, err := add("c", everySchedule(6*time.Hour))
	if err != nil {
		t.Fatalf("add at exactly the cap (96 + 4): %v", err)
	}
	at := time.Now().Add(time.Hour).UnixMilli()
	if _, err = add("once", CronSchedule{Kind: "at", AtMS: &at}); err != nil {
		t.Errorf("a one-time run does not count: %v", err)
	}
	refresh, err := add(MemoryRefreshJobName, CronSchedule{Kind: "cron", Expr: "*/30 * * * *"})
	if err != nil {
		t.Fatalf("an exempt job does not count: %v", err)
	}

	_, err = add("d", CronSchedule{Kind: "cron", Expr: "0 8 * * *"})
	if !errors.Is(err, ErrTooManyRuns) {
		t.Fatalf("add past the cap: got %v, want ErrTooManyRuns", err)
	}
	if want := "too many runs a day: automations would run 101 times a day, the limit is 100"; err.Error() != want {
		t.Errorf("error text %q, want %q", err, want)
	}

	paused, err := cs.AddPausedJobWithPayload(
		"d",
		CronSchedule{Kind: "cron", Expr: "0 8 * * *"},
		CronPayload{Message: "hi"},
	)
	if err != nil {
		t.Fatalf("a paused job does not count: %v", err)
	}
	if _, err := cs.EnableJob(paused.ID, true); !errors.Is(err, ErrTooManyRuns) {
		t.Errorf("resume past the cap: got %v, want ErrTooManyRuns", err)
	}
	resumed := *paused
	resumed.Enabled = true
	if err := cs.UpdateJob(&resumed); !errors.Is(err, ErrTooManyRuns) {
		t.Errorf("enabling update past the cap: got %v, want ErrTooManyRuns", err)
	}
	if job, _ := cs.GetJob(paused.ID); job.Enabled || job.State.NextRunAtMS != nil {
		t.Errorf("a refused resume left the job enabled=%v next=%v", job.Enabled, job.State.NextRunAtMS)
	}

	busier := *exact
	busier.Schedule = everySchedule(4 * time.Hour)
	if err := cs.UpdateJob(&busier); !errors.Is(err, ErrTooManyRuns) {
		t.Errorf("reschedule past the cap: got %v, want ErrTooManyRuns", err)
	}
	renamed := *exact
	renamed.Name = "renamed"
	if err := cs.UpdateJob(&renamed); err != nil {
		t.Errorf("an update that keeps the schedule is not counted again: %v", err)
	}
	if job, _ := cs.GetJob(exact.ID); scheduleText(job.Schedule) != "every 6h0m0s" {
		t.Errorf("a refused update changed the schedule to %s", scheduleText(job.Schedule))
	}

	unexempt := *refresh
	unexempt.Name = "renamed"
	if err := cs.UpdateJob(&unexempt); !errors.Is(err, ErrTooManyRuns) {
		t.Errorf("a rename that ends an exemption past the cap: got %v, want ErrTooManyRuns", err)
	}
	if job, _ := cs.GetJob(refresh.ID); job.Name != MemoryRefreshJobName {
		t.Errorf("a refused rename changed the job's name to %q", job.Name)
	}

	uncapped := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	for i := range 3 {
		if _, err := uncapped.AddJob(
			"dense",
			CronSchedule{Kind: "cron", Expr: "*/30 * * * *"},
			"hello",
			"cli",
			"direct",
		); err != nil {
			t.Errorf("without a cap job %d is allowed: %v", i, err)
		}
	}
}

// Three */30 jobs (144 runs a day) and an exempt one under a cap of 100: the
// newest counted job is paused and saved, and its missed run is not replayed.
func TestStartPausesTheNewestJobsPastTheCap(t *testing.T) {
	missed := int64Ptr(time.Now().Add(-2 * time.Minute).UnixMilli())
	dense := func(id string, created int64, next *int64) CronJob {
		return CronJob{
			ID: id, Name: id, Enabled: true, CreatedAtMS: created,
			Schedule: CronSchedule{Kind: "cron", Expr: "*/30 * * * *"},
			State:    CronJobState{NextRunAtMS: next},
		}
	}
	stored := []CronJob{
		dense("c", 2, missed), // ties with b on CreatedAtMS: the ID decides
		dense("a", 1, nil),
		dense("b", 2, nil),
		dense(MemoryRefreshJobName, 3, nil),
	}
	for _, tc := range []struct {
		limit  int
		paused string
	}{{100, "c"}, {0, ""}} {
		path := writeStore(t, stored...)
		runs, count := countRuns()
		cs := NewCronService(path, runs)
		cs.SetMaxRunsPerDay(tc.limit)
		cs.SetCapExempt(func(job *CronJob) bool { return job.ID == MemoryRefreshJobName })
		if err := cs.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(cs.Stop)

		saved, err := readCronStore(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range saved.Jobs {
			if want := job.ID != tc.paused; job.Enabled != want {
				t.Errorf("cap %d: job %s saved enabled=%v, want %v", tc.limit, job.ID, job.Enabled, want)
			}
		}
		if tc.paused != "" {
			time.Sleep(300 * time.Millisecond)
			if n := count(); n != 0 {
				t.Errorf("the paused job's missed run was replayed %d time(s)", n)
			}
		} else {
			waitForRuns(t, count, 1) // without a cap the missed run is replayed
		}
	}
}

// Start counts every stored job before the pod serves its wake.
func TestCountingManyDenseJobsAtBootIsQuick(t *testing.T) {
	jobs := make([]CronJob, 20)
	for i := range jobs {
		jobs[i] = CronJob{
			ID: fmt.Sprintf("job-%d", i), Name: "dense", Enabled: true,
			Schedule: CronSchedule{Kind: "cron", Expr: "*/30 * * * *"},
		}
	}
	cs := NewCronService(writeStore(t, jobs...), nil)
	cs.SetMinInterval(30 * time.Minute)
	cs.SetMaxRunsPerDay(1000)
	cs.mu.Lock()
	began := time.Now()
	cs.raiseShortSchedules(time.Now().UnixMilli())
	cs.pauseJobsPastCap()
	took := time.Since(began)
	cs.mu.Unlock()
	t.Logf("checked and counted %d jobs in %s", len(jobs), took)
	if took > 250*time.Millisecond {
		t.Errorf("checking and counting %d allowed */30 jobs took %s", len(jobs), took)
	}
	if paused := len(jobs) - len(cs.ListJobs(false)); paused != 0 {
		t.Errorf("%d jobs paused although 960 runs a day fit a cap of 1000", paused)
	}
}

func TestRunCapCountsAScheduleWhoseFirstYearIsLater(t *testing.T) {
	cs := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	cs.SetMaxRunsPerDay(1)
	later := CronSchedule{Kind: "cron", Expr: "0 */30 * * * * 2027"}
	if _, err := cs.AddJob("later", later, "hello", "cli", "direct"); !errors.Is(err, ErrTooManyRuns) {
		t.Errorf("48 runs a day from 2027 under a cap of 1: got %v, want ErrTooManyRuns", err)
	}
	over := CronSchedule{Kind: "cron", Expr: "0 */30 * * * * 2025"}
	if _, err := cs.AddJob("over", over, "hello", "cli", "direct"); err != nil {
		t.Errorf("a schedule whose year is over never runs: %v", err)
	}
}
