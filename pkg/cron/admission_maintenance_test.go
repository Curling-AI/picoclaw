package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func writeAdmissionFixture(t *testing.T, path string, admission executionAdmission) {
	t.Helper()
	data, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionMaintenancePrunesExpiredReceiptsWhileServiceRuns(t *testing.T) {
	service, job := admissionOneShot(t)
	service.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		t.Error("maintenance or duplicate admission dispatched effects")
		return "unexpected", nil
	})
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Stop)
	now := time.Now().UTC()
	old := now.Add(-admissionRetention - time.Hour)
	var recent, locked executionAdmission
	var release func()
	for _, kind := range []string{"expired", "recent", "prepared", "executing", "indeterminate", "locked"} {
		admission := executionAdmission{
			RunID: uuid.NewString(), StartedAt: now, Trigger: Scheduled,
			Job: *job, State: admissionFinished, UpdatedAt: old,
		}
		switch kind {
		case "recent":
			admission.UpdatedAt = now
			recent = admission
		case "prepared", "executing":
			admission.State = kind
			admission.RetryAtMS = now.Add(2 * time.Hour).UnixMilli()
		case "indeterminate":
			admission.Outcome = "indeterminate"
		}
		path := service.admissionPath(admission.RunID)
		writeAdmissionFixture(t, path, admission)
		unlock, err := lockStore(path, 0)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "locked" {
			locked, release = admission, unlock
			t.Cleanup(func() {
				if release != nil {
					release()
				}
			})
		} else {
			unlock()
		}
		lockInfo, err := os.Stat(path + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		if kind == "prepared" {
			service.queueExecutionAdmission(admission)
		}
		service.mu.Lock()
		service.nextAdmissionSweepMS = 0
		service.mu.Unlock()
		service.checkJobs()
		_, err = os.Lstat(path)
		if kind == "expired" {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("expired finished payload survived maintenance without restart", err)
			}
		} else if err != nil {
			t.Fatal("maintenance removed a recent, queued, active or uncertain admission", kind, err)
		}
		currentLock, err := os.Stat(path + ".lock")
		if err != nil || !os.SameFile(lockInfo, currentLock) {
			t.Fatal("maintenance replaced the shared lock inode", err)
		}
	}
	service.executeJobWithAdmission(job.ID, Scheduled, recent.RunID, recent.StartedAt, false)
	service.mu.RLock()
	pending := len(service.admissionRetries)
	nextSweep := service.nextAdmissionSweepMS
	service.mu.RUnlock()
	if pending != 1 || nextSweep <= time.Now().UnixMilli() {
		t.Fatal("maintenance lost pending work or failed to schedule its next sweep")
	}
	release()
	release = nil
	service.pruneExecutionAdmissions(time.Now())
	if _, err := os.Lstat(service.admissionPath(locked.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired payload was not reclaimed after its owner released the lock", err)
	}
}

func TestAdmissionMaintenanceWakesAnOtherwiseIdleDurableScheduler(t *testing.T) {
	service := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	service.SetOnExecutionWithAdmission(t.Context(), nil)
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Stop)
	service.mu.RLock()
	next := service.getNextWakeMS()
	sweep := service.nextAdmissionSweepMS
	service.mu.RUnlock()
	if next == nil || *next != sweep || sweep <= time.Now().UnixMilli() {
		t.Fatal("idle durable cron did not schedule admission maintenance")
	}
	service.SetOnExecution(t.Context(), nil)
	service.mu.RLock()
	next = service.getNextWakeMS()
	service.mu.RUnlock()
	if next != nil {
		t.Fatal("admission maintenance changed non-durable scheduling")
	}
}

func TestAdmissionRetentionPreservesTheFullReceiptWindow(t *testing.T) {
	now := time.Now().UTC()
	admission := executionAdmission{State: admissionFinished, UpdatedAt: now.Add(-admissionRetention)}
	if isExpiredFinishedAdmission(admission, now) {
		t.Fatal("receipt expired at the inclusive retention boundary")
	}
	if !isExpiredFinishedAdmission(admission, now.Add(time.Nanosecond)) {
		t.Fatal("receipt did not expire after its retention window")
	}
}

func TestAdmissionStoreFailureStillRefusesStartup(t *testing.T) {
	service, _ := admissionOneShot(t)
	if err := os.WriteFile(service.storePath+".admissions", []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.SetOnExecutionWithAdmission(t.Context(), nil)
	if err := service.Start(); err == nil {
		service.Stop()
		t.Fatal("cron started without being able to enumerate its admission store")
	}
}

func writeCorruptAdmissionFixture(t *testing.T, path, kind string, healthy executionAdmission) {
	t.Helper()
	var err error
	switch kind {
	case "truncated":
		err = os.WriteFile(path, []byte("{truncated"), 0o600)
	case "identity":
		invalid := healthy
		invalid.RunID = "invalid"
		writeAdmissionFixture(t, path, invalid)
	case "oversized":
		err = os.WriteFile(path, make([]byte, 1024*1024+1), 0o600)
	case "directory":
		err = os.Mkdir(path, 0o700)
	case "symlink":
		target := filepath.Join(t.TempDir(), "outside.json")
		writeAdmissionFixture(t, target, healthy)
		err = os.Symlink(target, path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestCorruptAdmissionDoesNotBlockHealthyRecoveryAtStart(t *testing.T) {
	for _, kind := range []string{"truncated", "identity", "oversized", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			service, job := admissionOneShot(t)
			healthy := executionAdmission{
				RunID: uuid.NewString(), StartedAt: time.Now().UTC(), Trigger: Scheduled,
				Job: *job, State: admissionPrepared, RetryAtMS: time.Now().Add(2 * time.Hour).UnixMilli(),
			}
			writeAdmissionFixture(t, service.admissionPath(healthy.RunID), healthy)
			badRun := uuid.NewString()
			badPath := service.admissionPath(badRun)
			writeCorruptAdmissionFixture(t, badPath, kind, healthy)
			restarted := NewCronService(service.storePath, nil)
			var effects int
			restarted.SetOnExecutionWithAdmission(
				t.Context(),
				func(_ context.Context, execution Execution) (string, error) {
					if err := execution.BeginEffects(); err != nil {
						return "", err
					}
					effects++
					return "done", nil
				},
			)
			if err := restarted.Start(); err != nil {
				t.Fatalf("one invalid journal stopped healthy cron work: %v", err)
			}
			t.Cleanup(restarted.Stop)
			restarted.mu.RLock()
			_, recovered := restarted.admissionRetries[healthy.RunID]
			_, invalidQueued := restarted.admissionRetries[badRun]
			restarted.mu.RUnlock()
			if !recovered || invalidQueued {
				t.Fatal("recovery lost healthy work or queued an invalid identity")
			}
			restarted.executeJobWithAdmission(job.ID, Scheduled, healthy.RunID, healthy.StartedAt, false)
			if effects != 1 {
				t.Fatal("healthy admission did not claim its effect exactly once")
			}
			_, _, unlock, err := restarted.beginExecutionAdmission(Execution{
				RunID: badRun, StartedAt: healthy.StartedAt, Trigger: Scheduled, Job: job,
			})
			if unlock != nil {
				unlock()
			}
			if err == nil {
				t.Fatal("invalid RunID was made available for fresh effects")
			}
			if _, err := os.Lstat(badPath); err != nil {
				t.Fatal("invalid journal was removed instead of remaining fail-closed", err)
			}
		})
	}
}
