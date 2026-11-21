package cron

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestGapBelow(t *testing.T) {
	const hour = time.Hour
	for _, tc := range []struct {
		expr    string
		minimum time.Duration
		want    time.Duration // 0: no two runs closer than minimum
	}{
		{"*/5 * * * *", 30 * time.Minute, 5 * time.Minute},
		{"*/30 * * * *", 30 * time.Minute, 0},
		{"*/45 * * * *", 30 * time.Minute, 15 * time.Minute}, // runs at :00 and :45
		{"0,10 8 * * *", 30 * time.Minute, 10 * time.Minute},
		{"0 8 * * *", hour, 0},
		{"0 9-17 * * 1-5", hour, 0},
		{"0 9-17 * * 1-5", hour + time.Minute, hour},
		{"0 4 * * 1,4", hour, 0},
		{"@hourly", hour + time.Minute, hour},
		{"@daily", hour, 0},
		{"@30minutes", 31 * time.Minute, 30 * time.Minute},
		{"0,10 8 1 * *", 30 * time.Minute, 10 * time.Minute},          // first run weeks after the reference
		{"0,50 0,23 1,15,16 * *", 30 * time.Minute, 10 * time.Minute}, // 15th 23:50 to 16th 00:00
		{"0,50 0,23 1,31 1,12 *", 30 * time.Minute, 10 * time.Minute}, // 31 December to 1 January
		{"*/10 * * * * *", 30 * time.Minute, 10 * time.Second},
		{"0,10 8 31 12 * 2026", 30 * time.Minute, 10 * time.Minute},
		{"0 8 31 12 * 2026", 30 * time.Minute, 0}, // gronx hands the last run back
		{"* * * * *", 30 * time.Minute, time.Minute},
	} {
		got, err := gapBelow(tc.expr, tc.minimum)
		if err != nil || got != tc.want {
			t.Errorf("gapBelow(%q, %s) = %s, %v; want %s", tc.expr, tc.minimum, got, err, tc.want)
		}
	}
	if _, err := gapBelow("not a cron", time.Hour); err == nil {
		t.Error("an expression that does not parse must fail")
	}
}

func TestRaiseScheduleKeepsEveryFieldButTheMinute(t *testing.T) {
	for _, tc := range []struct {
		expr    string
		minimum time.Duration
		want    string
	}{
		{"*/5 * * * *", 30 * time.Minute, "*/30 * * * *"},
		{"0,10 8 * * *", 30 * time.Minute, "*/30 8 * * *"},
		{"*/5 9-17 * * MON-FRI", 30 * time.Minute, "*/30 9-17 * * 1-5"},
		{"@5minutes", 30 * time.Minute, "*/30 * * * *"},
		{"*/10 * * * * *", 30 * time.Minute, "*/30 * * * *"},
		{"*/5 * * * *", 7 * time.Minute, "*/10 * * * *"},
		{"*/5 * * * *", time.Hour, "0 * * * *"},
		{"10,40 8 * * *", 45 * time.Minute, "10 8 * * *"},
		{"20-50/5 6,18 * * *", time.Hour, "20 6,18 * * *"},
		{"0 */5 8 25 * * *", 30 * time.Minute, "*/30 8 25 * *"}, // seven fields, any year
		{"0 */5 8 25 12 * 2027", 30 * time.Minute, "0 */30 8 25 12 * 2027"},
	} {
		raised, err := raiseSchedule(CronSchedule{Kind: "cron", Expr: tc.expr}, tc.minimum)
		if err != nil || raised.Expr != tc.want {
			t.Errorf("raiseSchedule(%q, %s) = %q, %v; want %q", tc.expr, tc.minimum, raised.Expr, err, tc.want)
			continue
		}
		if err := checkInterval(raised, tc.minimum); err != nil {
			t.Errorf("%q raised to %q still runs too often: %v", tc.expr, raised.Expr, err)
		}
	}
}

