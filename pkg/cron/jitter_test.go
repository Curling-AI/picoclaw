package cron

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestJitterSpreadsRoundMinutesAndSparesTheRest(t *testing.T) {
	const seed = "assistant-a"
	for _, expr := range []string{"0 8 * * *", "30 * * * *", "@daily", "@hourly", "@weekly", "0 0 1 1 *"} {
		if Jitter(seed, "job-1", expr) != Jitter(seed, "job-1", expr) {
			t.Fatalf("%q: jitter is not stable", expr)
		}
	}
	for _, expr := range []string{
		"*/5 * * * *", "0,30 * * * *", "0-10 * * * *", "@5minutes", "@30minutes",
		"* * * * * *", "", "not a cron",
	} {
		if got := Jitter(seed, "job-1", expr); got != 0 {
			t.Errorf("%q runs several times an hour or is unreadable; got jitter %s", expr, got)
		}
	}
	if got := Jitter("", "job-1", "0 8 * * *"); got != 0 {
		t.Errorf("an empty seed must turn jitter off; got %s", got)
	}

	seen := map[time.Duration]bool{}
	lowest, highest := MaxJitter, time.Duration(0)
	for i := range 2000 {
		j := Jitter(seed, fmt.Sprintf("job-%d", i), "0 8 * * *")
		if j < 0 || j >= MaxJitter || j%time.Second != 0 {
			t.Fatalf("jitter %s outside [0, %s) or not whole seconds", j, MaxJitter)
		}
		seen[j] = true
		lowest, highest = min(lowest, j), max(highest, j)
	}
	if len(seen) < 400 || lowest > 30*time.Second || highest < MaxJitter-30*time.Second {
		t.Errorf("2000 jobs got %d distinct delays in [%s, %s]; want them spread over the whole window",
			len(seen), lowest, highest)
	}
	if Jitter("assistant-a", "memory-refresh", "0 3 * * *") == Jitter("assistant-b", "memory-refresh", "0 3 * * *") {
		t.Error("the same job ID in two assistants should get different delays")
	}
}

func TestNextCronTickKeepsARunThatIsInsideItsDelay(t *testing.T) {
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, time.UTC) }
	const delay = 7 * time.Minute
	for _, tc := range []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{"before the minute", day(20, 7, 59), day(20, 8, 7)},
		{"past the minute, inside the delay", day(20, 8, 3), day(20, 8, 7)},
		{"exactly at the delayed run", day(20, 8, 7), day(21, 8, 7)},
		{"after the delayed run", day(20, 8, 8), day(21, 8, 7)},
	} {
		got, err := NextCronTick("0 8 * * *", delay, tc.after)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: after %s got %s, want %s", tc.name, tc.after, got, tc.want)
		}
	}
	if got, _ := NextCronTick("0 8 * * *", 0, day(20, 8, 3)); !got.Equal(day(21, 8, 0)) {
		t.Errorf("without a delay the next run after 08:03 is tomorrow 08:00; got %s", got)
	}
}

func TestSchedulerDelaysRoundMinuteJobsOnlyWithASeed(t *testing.T) {
	add := func(seed, expr string) (*CronJob, time.Duration) {
		cs := NewCronService(filepath.Join(t.TempDir(), "cron", "jobs.json"), nil)
		cs.SetJitterSeed(seed)
		job, err := cs.AddJob("daily", CronSchedule{Kind: "cron", Expr: expr}, "hello", "cli", "direct")
		if err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		if job.State.NextRunAtMS == nil {
			t.Fatalf("%q got no next run", expr)
		}
		return job, Jitter(seed, job.ID, expr)
	}

	job, delay := add("assistant-a", "17 3 * * *")
	if delay == 0 {
		t.Fatal("a seeded round-minute job should be delayed")
	}
	nominal := time.UnixMilli(*job.State.NextRunAtMS).Add(-delay)
	if nominal.Hour() != 3 || nominal.Minute() != 17 || nominal.Second() != 0 {
		t.Errorf("next run minus its delay %s is not a 03:17 tick", nominal)
	}

	job, _ = add("", "17 3 * * *")
	if next := time.UnixMilli(*job.State.NextRunAtMS); next.Minute() != 17 || next.Second() != 0 {
		t.Errorf("without a seed the run stays on the minute; got %s", next)
	}

	job, _ = add("assistant-a", "*/5 * * * *")
	if next := time.UnixMilli(*job.State.NextRunAtMS); next.Minute()%5 != 0 || next.Second() != 0 {
		t.Errorf("a job that runs every 5 minutes keeps its exact times; got %s", next)
	}
}

// A store written before the job had a delay holds the round minute. A pod
// that wakes after that minute, for the delayed run, must not replay it as
// missed: the run would happen now and again at its delayed time.
func TestTurningOnJitterMovesAStoredRunWithoutReplayingIt(t *testing.T) {
	minute := time.Now().Add(-2 * time.Minute).Truncate(time.Minute)
	expr := fmt.Sprintf("%d %d * * *", minute.Minute(), minute.Hour())
	seed := ""
	for i := 0; seed == ""; i++ {
		if s := fmt.Sprintf("assistant-%d", i); Jitter(s, "daily", expr) >= 5*time.Minute {
			seed = s
		}
	}
	start := func(seed string) (*CronService, func() int) {
		job := CronJob{
			ID: "daily", Name: "daily", Enabled: true,
			Schedule: CronSchedule{Kind: "cron", Expr: expr},
			State:    CronJobState{NextRunAtMS: int64Ptr(minute.UnixMilli())},
		}
		runs, count := countRuns()
		cs := NewCronService(writeStore(t, job), runs)
		cs.SetJitterSeed(seed)
		if err := cs.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(cs.Stop)
		return cs, count
	}

	cs, count := start(seed)
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("replayed the stored minute %d time(s); its delayed run is still ahead", n)
	}
	job, _ := cs.GetJob("daily")
	want := minute.Add(Jitter(seed, "daily", expr))
	if job.State.NextRunAtMS == nil || !time.UnixMilli(*job.State.NextRunAtMS).Equal(want) {
		t.Errorf("next run %v, want the same minute delayed to %s", job.State.NextRunAtMS, want)
	}

	_, count = start("")
	waitForRuns(t, count, 1) // without a delay the minute two minutes ago is a missed run
}
