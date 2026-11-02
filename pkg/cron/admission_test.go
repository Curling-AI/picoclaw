package cron

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func admissionOneShot(t *testing.T) (*CronService, *CronJob) {
	t.Helper()
	service := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	at := time.Now().Add(time.Hour).UnixMilli()
	job, err := service.AddJob("Deferred fixture", CronSchedule{Kind: "at", AtMS: &at}, "fixture", "cli", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return service, job
}

func TestAdmissionRetrySurvivesRestartAndKeepsTheOriginalOccurrence(t *testing.T) {
	service, job := admissionOneShot(t)
	run, at := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	service.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		if execution.RunID != run || !execution.StartedAt.Equal(at) || execution.ManualIdentity {
			t.Fatal("first admission changed the scheduled identity")
		}
		return "", RetryExecutionAfter(errors.New("temporary fixture outage"), time.Second)
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, at, false)
	if pending, ok := service.GetJob(
		job.ID,
	); !ok || pending.State.LastRunAtMS != nil ||
		pending.State.LastStatus != "" {
		t.Fatal("pending admission consumed the one-shot or recorded a false terminal")
	}
	restarted := NewCronService(service.storePath, nil)
	var effects atomic.Int32
	restarted.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		if execution.RunID != run || !execution.StartedAt.Equal(at) || execution.ManualIdentity || !execution.Deferred {
			t.Error("recovery invented another occurrence or manual binding")
		}
		if err := execution.BeginEffects(); err != nil {
			return "", err
		}
		effects.Add(1)
		return "done", nil
	})
	if err := restarted.Start(); err != nil {
		t.Fatal(err)
	}
	defer restarted.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, exists := restarted.GetJob(job.ID); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prepared admission did not recover")
		}
		time.Sleep(time.Millisecond)
	}
	if effects.Load() != 1 {
		t.Fatal("recovery duplicated or lost the effect")
	}
}

func TestClaimedAdmissionIsNeverExecutedAgainAfterRestart(t *testing.T) {
	service, job := admissionOneShot(t)
	run, at := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	execution := Execution{RunID: run, StartedAt: at, Trigger: Scheduled, Job: job}
	admission, claim, unlock, err := service.beginExecutionAdmission(execution)
	if err != nil {
		t.Fatal(err)
	}
	if err = claim(); err != nil {
		t.Fatal(err)
	}
	unlock()
	if admission.State != admissionExecuting {
		t.Fatal("effects claim was not durable")
	}
	restarted := NewCronService(service.storePath, nil)
	var calls atomic.Int32
	restarted.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		calls.Add(1)
		return "unexpected", nil
	})
	restarted.executeJobWithAdmission(job.ID, Scheduled, run, at, false)
	if calls.Load() != 0 {
		t.Fatal("an interrupted effects claim was executed again")
	}
	finished, err := readExecutionAdmission(service.admissionPath(run))
	if err != nil || finished.State != admissionFinished {
		t.Fatal("interrupted execution did not retain an unknown-outcome tombstone", err)
	}
}

func TestAdmissionCannotDeferAfterClaimingEffects(t *testing.T) {
	service, job := admissionOneShot(t)
	service.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		if err := execution.BeginEffects(); err != nil {
			return "", err
		}
		return "", RetryExecutionAfter(errors.New("fixture failure after effects"), time.Second)
	})
	service.executeJob(job.ID, Scheduled)
	if _, exists := service.GetJob(job.ID); exists || len(service.admissionRetries) != 0 {
		t.Fatal("effects-claimed work was queued for automatic replay")
	}
}

func TestPreparedAdmissionRespectsPauseAndRejectsEditedPayload(t *testing.T) {
	service, job := admissionOneShot(t)
	run, at := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	service.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		return "", RetryExecutionAfter(errors.New("fixture outage"), time.Second)
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, at, false)
	if _, err := service.EnableJob(job.ID, false); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	due := service.dueExecutionAdmissions(time.Now().Add(time.Hour).UnixMilli())
	service.mu.Unlock()
	if len(due) != 0 {
		t.Fatal("paused scheduled work was recovered")
	}
	if _, err := service.EnableJob(job.ID, true); err != nil {
		t.Fatal(err)
	}
	edited, _ := service.GetJob(job.ID)
	edited.Payload.Message = "different intent"
	if err := service.UpdateJob(edited); err != nil {
		t.Fatal(err)
	}
	var effects int
	service.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		if err := execution.BeginEffects(); err != nil {
			return "", err
		}
		effects++
		return "done", nil
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, at, false)
	if effects != 0 {
		t.Fatal("an edited payload inherited an older admission")
	}
}