func TestMinIntervalRefusesShorterSchedulesOnAddAndUpdate(t *testing.T) {
	cs := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	cs.SetMinInterval(30 * time.Minute)
	every := func(d time.Duration) CronSchedule {
		ms := d.Milliseconds()
		return CronSchedule{Kind: "every", EveryMS: &ms}
	}

	_, err := cs.AddJob("often", every(5*time.Minute), "hello", "cli", "direct")
	if !errors.Is(err, ErrIntervalTooShort) {
		t.Fatalf("every 5 min: got %v, want ErrIntervalTooShort", err)
	}
	if want := "schedule runs too often: two runs are 5 min apart, the minimum is 30 min"; err.Error() != want {
		t.Errorf("error text %q, want %q", err, want)
	}
	if _, err := cs.AddJob(
		"often",
		CronSchedule{Kind: "cron", Expr: "*/5 * * * *"},
		"hello",
		"cli",
		"direct",
	); !errors.Is(
		err,
		ErrIntervalTooShort,
	) {
		t.Errorf("*/5: got %v, want ErrIntervalTooShort", err)
	}
	paused := CronSchedule{Kind: "cron", Expr: "0,10 8 * * *"}
	if _, err := cs.AddPausedJobWithPayload(
		"often",
		paused,
		CronPayload{Message: "hello"},
	); !errors.Is(
		err,
		ErrIntervalTooShort,
	) {
		t.Errorf("paused 0,10 8: got %v, want ErrIntervalTooShort", err)
	}

	at := time.Now().Add(time.Minute).UnixMilli()
	for _, schedule := range []CronSchedule{
		every(30 * time.Minute),
		{Kind: "cron", Expr: "*/30 * * * *"},
		{Kind: "at", AtMS: &at},
	} {
		if _, err := cs.AddJob("ok", schedule, "hello", "cli", "direct"); err != nil {
			t.Errorf("%s at exactly the minimum (or one-time): %v", scheduleText(schedule), err)
		}
	}
	if n := len(cs.ListJobs(true)); n != 3 {
		t.Fatalf("store has %d jobs, want only the 3 accepted ones", n)
	}

	job := cs.ListJobs(true)[1]
	job.Schedule = CronSchedule{Kind: "cron", Expr: "*/10 * * * *"}
	if err := cs.UpdateJob(&job); !errors.Is(err, ErrIntervalTooShort) {
		t.Errorf("update to */10: got %v, want ErrIntervalTooShort", err)
	}
	if stored, _ := cs.GetJob(job.ID); stored.Schedule.Expr != "*/30 * * * *" {
		t.Errorf("a refused update changed the job to %q", stored.Schedule.Expr)
	}
	job.Schedule = CronSchedule{Kind: "cron", Expr: "0 8 * * *"}
	if err := cs.UpdateJob(&job); err != nil {
		t.Errorf("update to a daily run: %v", err)
	}

	if _, err := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil).
		AddJob("often", every(5*time.Minute), "hello", "cli", "direct"); err != nil {
		t.Errorf("without a minimum every 5 min is allowed: %v", err)
	}
}

func TestStartRewritesStoredJobsBelowTheMinimum(t *testing.T) {
	fiveMinutes := (5 * time.Minute).Milliseconds()
	stored := []CronJob{
		{ID: "cron-5", Name: "a", Enabled: true, Schedule: CronSchedule{Kind: "cron", Expr: "*/5 * * * *"}},
		{ID: "every-5", Name: "b", Enabled: true, Schedule: CronSchedule{Kind: "every", EveryMS: &fiveMinutes}},
		{ID: "daily", Name: "c", Enabled: true, Schedule: CronSchedule{Kind: "cron", Expr: "0 8 * * *"}},
		{ID: "paused", Name: "d", Schedule: CronSchedule{Kind: "cron", Expr: "0,10 8 * * *"}},
	}
	for _, tc := range []struct {
		minimum time.Duration
		want    map[string]string
	}{
		{0, map[string]string{"cron-5": "*/5 * * * *", "every-5": "every 5m0s", "daily": "0 8 * * *", "paused": "0,10 8 * * *"}},
		{30 * time.Minute, map[string]string{"cron-5": "*/30 * * * *", "every-5": "every 30m0s", "daily": "0 8 * * *", "paused": "*/30 8 * * *"}},
	} {
		path := writeStore(t, stored...)
		cs := NewCronService(path, nil)
		cs.SetMinInterval(tc.minimum)
		if err := cs.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		cs.Stop()

		saved, err := readCronStore(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range saved.Jobs {
			if got := scheduleText(job.Schedule); got != tc.want[job.ID] {
				t.Errorf("minimum %s: job %s saved as %q, want %q", tc.minimum, job.ID, got, tc.want[job.ID])
			}
			if job.Enabled != (job.ID != "paused") || (job.State.NextRunAtMS == nil) != (job.ID == "paused") {
				t.Errorf("minimum %s: job %s enabled=%v next=%v; a rewrite must not pause or resume it",
					tc.minimum, job.ID, job.Enabled, job.State.NextRunAtMS)
			}
		}
	}
}

// A stored run the new schedule does not have is not replayed at boot.
func TestRaisingAJobDoesNotReplayARunItsNewScheduleLacks(t *testing.T) {
	fiveMinutes := (5 * time.Minute).Milliseconds()
	start := func(minimum time.Duration) (*CronService, func() int) {
		runs, count := countRuns()
		cs := NewCronService(writeStore(t, CronJob{
			ID: "often", Name: "often", Enabled: true,
			Schedule: CronSchedule{Kind: "every", EveryMS: &fiveMinutes},
			State:    CronJobState{NextRunAtMS: int64Ptr(time.Now().Add(-2 * time.Minute).UnixMilli())},
		}), runs)
		cs.SetMinInterval(minimum)
		if err := cs.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(cs.Stop)
		return cs, count
	}

	cs, count := start(30 * time.Minute)
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("replayed the old 5-minute run %d time(s)", n)
	}
	if next := time.Until(nextRunOf(t, cs, "often")); next < 29*time.Minute || next > 30*time.Minute {
		t.Errorf("next run in %s, want about 30 min", next)
	}

	_, count = start(0)
	waitForRuns(t, count, 1) // without a minimum it is a missed run
}

