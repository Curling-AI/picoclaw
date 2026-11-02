package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

const (
	admissionPrepared  = "prepared"
	admissionExecuting = "executing"
	admissionFinished  = "finished"
	admissionRetention = 7 * 24 * time.Hour
)

type deferredExecutionError struct {
	cause error
	delay time.Duration
}

func (e *deferredExecutionError) Error() string { return e.cause.Error() }
func (e *deferredExecutionError) Unwrap() error { return e.cause }

// RetryExecutionAfter preserves an admission without consuming its occurrence.
// Only an admission-aware handler that has not claimed effects may use it.
func RetryExecutionAfter(err error, delay time.Duration) error {
	if err == nil {
		return nil
	}
	return &deferredExecutionError{cause: err, delay: max(delay, time.Second)}
}

type executionAdmission struct {
	RunID          string    `json:"runId"`
	StartedAt      time.Time `json:"startedAt"`
	Trigger        Trigger   `json:"trigger"`
	ManualIdentity bool      `json:"manualIdentity"`
	Job            CronJob   `json:"job"`
	State          string    `json:"state"`
	RetryAtMS      int64     `json:"retryAtMs"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Retried        bool      `json:"retried"`
	Outcome        string    `json:"outcome,omitempty"`
}

func (cs *CronService) admissionPath(run string) string {
	return filepath.Join(cs.storePath+".admissions", run+".json")
}

func (cs *CronService) writeExecutionAdmission(admission *executionAdmission) error {
	admission.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(admission)
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(cs.admissionPath(admission.RunID), data, 0o600)
}

func readExecutionAdmission(path string) (executionAdmission, error) {
	var admission executionAdmission
	info, err := os.Lstat(path)
	if err != nil {
		return admission, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return admission, errors.New("invalid cron admission journal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return admission, err
	}
	if err := json.Unmarshal(data, &admission); err != nil {
		return admission, err
	}
	if uuid.Validate(admission.RunID) != nil || admission.RunID == uuid.Nil.String() || admission.Job.ID == "" ||
		admission.StartedAt.IsZero() ||
		(admission.Trigger != Manual && admission.Trigger != Scheduled) ||
		(admission.State != admissionPrepared && admission.State != admissionExecuting && admission.State != admissionFinished) {
		return admission, errors.New("invalid cron admission identity")
	}
	return admission, nil
}

func (cs *CronService) beginExecutionAdmission(execution Execution) (*executionAdmission, func() error, func(), error) {
	if runtime.GOOS == "windows" {
		return nil, nil, nil, errors.New("durable cron admission requires advisory file locking")
	}
	path := cs.admissionPath(execution.RunID)
	unlock, err := lockStore(path, 0)
	if err != nil {
		if errors.Is(err, errStoreLocked) {
			if admission, readErr := readExecutionAdmission(path); readErr == nil {
				admission.RetryAtMS = time.Now().Add(time.Second).UnixMilli()
				cs.queueExecutionAdmission(admission)
			}
			return nil, nil, nil, ErrDuplicateExecution
		}
		return nil, nil, nil, err
	}
	admission, err := readExecutionAdmission(path)
	if errors.Is(err, os.ErrNotExist) {
		admission = executionAdmission{
			RunID: execution.RunID, StartedAt: execution.StartedAt, Trigger: execution.Trigger,
			ManualIdentity: execution.ManualIdentity, Job: cloneCronJob(*execution.Job), State: admissionPrepared,
		}
	} else if err != nil {
		return nil, nil, unlock, err
	} else if admission.Job.ID != execution.Job.ID || !admission.StartedAt.Equal(execution.StartedAt) || admission.Trigger != execution.Trigger || admission.ManualIdentity != execution.ManualIdentity {
		return nil, nil, unlock, errors.New("cron admission identity changed")
	} else if admission.State == admissionFinished {
		return nil, nil, unlock, ErrDuplicateExecution
	} else if admission.State == admissionExecuting {
		admission.Outcome = "indeterminate"
		return &admission, nil, unlock, errors.New("cron execution outcome unknown after runtime restart")
	} else {
		admission.Retried = true
	}
	if err := cs.writeExecutionAdmission(&admission); err != nil {
		return nil, nil, unlock, err
	}
	claim := func() error {
		current, exists := cs.GetJob(admission.Job.ID)
		if !exists || current.Payload != admission.Job.Payload ||
			!sameSchedule(current.Schedule, admission.Job.Schedule) ||
			(admission.Trigger == Scheduled && !current.Enabled) {
			return ErrExecutionSuperseded
		}
		if admission.State != admissionPrepared {
			return ErrDuplicateExecution
		}
		admission.State = admissionExecuting
		if err := cs.writeExecutionAdmission(&admission); err != nil {
			admission.State = admissionPrepared
			return err
		}
		return nil
	}
	return &admission, claim, unlock, nil
}

func (cs *CronService) finishExecutionAdmission(admission *executionAdmission) error {
	admission.State = admissionFinished
	return cs.writeExecutionAdmission(admission)
}

func (cs *CronService) deferExecutionAdmission(admission *executionAdmission, delay time.Duration) error {
	admission.Retried = true
	admission.RetryAtMS = time.Now().Add(delay).UnixMilli()
	if err := cs.writeExecutionAdmission(admission); err != nil {
		return err
	}
	cs.queueExecutionAdmission(*admission)
	return nil
}

func (cs *CronService) queueExecutionAdmission(admission executionAdmission) {
	cs.mu.Lock()
	if cs.admissionRetries == nil {
		cs.admissionRetries = make(map[string]executionAdmission)
	}
	cs.admissionRetries[admission.RunID] = admission
	cs.mu.Unlock()
	cs.notify()
}

// Called under the service lock at startup. A claimed effect is never replayed.
func (cs *CronService) loadExecutionAdmissions() error {
	cs.admissionRetries = make(map[string]executionAdmission)
	if !cs.durableAdmission {
		return nil
	}
	entries, err := os.ReadDir(cs.storePath + ".admissions")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") ||
			uuid.Validate(strings.TrimSuffix(entry.Name(), ".json")) != nil {
			continue
		}
		path := filepath.Join(cs.storePath+".admissions", entry.Name())
		admission, err := readExecutionAdmission(path)
		if err != nil {
			return fmt.Errorf("read pending cron admission: %w", err)
		}
		if admission.State == admissionFinished {
			if time.Since(admission.UpdatedAt) > admissionRetention {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if err := os.Remove(path + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			continue
		}
		if admission.State == admissionExecuting || admission.RetryAtMS == 0 {
			admission.RetryAtMS = time.Now().UnixMilli()
		}
		cs.admissionRetries[admission.RunID] = admission
	}
	return nil
}

func (cs *CronService) hasAdmissionRetry(jobID string) bool {
	for _, admission := range cs.admissionRetries {
		if admission.Job.ID == jobID {
			return true
		}
	}
	return false
}

func (cs *CronService) admissionJobEnabled(admission executionAdmission) bool {
	for _, job := range cs.store.Jobs {
		if job.ID == admission.Job.ID {
			return job.Enabled || admission.Trigger == Manual
		}
	}
	return false
}

func (cs *CronService) dueExecutionAdmissions(now int64) []executionAdmission {
	var due []executionAdmission
	for run, admission := range cs.admissionRetries {
		if admission.RetryAtMS <= now && cs.admissionJobEnabled(admission) {
			due = append(due, admission)
			delete(cs.admissionRetries, run)
		}
	}
	return due
}

func (cs *CronService) dispatchExecutionAdmissions(admissions []executionAdmission) {
	for _, admission := range admissions {
		go func() {
			cs.execSlots <- struct{}{}
			defer func() { <-cs.execSlots }()
			cs.executeJobWithAdmission(
				admission.Job.ID,
				admission.Trigger,
				admission.RunID,
				admission.StartedAt,
				admission.ManualIdentity,
			)
		}()
	}
}
