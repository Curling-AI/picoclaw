package cron

import "errors"

var errScheduledClaimInProgress = errors.New("another scheduler claimed a due job")

// Scheduler updates share the startup store lock so each owner can change only
// the jobs it actually claimed or ran. The caller holds cs.mu.
func (cs *CronService) schedulerStoreUnsafe() (*CronStore, func(), error) {
	if !cs.durableAdmission {
		return cs.store, func() {}, nil
	}
	unlock, err := lockStore(cs.storePath, storeLockTimeout)
	if err != nil {
		return nil, nil, err
	}
	stored, err := readCronStore(cs.storePath)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return stored, unlock, nil
}

func (cs *CronService) claimDueJobsUnsafe(now int64) ([]string, error) {
	var candidates map[string]int64
	for _, job := range cs.store.Jobs {
		if job.Enabled && !cs.hasAdmissionRetry(job.ID) && job.State.NextRunAtMS != nil &&
			*job.State.NextRunAtMS <= now {
			if candidates == nil {
				candidates = make(map[string]int64)
			}
			candidates[job.ID] = *job.State.NextRunAtMS
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	stored, unlock, err := cs.schedulerStoreUnsafe()
	if err != nil {
		return nil, err
	}
	defer unlock()

	var claimed []string
	waitForOwner := false
	current := make(map[string]*CronJob, len(candidates))
	for i := range stored.Jobs {
		job := &stored.Jobs[i]
		expected, ok := candidates[job.ID]
		if !ok {
			continue
		}
		if job.Enabled && job.State.NextRunAtMS == nil {
			// Keep the local timestamp as a polling trigger. It cannot be
			// claimed until the store contains that same timestamp again.
			waitForOwner = true
			delete(candidates, job.ID)
			continue
		}
		current[job.ID] = job
		if job.Enabled && job.State.NextRunAtMS != nil && *job.State.NextRunAtMS == expected {
			job.State.NextRunAtMS = nil
			claimed = append(claimed, job.ID)
		}
	}
	if len(claimed) != 0 {
		err = writeCronStore(cs.storePath, stored)
	}
	if !cs.durableAdmission {
		return claimed, err
	}
	if err != nil {
		// The old store may still be visible to another scheduler. Do not
		// dispatch or mutate local candidates until the claim is persisted.
		return nil, err
	}
	for jobID := range candidates {
		cs.syncStoredJobUnsafe(jobID, current[jobID])
	}
	if waitForOwner {
		return claimed, errScheduledClaimInProgress
	}
	return claimed, nil
}

func (cs *CronService) syncStoredJobUnsafe(jobID string, job *CronJob) {
	if job == nil {
		cs.removeJobUnsafe(jobID)
		return
	}
	for i := range cs.store.Jobs {
		if cs.store.Jobs[i].ID == jobID {
			cs.store.Jobs[i] = *job
			return
		}
	}
}