// Even a stored run the new schedule has is not replayed at the boot that
// rewrites the job: it would come sooner than the minimum after the last run.
func TestRaisingAJobDoesNotReplayEvenARunItsNewScheduleHas(t *testing.T) {
	minute := time.Now().Add(-2 * time.Minute).Truncate(time.Minute)
	if minute.Minute() == 59 {
		minute = minute.Add(-time.Minute) // keep a range of minutes to raise
	}
	runs, count := countRuns()
	cs := NewCronService(writeStore(t, CronJob{
		ID: "often", Name: "often", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: fmt.Sprintf("%d-59 %d * * *", minute.Minute(), minute.Hour())},
		State:    CronJobState{NextRunAtMS: int64Ptr(minute.UnixMilli())},
	}), runs)
	cs.SetMinInterval(time.Hour)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)

	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("replayed the stored minute %d time(s)", n)
	}
	job, _ := cs.GetJob("often")
	if raised := fmt.Sprintf("%d %d * * *", minute.Minute(), minute.Hour()); job.Schedule.Expr != raised {
		t.Errorf("schedule %q, want %q", job.Schedule.Expr, raised)
	}
	if next := nextRunOf(t, cs, "often"); !next.Equal(minute.AddDate(0, 0, 1)) {
		t.Errorf("next run %s, want the same minute tomorrow", next)
	}
}

// Start checks every stored job against the minimum before the pod serves its
// wake, so dense schedules that are allowed must stay cheap to check.
func TestCheckingManyAllowedDenseJobsAtBootIsQuick(t *testing.T) {
	jobs := make([]CronJob, 20)
	for i := range jobs {
		jobs[i] = CronJob{
			ID: fmt.Sprintf("job-%d", i), Name: "dense", Enabled: true,
			Schedule: CronSchedule{Kind: "cron", Expr: "*/30 * * * *"},
		}
	}
	cs := NewCronService(writeStore(t, jobs...), nil)
	cs.SetMinInterval(30 * time.Minute)
	cs.mu.Lock()
	began := time.Now()
	cs.raiseShortSchedules(time.Now().UnixMilli())
	took := time.Since(began)
	cs.mu.Unlock()
	// About 12 ms on a laptop; a whole year of runs per job takes seconds.
	t.Logf("checked %d jobs in %s", len(jobs), took)
	if took > 250*time.Millisecond {
		t.Errorf("checking %d allowed */30 jobs took %s", len(jobs), took)
	}
}

