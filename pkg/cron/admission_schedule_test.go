package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFinishedAdmissionRetryPreservesRecurringSchedule(t *testing.T) {
	winner := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	every := time.Hour.Milliseconds()
	job, err := winner.AddJob(
		"Recurring fixture", CronSchedule{Kind: "every", EveryMS: &every}, "fixture", "cli", "fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	other, err := winner.AddJob(
		"Another recurring fixture", CronSchedule{Kind: "every", EveryMS: &every}, "other fixture", "cli", "fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the persisted claim that checkJobs makes before dispatching a due job.
	job.State.NextRunAtMS = nil
	if err := winner.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	run, started := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	winner.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		return "", RetryExecutionAfter(errors.New("fixture outage"), time.Second)
	})
	winner.executeJobWithAdmission(job.ID, Scheduled, run, started, false)

	loser := NewCronService(winner.storePath, nil)
	effects := 0
	complete := func(_ context.Context, execution Execution) (string, error) {
		if err := execution.BeginEffects(); err != nil {
			return "", err
		}
		effects++
		return "done", nil
	}
	loser.SetOnExecutionWithAdmission(t.Context(), complete)
	if err := loser.loadExecutionAdmissions(); err != nil {
		t.Fatal(err)
	}
	winner.SetOnExecutionWithAdmission(t.Context(), complete)
	winner.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	completed, exists := winner.GetJob(job.ID)
	if !exists || completed.State.NextRunAtMS == nil || completed.State.LastStatus != "ok" {
		t.Fatal("winning owner did not persist a successful execution and its next run")
	}

	// The other owner still has the earlier checkpoint. Run a pass while its
	// retry is pending, then drain the retry after the winner has finished it.
	loser.running = true
	pending, exists := loser.admissionRetries[run]
	if !exists {
		t.Fatal("losing owner did not recover the pending admission")
	}
	pending.RetryAtMS = time.Now().Add(time.Hour).UnixMilli()
	loser.admissionRetries[run] = pending
	loser.checkJobs()
	stored, exists := NewCronService(winner.storePath, nil).GetJob(job.ID)
	if !exists || !reflect.DeepEqual(stored.State, completed.State) {
		t.Error("a retry-only scheduler pass overwrote the winner's persisted result")
	}

	// Another job becomes due while this owner still holds A's stale retry.
	dueAt := time.Now().Add(-time.Second).UnixMilli()
	other.State.NextRunAtMS = &dueAt
	if err := winner.UpdateJob(other); err != nil {
		t.Fatal(err)
	}
	for i := range loser.store.Jobs {
		if loser.store.Jobs[i].ID == other.ID {
			loser.store.Jobs[i].State.NextRunAtMS = &dueAt
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	allowCompletion := sync.OnceFunc(func() { close(release) })
	defer allowCompletion()
	loser.SetOnExecutionWithAdmission(t.Context(), func(ctx context.Context, execution Execution) (string, error) {
		if execution.Job.ID != other.ID {
			return complete(ctx, execution)
		}
		if err := execution.BeginEffects(); err != nil {
			return "", err
		}
		close(entered)
		<-release
		return "other done", nil
	})
	loser.checkJobs()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the unrelated due job did not begin")
	}
	mixedCheckpoint := NewCronService(winner.storePath, nil)
	if stored, ok := mixedCheckpoint.GetJob(job.ID); !ok || !reflect.DeepEqual(stored.State, completed.State) {
		t.Error("claiming an unrelated due job overwrote the winner's persisted result")
	}
	if claimed, ok := mixedCheckpoint.GetJob(other.ID); !ok || claimed.State.NextRunAtMS != nil {
		t.Error("the unrelated job was dispatched before its claim was persisted")
	}
	allowCompletion()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if current, ok := loser.GetJob(other.ID); ok && current.State.LastStatus == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the unrelated due job did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	if stored, ok := NewCronService(winner.storePath, nil).GetJob(job.ID); !ok ||
		!reflect.DeepEqual(stored.State, completed.State) {
		t.Error("completing an unrelated job overwrote the winner's persisted result")
	}

	if due := loser.dueExecutionAdmissions(pending.RetryAtMS); len(due) != 1 {
		t.Fatalf("expected one stale retry, got %d", len(due))
	}
	loser.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	if current, exists := loser.GetJob(job.ID); !exists || !reflect.DeepEqual(current.State, completed.State) {
		t.Error("finished admission did not restore the winner's execution state and next run")
	}
	if effects != 1 {
		t.Errorf("the same occurrence ran effects %d times, want 1", effects)
	}
}

