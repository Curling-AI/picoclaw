package cron

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// startWithStoredJob boots a service over a jobs.json written beforehand, the
// way a pod that slept finds its store on wake.
func startWithStoredJob(t *testing.T, job CronJob) (*CronService, func() int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(CronStore{Version: 1, Jobs: []CronJob{job}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	runs := 0
	cs := NewCronService(path, func(*CronJob) (string, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		return "ok", nil
	})
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)
	return cs, func() int {
		mu.Lock()
		defer mu.Unlock()
		return runs
	}
}

func dailyJob(next, last time.Time) CronJob {
	state := CronJobState{NextRunAtMS: int64Ptr(next.UnixMilli())}
	if !last.IsZero() {
		state.LastRunAtMS = int64Ptr(last.UnixMilli())
	}
	return CronJob{
		ID:       "daily",
		Name:     "daily",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "cron", Expr: "0 9 * * *"},
		State:    state,
	}
}

func waitForRuns(t *testing.T, runs func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runs() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("runs = %d, want %d", runs(), want)
}

func nextRunOf(t *testing.T, cs *CronService, id string) time.Time {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := cs.GetJob(id)
		if !ok {
			t.Fatalf("job %s not found", id)
		}
		if job.State.NextRunAtMS != nil {
			return time.UnixMilli(*job.State.NextRunAtMS)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s has no next run", id)
	return time.Time{}
}

func TestStart_RunsAMissedOccurrenceWithinTheGrace(t *testing.T) {
	now := time.Now()
	cs, runs := startWithStoredJob(t, dailyJob(now.Add(-5*time.Minute), now.Add(-24*time.Hour)))

	waitForRuns(t, runs, 1)
	if next := nextRunOf(t, cs, "daily"); !next.After(now) {
		t.Errorf("next run after the catch-up = %s, want a future occurrence", next)
	}
	time.Sleep(200 * time.Millisecond)
	if got := runs(); got != 1 {
		t.Errorf("runs = %d, want exactly one catch-up", got)
	}
}

func TestStart_SkipsAMissedOccurrenceBeyondTheGrace(t *testing.T) {
	now := time.Now()
	cs, runs := startWithStoredJob(t, dailyJob(now.Add(-missedRunGrace-time.Minute), now.Add(-26*time.Hour)))

	if next := nextRunOf(t, cs, "daily"); !next.After(now) {
		t.Errorf("next run = %s, want the next occurrence", next)
	}
	time.Sleep(200 * time.Millisecond)
	if got := runs(); got != 0 {
		t.Errorf("runs = %d, want the stale occurrence skipped", got)
	}
}

func TestStart_DoesNotRepeatAnOccurrenceThatAlreadyRan(t *testing.T) {
	now := time.Now()
	cs, runs := startWithStoredJob(t, dailyJob(now.Add(-5*time.Minute), now.Add(-4*time.Minute)))

	if next := nextRunOf(t, cs, "daily"); !next.After(now) {
		t.Errorf("next run = %s, want the next occurrence", next)
	}
	time.Sleep(200 * time.Millisecond)
	if got := runs(); got != 0 {
		t.Errorf("runs = %d, want no repeat of a run that already happened", got)
	}
}

func TestStart_RunsAMissedOneShotWithinTheGrace(t *testing.T) {
	at := time.Now().Add(-5 * time.Minute).UnixMilli()
	_, runs := startWithStoredJob(t, CronJob{
		ID:             "once",
		Name:           "once",
		Enabled:        true,
		Schedule:       CronSchedule{Kind: "at", AtMS: &at},
		State:          CronJobState{NextRunAtMS: &at},
		DeleteAfterRun: true,
	})

	waitForRuns(t, runs, 1)
}

func TestMissedRun(t *testing.T) {
	now := time.Now().UnixMilli()
	minutesAgo := func(m int64) *int64 { return int64Ptr(now - m*60_000) }

	tests := []struct {
		name     string
		state    CronJobState
		wantLate time.Duration
		wantOK   bool
	}{
		{name: "no stored next run", state: CronJobState{}},
		{name: "next run still ahead", state: CronJobState{NextRunAtMS: int64Ptr(now + 60_000)}},
		{
			name:     "due and never ran",
			state:    CronJobState{NextRunAtMS: minutesAgo(5)},
			wantLate: 5 * time.Minute,
			wantOK:   true,
		},
		{
			name:     "due and last run is older",
			state:    CronJobState{NextRunAtMS: minutesAgo(5), LastRunAtMS: minutesAgo(24 * 60)},
			wantLate: 5 * time.Minute,
			wantOK:   true,
		},
		{name: "due but already ran", state: CronJobState{NextRunAtMS: minutesAgo(5), LastRunAtMS: minutesAgo(5)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			late, ok := missedRun(tt.state, now)
			if ok != tt.wantOK || late != tt.wantLate {
				t.Errorf("missedRun = (%s, %v), want (%s, %v)", late, ok, tt.wantLate, tt.wantOK)
			}
		})
	}
}
