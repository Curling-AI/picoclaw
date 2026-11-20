package cron

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/adhocore/gronx"
)

// ErrTooManyRuns is returned, wrapped with both numbers, when the jobs that
// count toward the cap (SetMaxRunsPerDay) would run more often than it allows.
var ErrTooManyRuns = errors.New("too many runs a day")

// SetMaxRunsPerDay caps the runs a day of all enabled recurring jobs together;
// 0, the default, sets no cap. Adding, rescheduling or resuming a job past it
// fails with ErrTooManyRuns, and Start pauses the newest jobs until the stored
// ones fit (pauseJobsPastCap). Call it, and SetCapExempt, before jobs are added
// and before Start.
func (cs *CronService) SetMaxRunsPerDay(n int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.maxRunsPerDay = n
}

// MaxRunsPerDay is the cap set by SetMaxRunsPerDay.
func (cs *CronService) MaxRunsPerDay() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.maxRunsPerDay
}

// SetCapExempt names the jobs that neither count toward the cap nor are paused
// by it: the platform's own. It runs under the service lock, so it must be
// fast and must not change the job or call the service.
func (cs *CronService) SetCapExempt(exempt func(job *CronJob) bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.capExempt = exempt
}

func (cs *CronService) countsTowardCap(job *CronJob) bool {
	return job.Enabled && job.Schedule.Kind != "at" && (cs.capExempt == nil || !cs.capExempt(job))
}

// checkRunCap fails with ErrTooManyRuns when job, in place of its stored
// version, would take the counted jobs past the cap. The total adds each
// job's busiest day, an upper bound: two jobs' busiest days need not be the
// same day. The caller holds cs.mu.
func (cs *CronService) checkRunCap(job CronJob) error {
	if cs.maxRunsPerDay <= 0 || !cs.countsTowardCap(&job) {
		return nil
	}
	total := dailyRuns(job.Schedule, time.Local, cs.maxRunsPerDay)
	for i := range cs.store.Jobs {
		if other := &cs.store.Jobs[i]; other.ID != job.ID && cs.countsTowardCap(other) {
			total += dailyRuns(other.Schedule, time.Local, cs.maxRunsPerDay)
		}
	}
	if total <= cs.maxRunsPerDay {
		return nil
	}
	return fmt.Errorf("%w: automations would run %d times a day, the limit is %d",
		ErrTooManyRuns, total, cs.maxRunsPerDay)
}

// pauseJobsPastCap pauses the newest counted jobs, saved before the cap or
// under a higher one, until the rest fit it. Paused here, before
// recomputeNextRuns, they are not replayed as missed. The caller holds cs.mu.
func (cs *CronService) pauseJobsPastCap() {
	if cs.maxRunsPerDay <= 0 {
		return
	}
	var counted []int
	for i := range cs.store.Jobs {
		if cs.countsTowardCap(&cs.store.Jobs[i]) {
			counted = append(counted, i)
		}
	}
	jobs := cs.store.Jobs
	slices.SortFunc(counted, func(a, b int) int {
		return cmp.Or(cmp.Compare(jobs[a].CreatedAtMS, jobs[b].CreatedAtMS), strings.Compare(jobs[a].ID, jobs[b].ID))
	})
	loads := make([]int, len(counted))
	total := 0
	for k, i := range counted {
		loads[k] = dailyRuns(jobs[i].Schedule, time.Local, cs.maxRunsPerDay)
		total += loads[k]
	}
	for k := len(counted) - 1; k >= 0 && total > cs.maxRunsPerDay; k-- {
		job := &jobs[counted[k]]
		job.Enabled = false
		job.State.NextRunAtMS = nil
		log.Printf("[cron] job %s paused: its %d runs a day took the automations to %d, the limit is %d",
			job.ID, loads[k], total, cs.maxRunsPerDay)
		total -= loads[k]
	}
}

// dailyRuns is the most runs schedule makes on one calendar day in loc, the
// zone computeNextRun reads expressions in. Counting stops past limit: a job
// over it alone is reported as at least limit + 1.
func dailyRuns(schedule CronSchedule, loc *time.Location, limit int) int {
	switch schedule.Kind {
	case "every":
		if schedule.EveryMS == nil || *schedule.EveryMS <= 0 {
			return 0
		}
		day, every := (24 * time.Hour).Milliseconds(), *schedule.EveryMS
		if day%every == 0 {
			return int(day / every)
		}
		return int(day/every) + 1
	case "cron":
		return dailyCronRuns(schedule.Expr, loc, limit)
	}
	return 0
}

func dailyCronRuns(expr string, loc *time.Location, limit int) int {
	segments, err := gronx.Segments(expr)
	if err != nil {
		return 0 // never runs: computeNextRun logs it
	}
	start := time.Date(gapReference.Year(), gapReference.Month(), gapReference.Day(), 0, 0, 0, 0, loc)
	end := start.Add(repeatHorizon(segments))
	// Each day of the horizon (one more for a daylight-saving shift) counts at
	// most limit + 1 runs before the loop returns.
	bound := (int(end.Sub(start)/(24*time.Hour)) + 1) * (limit + 1)
	most, count, day := 0, 0, 0
	prev := start.Add(-time.Second)
	tick, err := gronx.NextTickAfter(expr, start, true)
	for range bound {
		// Past an expression's last run (its year is over), gronx fails or hands it back.
		if err != nil || !tick.After(prev) || !tick.Before(end) {
			break
		}
		if d := 400*tick.Year() + tick.YearDay(); d != day {
			day, count = d, 0
		}
		count++
		if most = max(most, count); most > limit {
			break
		}
		prev = tick
		tick, err = gronx.NextTickAfter(expr, tick, false)
	}
	return most
}
