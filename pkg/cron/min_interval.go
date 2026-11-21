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

// The gaps of a cron expression are read from its first run on or after a
// fixed hour, over a period its pattern repeats in plus a margin.
var gapReference = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	dayHorizon  = 2 * 24 * time.Hour   // any day, month and weekday
	weekHorizon = 8 * 24 * time.Hour   // any day and month
	yearHorizon = 397 * 24 * time.Hour // a year and a month: December to January, short months
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
		if below, err := gapBelow(schedule.Expr, minimum); err == nil && below > 0 {
			gap = below
		}
	}
	if gap >= minimum {
		return nil
	}
	return fmt.Errorf("%w: two runs are %s apart, the minimum is %s",
		ErrIntervalTooShort, minutesText(gap), minutesText(minimum))
}

// gapBelow is the first gap between two consecutive runs of expr shorter than
// minimum, or 0 when there is none. It stops at the first one: Start checks
// every stored job.
func gapBelow(expr string, minimum time.Duration) (time.Duration, error) {
	segments, err := gronx.Segments(expr)
	if err != nil {
		return 0, err
	}
	first, err := gronx.NextTickAfter(expr, gapReference, true)
	if err != nil {
		return 0, err
	}
	horizon := dayHorizon
	switch {
	case !anyValue(segments[3]) || !anyValue(segments[4]):
		// shortcut: 2026 has no 29 February, so gaps next to one go unseen; check a leap year when that matters.
		horizon = yearHorizon
	case !anyValue(segments[5]):
		horizon = weekHorizon
	}
	// Pigeonhole: runs at least minimum apart fit horizon/minimum + 1 times in
	// the horizon, so the loop ends sooner, at a short gap or past the horizon.
	runs := int(horizon/minimum) + 1
	prev := first
	for range runs {
		next, err := gronx.NextTickAfter(expr, prev, false)
		// Past an expression's last run (its year is over), gronx fails or hands prev back.
		if err != nil || !next.After(prev) {
			return 0, nil //nolint:nilerr // no later run: no short gap
		}
		if gap := next.Sub(prev); gap < minimum {
			return gap, nil
		}
		if next.After(first.Add(horizon)) {
			return 0, nil
		}
		prev = next
	}
	// More runs than fit minimum apart: on average, and so somewhere, closer.
	return prev.Sub(first) / time.Duration(runs), nil
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
			from := *job.State.NextRunAtMS
			// The first run of a rewritten expression keeps the minimum after the last one;
			// "every" already starts a whole interval after the stored run.
			if last := job.State.LastRunAtMS; last != nil && job.Schedule.Kind == "cron" {
				from = max(from, *last+cs.minInterval.Milliseconds())
			}
			job.State.NextRunAtMS = cs.computeNextRun(&job.Schedule, from-1)
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
