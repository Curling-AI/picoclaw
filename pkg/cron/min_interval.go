package cron

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/adhocore/gronx"
)

// ErrIntervalTooShort is returned, wrapped with both intervals, when a
// recurring job would run more often than the minimum (SetMinInterval).
var ErrIntervalTooShort = errors.New("schedule runs too often")

// The gap of a cron expression is read from its runs over a week and a day
// from its first run on or after a fixed hour: every pattern of minutes, hours
// and weekdays repeats within a week.
var gapReference = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	gapHorizon = 8 * 24 * time.Hour
	// Reaching it means the runs average under 5 minutes apart, and the
	// shortest gap found by then is at most that average.
	maxGapTicks = 2500
)

// SetMinInterval sets the shortest time allowed between two runs of a
// recurring job; 0, the default, allows any. Adding or rescheduling a job that
// runs more often fails with ErrIntervalTooShort, and Start rewrites the stored
// jobs that do (raiseShortSchedules). Call it before Start: the rewrite happens
// there, under the store lock, before the boot replays missed runs.
func (cs *CronService) SetMinInterval(d time.Duration) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	// shortcut: a minimum above an hour counts as one hour, since the rewrite
	// changes only the minute field; rewrite the hour field too when one is needed.
	cs.minInterval = min(d, time.Hour)
}

// MinInterval is the minimum in force, after SetMinInterval's cap.
func (cs *CronService) MinInterval() time.Duration {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.minInterval
}

// checkInterval fails with ErrIntervalTooShort when schedule runs more often
// than minimum. One-time runs pass, and so do schedules that never run (a
// cron expression that does not parse): computeNextRun already logs those.
func checkInterval(schedule CronSchedule, minimum time.Duration) error {
	if minimum <= 0 {
		return nil
	}
	gap := time.Duration(math.MaxInt64)
	switch schedule.Kind {
	case "every":
		// Compared in milliseconds: a huge EveryMS would overflow a Duration.
		if schedule.EveryMS != nil && *schedule.EveryMS > 0 && *schedule.EveryMS < minimum.Milliseconds() {
			gap = time.Duration(*schedule.EveryMS) * time.Millisecond
		}
	case "cron":
		if shortest, err := shortestGap(schedule.Expr); err == nil {
			gap = shortest
		}
	}
	if gap >= minimum {
		return nil
	}
	return fmt.Errorf("%w: two runs are %s apart, the minimum is %s",
		ErrIntervalTooShort, minutesText(gap), minutesText(minimum))
}

// shortestGap is the shortest time between two consecutive runs of expr.
func shortestGap(expr string) (time.Duration, error) {
	prev, err := gronx.NextTickAfter(expr, gapReference, true)
	if err != nil {
		return 0, err
	}
	end := prev.Add(gapHorizon)
	shortest := time.Duration(math.MaxInt64)
	for range maxGapTicks {
		next, err := gronx.NextTickAfter(expr, prev, false)
		// Past an expression's last run (its year is over), gronx fails or hands prev back.
		if err != nil || !next.After(prev) {
			return shortest, nil //nolint:nilerr // no later run: no shorter gap
		}
		shortest = min(shortest, next.Sub(prev))
		if next.After(end) {
			break
		}
		prev = next
	}
	return shortest, nil
}

func anyValue(segment string) bool { return segment == "*" || segment == "?" }

func minutesText(d time.Duration) string {
	if d%time.Minute != 0 {
		return d.String()
	}
	return fmt.Sprintf("%d min", d/time.Minute)
}

// raiseShortSchedules rewrites the stored jobs, enabled or paused, that run
// more often than the minimum: saved before it, or under a lower one, they
// would otherwise refuse every edit while running as before. A stored run
// moves to the new schedule's first run at or after it, so recomputeNextRuns
// replays it only if the new schedule has it. The caller holds cs.mu.
func (cs *CronService) raiseShortSchedules() {
	for i := range cs.store.Jobs {
		job := &cs.store.Jobs[i]
		if checkInterval(job.Schedule, cs.minInterval) == nil {
			continue
		}
		raised, err := raiseSchedule(job.Schedule, cs.minInterval)
		if err == nil {
			err = checkInterval(raised, cs.minInterval)
		}
		if err != nil {
			log.Printf("[cron] job %s runs more often than %s and could not be slowed down: %v",
				job.ID, cs.minInterval, err)
			continue
		}
		log.Printf("[cron] job %s ran more often than %s: schedule %q rewritten to %q",
			job.ID, cs.minInterval, scheduleText(job.Schedule), scheduleText(raised))
		job.Schedule = raised
		if job.State.NextRunAtMS != nil {
			job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, *job.State.NextRunAtMS-1)
		}
	}
}

// raiseSchedule slows schedule down to minimum, at most an hour. An expression
// keeps every field but the minute, which becomes */M with M the smallest
// divisor of 60 at or above minimum, or, when that is 60, the first minute the
// expression ran at. Names (MON, JAN) come back as numbers, a seconds field
// is dropped (runs start on the minute) and so is a year of "*".
func raiseSchedule(schedule CronSchedule, minimum time.Duration) (CronSchedule, error) {
	if schedule.Kind == "every" {
		everyMS := minimum.Milliseconds()
		schedule.EveryMS = &everyMS
		return schedule, nil
	}
	segments, err := gronx.Segments(schedule.Expr)
	if err != nil {
		return schedule, err
	}
	minute := ""
	for _, step := range []int{1, 2, 3, 4, 5, 6, 10, 12, 15, 20, 30} {
		if time.Duration(step)*time.Minute >= minimum {
			minute = "*/" + strconv.Itoa(step)
			break
		}
	}
	if minute == "" {
		// From a whole hour, the first run is at the smallest minute of the set.
		first, err := gronx.NextTickAfter(schedule.Expr, gapReference, true)
		if err != nil {
			return schedule, err
		}
		minute = strconv.Itoa(first.Minute())
	}
	segments[1] = minute
	fields := segments[1:6]
	if len(segments) == 7 && !anyValue(segments[6]) {
		fields = append([]string{"0"}, segments[1:]...) // a year needs the seven-field form
	}
	expr := strings.Join(fields, " ")
	if !gronx.IsValid(expr) {
		return schedule, fmt.Errorf("rewritten expression %q does not parse", expr)
	}
	schedule.Expr = expr
	return schedule, nil
}

func scheduleText(schedule CronSchedule) string {
	if schedule.Kind == "every" && schedule.EveryMS != nil {
		return "every " + (time.Duration(*schedule.EveryMS) * time.Millisecond).String()
	}
	return schedule.Expr
}
