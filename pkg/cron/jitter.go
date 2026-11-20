package cron

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/adhocore/gronx"
)

// MaxJitter is how long after its minute a cron run may start. Schedules that
// share a round minute would otherwise all start, and wake their hosts, in the
// same instant.
const MaxJitter = 10 * time.Minute

// Jitter is the fixed delay of one job's cron runs. It comes from the seed and
// the job ID alone, so whoever wakes the scheduler ahead of a run computes the
// same time. Only an expression pinned to one minute of the hour is delayed: a
// job that runs several times an hour keeps its exact times. An empty seed
// turns it off.
func Jitter(seed, jobID, expr string) time.Duration {
	if seed == "" || !pinnedMinute(expr) {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(seed))
	h.Write([]byte{0})
	h.Write([]byte(jobID))
	return time.Duration(h.Sum64()%uint64(MaxJitter/time.Second)) * time.Second
}

// NextCronTick is the first run of expr, delayed by jitter, strictly after
// `after`. The tick is looked up from `after - jitter`, so a run already due
// but still inside its delay is not skipped to the next occurrence.
func NextCronTick(expr string, jitter time.Duration, after time.Time) (time.Time, error) {
	t, err := gronx.NextTickAfter(expr, after.Add(-jitter), false)
	if err != nil {
		return time.Time{}, err
	}
	return t.Add(jitter), nil
}

// pinnedMinute: five fields with a single minute, or a macro that is one.
func pinnedMinute(expr string) bool {
	fields := strings.Fields(expr)
	if len(fields) == 1 {
		switch strings.ToLower(fields[0]) {
		case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@hourly":
			return true
		}
		return false
	}
	if len(fields) != 5 {
		return false
	}
	minute, err := strconv.Atoi(fields[0])
	return err == nil && minute >= 0 && minute <= 59
}