func TestSupersededAdmissionPreservesEditedOneShot(t *testing.T) {
	service, job := admissionOneShot(t)
	run, started := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	service.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		return "", RetryExecutionAfter(errors.New("fixture outage"), time.Second)
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	edited, exists := service.GetJob(job.ID)
	if !exists {
		t.Fatal("pending admission consumed the one-shot")
	}
	at := time.Now().Add(2 * time.Hour).UnixMilli()
	edited.Schedule.AtMS = &at
	edited.Payload.Message = "edited fixture"
	if err := service.UpdateJob(edited); err != nil {
		t.Fatal(err)
	}
	var claimErr error
	service.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		claimErr = execution.BeginEffects()
		return "", claimErr
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	if !errors.Is(claimErr, ErrExecutionSuperseded) {
		t.Fatalf("claim error = %v, want ErrExecutionSuperseded", claimErr)
	}
	persisted, exists := NewCronService(service.storePath, nil).GetJob(job.ID)
	if !exists {
		t.Fatal("superseded admission deleted the edited one-shot without executing it")
	}
	if persisted.State.NextRunAtMS == nil || *persisted.State.NextRunAtMS != at ||
		persisted.State.LastRunAtMS != nil || persisted.State.LastStatus != "" || !persisted.Enabled {
		t.Errorf("superseded admission changed the edited one-shot schedule or execution state: %+v", persisted.State)
	}
}

func TestPreparedAdmissionRejectsChangedConnectors(t *testing.T) {
	service, job := admissionOneShot(t)
	run, started := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	service.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		return "", RetryExecutionAfter(errors.New("fixture outage"), time.Second)
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	edited, exists := service.GetJob(job.ID)
	if !exists {
		t.Fatal("pending admission consumed the one-shot")
	}
	edited.Payload.Connectors = []string{"github"}
	if err := service.UpdateJob(edited); err != nil {
		t.Fatal(err)
	}
	var claimErr error
	service.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		claimErr = execution.BeginEffects()
		return "", claimErr
	})
	service.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	if !errors.Is(claimErr, ErrExecutionSuperseded) {
		t.Fatalf("claim error = %v, want ErrExecutionSuperseded", claimErr)
	}
	persisted, exists := NewCronService(service.storePath, nil).GetJob(job.ID)
	if !exists || !reflect.DeepEqual(persisted.Payload.Connectors, edited.Payload.Connectors) {
		t.Fatal("superseded admission did not preserve the saved connector edit")
	}
}

func TestFinishedAdmissionRetryKeepsJobWhileStoreIsUnavailable(t *testing.T) {
	winner, job := admissionOneShot(t)
	loser := NewCronService(winner.storePath, nil)
	run, started := uuid.NewString(), time.Now().UTC().Truncate(time.Microsecond)
	winner.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		return "done", execution.BeginEffects()
	})
	winner.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	loser.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		t.Error("finished admission ran effects again")
		return "", nil
	})
	if err := os.Rename(winner.storePath, winner.storePath+".offline"); err != nil {
		t.Fatal(err)
	}
	loser.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	if _, exists := loser.GetJob(job.ID); !exists {
		t.Error("unavailable store was mistaken for a deleted job")
	}
	if err := os.Rename(winner.storePath+".offline", winner.storePath); err != nil {
		t.Fatal(err)
	}
	if due := loser.dueExecutionAdmissions(time.Now().Add(time.Minute).UnixMilli()); len(due) != 1 {
		t.Fatalf("expected one reconciliation retry, got %d", len(due))
	}
	loser.executeJobWithAdmission(job.ID, Scheduled, run, started, false)
	if _, exists := loser.GetJob(job.ID); exists {
		t.Error("the loser kept a deleted one-shot after the store came back")
	}
}