// The first run after a boot rewrite comes no sooner than the boot or the
// minimum after the last run, and never replays the stored one.
func TestRaisingAJobKeepsTheMinimumAfterItsLastRun(t *testing.T) {
	at := func(h, m int) int64 { return time.Date(2026, 3, 2, h, m, 0, 0, time.Local).UnixMilli() }
	fiveMinutes := (5 * time.Minute).Milliseconds()
	cron5 := CronSchedule{Kind: "cron", Expr: "*/5 * * * *"}
	every5 := CronSchedule{Kind: "every", EveryMS: &fiveMinutes}
	for _, tc := range []struct {
		name            string
		schedule        CronSchedule
		last, now, want int64
	}{
		{"ran 5 minutes before the boot", cron5, at(10, 25), at(10, 28), at(11, 0)},
		{"ran long ago, stored run ahead", cron5, at(9, 0), at(10, 28), at(10, 30)},
		{"stored run expired an hour ago", cron5, at(10, 25), at(11, 31), at(12, 0)},
		{"every, ran 5 minutes before the boot", every5, at(10, 25), at(10, 28), at(10, 58)},
	} {
		cs := NewCronService(writeStore(t, CronJob{
			ID: "often", Name: "often", Enabled: true, Schedule: tc.schedule,
			State: CronJobState{LastRunAtMS: int64Ptr(tc.last), NextRunAtMS: int64Ptr(at(10, 30))},
		}), nil)
		cs.SetMinInterval(30 * time.Minute)
		cs.mu.Lock()
		rewritten := cs.raiseShortSchedules(tc.now)
		job := cs.store.Jobs[0]
		cs.mu.Unlock()
		if !rewritten["often"] || job.State.NextRunAtMS == nil || *job.State.NextRunAtMS != tc.want {
			t.Errorf("%s: rewritten=%v %s next %v, want %s", tc.name, rewritten, scheduleText(job.Schedule),
				job.State.NextRunAtMS, time.UnixMilli(tc.want))
		}
	}
}

// Through Start: a job that ran 5 minutes ago and is rewritten to a minute
// due in two waits a whole minimum after its last run instead.
func TestBootAfterARewriteWaitsTheMinimumAfterTheLastRun(t *testing.T) {
	now := time.Now()
	soon := now.Add(2 * time.Minute).Truncate(time.Minute)
	if soon.Minute() == 59 {
		soon = soon.Add(time.Minute) // keep a range of minutes to raise
	}
	last := now.Add(-5 * time.Minute)
	runs, count := countRuns()
	cs := NewCronService(writeStore(t, CronJob{
		ID: "often", Name: "often", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: fmt.Sprintf("%d-59 * * * *", soon.Minute())},
		State: CronJobState{
			LastRunAtMS: int64Ptr(last.UnixMilli()),
			NextRunAtMS: int64Ptr(now.Add(-time.Minute).UnixMilli()),
		},
	}), runs)
	cs.SetMinInterval(time.Hour)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)

	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("the rewritten job caught up %d time(s) at boot", n)
	}
	if next := nextRunOf(t, cs, "often"); next.Before(last.Add(time.Hour)) {
		t.Errorf("next run %s, sooner than an hour after the last run at %s", next, last)
	}
}

// Through Start: a rewritten job whose stored run expired over an hour ago
// does not catch up, although a run of its new schedule fell within the grace.
func TestBootDoesNotReplayALongExpiredRunOfARewrittenJob(t *testing.T) {
	now := time.Now()
	runs, count := countRuns()
	cs := NewCronService(writeStore(t, CronJob{
		ID: "often", Name: "often", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: "*/5 * * * *"},
		State: CronJobState{
			LastRunAtMS: int64Ptr(now.Add(-66 * time.Minute).UnixMilli()),
			NextRunAtMS: int64Ptr(now.Add(-61 * time.Minute).UnixMilli()),
		},
	}), runs)
	cs.SetMinInterval(30 * time.Minute)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)

	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("the rewritten job caught up %d time(s) at boot", n)
	}
	if next := nextRunOf(t, cs, "often"); next.Before(now) {
		t.Errorf("next run %s is in the past", next)
	}
}

// A second restart keeps the minimum too: the job rewritten at the first boot is
// compliant now, and recomputing it from now must not land closer to its last run.
func TestRestartKeepsTheMinimumAfterTheLastRunOfACompliantJob(t *testing.T) {
	now := time.Now()
	tick := now.Truncate(30 * time.Minute).Add(30 * time.Minute) // the next */30 run after now
	last := tick.Add(-5 * time.Minute)                           // so that run is under 30 min after the last one
	if last.After(now) {
		last = now
	}
	runs, count := countRuns()
	cs := NewCronService(writeStore(t, CronJob{
		ID: "rewritten", Name: "rewritten", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: "*/30 * * * *"},
		State: CronJobState{
			LastRunAtMS: int64Ptr(last.UnixMilli()),
			NextRunAtMS: int64Ptr(tick.Add(30 * time.Minute).UnixMilli()),
		},
	}), runs)
	cs.SetMinInterval(30 * time.Minute)
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)
	if next := nextRunOf(t, cs, "rewritten"); next.Before(last.Add(30 * time.Minute)) {
		t.Errorf("next run %s is under 30 min after the last one at %s", next, last)
	}
	if n := count(); n != 0 {
		t.Errorf("ran %d time(s) at the restart", n)
	}
}
