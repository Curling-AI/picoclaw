package cron

import (
	"errors"
	"testing"
	"time"
)

func storedJob(t *testing.T, path, id string) CronJob {
	t.Helper()
	saved, err := readCronStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range saved.Jobs {
		if job.ID == id {
			return job
		}
	}
	t.Fatalf("job %s not in the store", id)
	return CronJob{}
}

// Start records why it paused or slowed a job, so the agent and the app can
// tell the owner; the owner's own pause, resume or new schedule clears it.
func TestStartMarksWhatItPausesAndSlowsAndTheOwnerClearsIt(t *testing.T) {
	everyHalfHour := func(id string, created int64) CronJob {
		return CronJob{
			ID: id, Name: id, Enabled: true, CreatedAtMS: created,
			Schedule: CronSchedule{Kind: "cron", Expr: "*/30 * * * *"},
		}
	}
	fast := CronJob{ID: "b", Name: "b", Enabled: true, CreatedAtMS: 2, Schedule: everySchedule(15 * time.Minute)}
	path := writeStore(t, everyHalfHour("a", 1), fast, everyHalfHour("c", 3))
	cs := NewCronService(path, nil)
	cs.SetMinInterval(30 * time.Minute)
	cs.SetMaxRunsPerDay(100)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)

	if c := storedJob(t, path, "c"); c.Enabled || c.State.PausedBy != PausedByDailyLimit {
		t.Fatalf("c: enabled=%v pausedBy=%q, want paused by the daily limit", c.Enabled, c.State.PausedBy)
	}
	b := storedJob(t, path, "b")
	if b.State.SlowedFrom == nil || *b.State.SlowedFrom.EveryMS != (15*time.Minute).Milliseconds() ||
		*b.Schedule.EveryMS != (30*time.Minute).Milliseconds() {
		t.Fatalf(
			"b: schedule %s slowed from %+v, want every 30 min slowed from every 15 min",
			scheduleText(b.Schedule),
			b.State.SlowedFrom,
		)
	}
	c, _ := cs.GetJob("c")
	if used, needs := cs.RunsPerDayInUse(), cs.RunsPerDay(*c); used != 96 || needs != 48 {
		t.Fatalf("runs a day in use %d, c needs %d; want 96 and 48", used, needs)
	}

	if _, err := cs.EnableJob("c", true); !errors.Is(err, ErrTooManyRuns) {
		t.Fatalf("resuming c without room: %v, want ErrTooManyRuns", err)
	}
	if storedJob(t, path, "c").State.PausedBy != PausedByDailyLimit {
		t.Fatal("a refused resume cleared the mark")
	}
	if _, err := cs.EnableJob("a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.EnableJob("c", true); err != nil {
		t.Fatalf("resuming c after freeing a's runs: %v", err)
	}
	if storedJob(t, path, "c").State.PausedBy != "" {
		t.Fatal("resuming c kept the daily-limit mark")
	}

	renamed, _ := cs.GetJob("b")
	renamed.Name = "b renamed"
	renamed.State.SlowedFrom = nil // a caller's copy never clears the platform's mark
	if err := cs.UpdateJob(renamed); err != nil {
		t.Fatal(err)
	}
	if storedJob(t, path, "b").State.SlowedFrom == nil {
		t.Fatal("a rename cleared the slowed-from mark")
	}
	rescheduled, _ := cs.GetJob("b")
	rescheduled.Schedule = everySchedule(time.Hour)
	if err := cs.UpdateJob(rescheduled); err != nil {
		t.Fatal(err)
	}
	if storedJob(t, path, "b").State.SlowedFrom != nil {
		t.Fatal("a new schedule kept the slowed-from mark")
	}

	ownPause, _ := cs.GetJob("a")
	ownPause.State.PausedBy = PausedByDailyLimit // nor sets it
	if err := cs.UpdateJob(ownPause); err != nil {
		t.Fatal(err)
	}
	if storedJob(t, path, "a").State.PausedBy != "" {
		t.Fatal("a caller's copy marked its own pause as the platform's")
	}
}

// A minimum raised again keeps the schedule the owner wrote.
func TestSlowedFromKeepsTheOwnersSchedule(t *testing.T) {
	cs := NewCronService(writeStore(t, CronJob{
		ID: "x", Name: "x", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: "*/5 * * * *"},
	}), nil)
	cs.mu.Lock()
	cs.minInterval = 10 * time.Minute
	cs.raiseShortSchedules(time.Now().UnixMilli())
	cs.minInterval = 30 * time.Minute
	cs.raiseShortSchedules(time.Now().UnixMilli())
	cs.mu.Unlock()
	job, _ := cs.GetJob("x")
	if job.Schedule.Expr != "*/30 * * * *" || job.State.SlowedFrom == nil ||
		job.State.SlowedFrom.Expr != "*/5 * * * *" {
		t.Fatalf("schedule %q slowed from %+v, want */30 slowed from */5", job.Schedule.Expr, job.State.SlowedFrom)
	}
	*job.State.SlowedFrom = CronSchedule{Kind: "cron", Expr: "changed"}
	if again, _ := cs.GetJob("x"); again.State.SlowedFrom.Expr != "*/5 * * * *" {
		t.Fatal("GetJob shares the slowed-from schedule with the store")
	}
}