func TestFinishedAdmissionWithoutStoredResultRestoresOnlyRecurringSchedule(t *testing.T) {
	service := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	service.SetOnExecutionWithAdmission(t.Context(), func(context.Context, Execution) (string, error) {
		return "", nil
	})
	every := time.Hour.Milliseconds()
	job, err := service.AddJob(
		"Recurring fixture", CronSchedule{Kind: "every", EveryMS: &every}, "fixture", "cli", "fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	job.State.NextRunAtMS = nil
	if updateErr := service.UpdateJob(job); updateErr != nil {
		t.Fatal(updateErr)
	}
	admission := executionAdmission{Job: *job, StartedAt: time.Now()}
	if restoreErr := service.restoreCompletedJob(admission); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	current, exists := service.GetJob(job.ID)
	if !exists || current.State.NextRunAtMS == nil ||
		current.State.LastRunAtMS != nil || current.State.LastStatus != "" {
		t.Fatal("missing result was recorded as an execution or left the recurring job unscheduled")
	}
	if stored, exists := NewCronService(service.storePath, nil).GetJob(job.ID); !exists ||
		!reflect.DeepEqual(stored.State, current.State) {
		t.Fatal("the recovered schedule was not stored, or its execution history changed")
	}
	service.mu.Lock()
	claimed, err := service.claimDueJobsUnsafe(*current.State.NextRunAtMS)
	service.mu.Unlock()
	if err != nil || !reflect.DeepEqual(claimed, []string{job.ID}) {
		t.Fatalf("the recovered occurrence was not claimable: %v, %v", claimed, err)
	}
}

func TestDurableSchedulersCannotBothClaimTheSameOccurrence(t *testing.T) {
	first := NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	every := time.Hour.Milliseconds()
	job, err := first.AddJob(
		"Shared recurring fixture", CronSchedule{Kind: "every", EveryMS: &every}, "fixture", "cli", "fixture",
	)
	if err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().Add(-time.Second).UnixMilli()
	job.State.NextRunAtMS = &dueAt
	if updateErr := first.UpdateJob(job); updateErr != nil {
		t.Fatal(updateErr)
	}
	second := NewCronService(first.storePath, nil)
	effects := 0
	for _, owner := range []*CronService{first, second} {
		owner.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
			if claimErr := execution.BeginEffects(); claimErr != nil {
				return "", claimErr
			}
			effects++
			return "done", nil
		})
	}
	for i, owner := range []*CronService{first, second} {
		owner.mu.Lock()
		claimed, claimErr := owner.claimDueJobsUnsafe(dueAt)
		owner.mu.Unlock()
		if i == 0 && (claimErr != nil || !reflect.DeepEqual(claimed, []string{job.ID})) {
			t.Fatalf("first owner claimed %v with %v, want only %s", claimed, claimErr, job.ID)
		}
		if i == 1 && (len(claimed) != 0 || !errors.Is(claimErr, errScheduledClaimInProgress)) {
			t.Fatalf("second owner claimed %v with %v, want no claim and a pending owner", claimed, claimErr)
		}
	}
	first.executeJob(job.ID, Scheduled)
	second.mu.Lock()
	claimed, err := second.claimDueJobsUnsafe(dueAt)
	second.mu.Unlock()
	if err != nil || len(claimed) != 0 || effects != 1 {
		t.Fatalf("handoff claimed %v with %v after %d effects, want no further claim", claimed, err, effects)
	}
	completed, _ := first.GetJob(job.ID)
	if current, ok := second.GetJob(job.ID); !ok || completed == nil ||
		!reflect.DeepEqual(current.State, completed.State) {
		t.Fatal("the losing owner did not adopt the winner's persisted next run")
	}
}

func TestDurableExecutionCompletionPreservesConcurrentJobEdit(t *testing.T) {
	service, job := admissionOneShot(t)
	editor := NewCronService(service.storePath, nil)
	at := time.Now().Add(2 * time.Hour).UnixMilli()
	service.SetOnExecutionWithAdmission(t.Context(), func(_ context.Context, execution Execution) (string, error) {
		if err := execution.BeginEffects(); err != nil {
			return "", err
		}
		edited, exists := editor.GetJob(job.ID)
		if !exists {
			t.Fatal("one-shot disappeared before its edit")
		}
		edited.Schedule.AtMS = &at
		edited.Payload.Message = "new intent"
		if err := editor.UpdateJob(edited); err != nil {
			t.Fatal(err)
		}
		return "old effect done", nil
	})
	service.executeJob(job.ID, Scheduled)
	for _, owner := range []*CronService{service, NewCronService(service.storePath, nil)} {
		current, ok := owner.GetJob(job.ID)
		if !ok || current.Payload.Message != "new intent" || current.State.NextRunAtMS == nil ||
			*current.State.NextRunAtMS != at || current.State.LastRunAtMS != nil || current.State.LastStatus != "" {
			t.Fatal("the old completion consumed or recorded an execution for the newly edited job")
		}
	}
}
