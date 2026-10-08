package evolution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingColdPathRuntime struct {
	runCount    atomic.Int32
	cancelCount atomic.Int32
	started     chan string
	release     chan struct{}
}

func (r *blockingColdPathRuntime) RunColdPathOnce(ctx context.Context, workspace string) error {
	r.runCount.Add(1)
	r.started <- workspace
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		r.cancelCount.Add(1)
		return ctx.Err()
	}
}

func TestColdPathRunner_QueuesPendingRunForWorkspace(t *testing.T) {
	runtime := &blockingColdPathRuntime{
		started: make(chan string, 4),
		release: make(chan struct{}, 4),
	}
	runner := NewColdPathRunner(runtime)
	defer runner.Close()

	if scheduled := runner.Trigger("workspace-a"); !scheduled {
		t.Fatal("expected first trigger to be scheduled")
	}

	select {
	case workspace := <-runtime.started:
		if workspace != "workspace-a" {
			t.Fatalf("workspace = %q, want workspace-a", workspace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first cold path run")
	}

	if scheduled := runner.Trigger("workspace-a"); !scheduled {
		t.Fatal("expected second trigger to queue a pending run")
	}

	select {
	case workspace := <-runtime.started:
		t.Fatalf("unexpected early pending cold path run for %q", workspace)
	case <-time.After(150 * time.Millisecond):
	}

	runtime.release <- struct{}{}

	select {
	case workspace := <-runtime.started:
		if workspace != "workspace-a" {
			t.Fatalf("workspace = %q, want workspace-a", workspace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending cold path run")
	}

	runtime.release <- struct{}{}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.runCount.Load() == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("runCount = %d, want 2", runtime.runCount.Load())
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type noCreditColdPathRuntime struct {
	runCount atomic.Int32
}

func (r *noCreditColdPathRuntime) RunColdPathOnce(context.Context, string) error {
	r.runCount.Add(1)
	return fmt.Errorf("cold path: %w", ErrNoCredit)
}

func waitRunnerIdle(t *testing.T, runner *ColdPathRunner, workspace string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		_, busy := runner.running[workspace]
		runner.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("runner still busy with %q", workspace)
}

func TestColdPathRunner_WaitsMinIntervalBeforeTheNextRun(t *testing.T) {
	runtime := &blockingColdPathRuntime{
		started: make(chan string, 4),
		release: make(chan struct{}, 4),
	}
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	waits := make(chan time.Duration, 4)
	elapsed := make(chan time.Time)
	runner := NewColdPathRunnerWithOptions(runtime, ColdPathRunnerOptions{
		MinInterval: 30 * time.Minute,
		Now:         clock.Now,
		After: func(d time.Duration) <-chan time.Time {
			waits <- d
			return elapsed
		},
	})
	defer runner.Close()

	runner.Trigger("workspace-a")
	select {
	case <-runtime.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first run should start at once: the workspace never ran")
	}

	clock.Advance(5 * time.Minute)
	runner.Trigger("workspace-a")
	runtime.release <- struct{}{}

	select {
	case d := <-waits:
		if d != 25*time.Minute {
			t.Errorf("waited %v, want the 25m left of the 30m interval", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending run did not wait for the interval")
	}
	select {
	case <-runtime.started:
		t.Fatal("pending run started before the interval elapsed")
	case <-time.After(100 * time.Millisecond):
	}

	clock.Advance(25 * time.Minute)
	elapsed <- clock.Now()
	select {
	case <-runtime.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pending run did not start after the interval")
	}
	runtime.release <- struct{}{}
	waitRunnerIdle(t, runner, "workspace-a")
	if got := runtime.runCount.Load(); got != 2 {
		t.Fatalf("runCount = %d, want 2", got)
	}
}

// Turns that end while the runner waits out the interval append their records
// before triggering, so the run after the wait covers them; asking for one more
// run would re-cluster the same window with nothing new.
func TestColdPathRunner_TriggersDuringTheWaitShareTheNextRun(t *testing.T) {
	runtime := &blockingColdPathRuntime{
		started: make(chan string, 4),
		release: make(chan struct{}, 4),
	}
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	waits := make(chan time.Duration, 4)
	elapsed := make(chan time.Time)
	runner := NewColdPathRunnerWithOptions(runtime, ColdPathRunnerOptions{
		MinInterval: 30 * time.Minute,
		Now:         clock.Now,
		After: func(d time.Duration) <-chan time.Time {
			waits <- d
			return elapsed
		},
	})
	defer runner.Close()

	runner.Trigger("workspace-a")
	<-runtime.started
	runner.Trigger("workspace-a")
	runtime.release <- struct{}{}
	select {
	case <-waits:
	case <-time.After(2 * time.Second):
		t.Fatal("pending run did not wait for the interval")
	}

	runner.Trigger("workspace-a")
	runner.Trigger("workspace-a")
	clock.Advance(30 * time.Minute)
	elapsed <- clock.Now()
	select {
	case <-runtime.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run after the wait did not start")
	}
	runtime.release <- struct{}{}
	waitRunnerIdle(t, runner, "workspace-a")

	if got := runtime.runCount.Load(); got != 2 {
		t.Fatalf("runCount = %d, want 2", got)
	}
	if extra := len(waits); extra != 0 {
		t.Fatalf("runner waited %d more times for a run nobody needs", extra)
	}
}

func TestColdPathRunner_PausesWorkspaceAfterNoCredit(t *testing.T) {
	runtime := &noCreditColdPathRuntime{}
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	reported := make(chan error, 4)
	runner := NewColdPathRunnerWithOptions(runtime, ColdPathRunnerOptions{
		NoCreditPause: 30 * time.Minute,
		Now:           clock.Now,
		OnError:       func(err error) { reported <- err },
	})
	defer runner.Close()

	if !runner.Trigger("workspace-a") {
		t.Fatal("first trigger should run")
	}
	select {
	case err := <-reported:
		if !errors.Is(err, ErrNoCredit) {
			t.Fatalf("reported %v, want ErrNoCredit", err)
		}
		// A pod runs several workspaces: the log must say which one paused and
		// until when.
		until := clock.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
		if msg := err.Error(); !strings.Contains(msg, "workspace-a") || !strings.Contains(msg, until) {
			t.Errorf("reported %q, want the workspace and the pause deadline %s", msg, until)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no-credit error was not reported")
	}
	waitRunnerIdle(t, runner, "workspace-a")

	clock.Advance(29 * time.Minute)
	if runner.Trigger("workspace-a") {
		t.Error("trigger during the pause should be refused")
	}
	if !runner.Trigger("workspace-b") {
		t.Error("the pause is per workspace")
	}
	waitRunnerIdle(t, runner, "workspace-b")

	clock.Advance(2 * time.Minute)
	if !runner.Trigger("workspace-a") {
		t.Error("trigger after the pause should run again")
	}
	waitRunnerIdle(t, runner, "workspace-a")
	if got := runtime.runCount.Load(); got != 3 {
		t.Fatalf("runCount = %d, want 3 (a, b, a after the pause)", got)
	}
}

func TestColdPathRunner_CloseCancelsActiveRunAndDropsPendingWork(t *testing.T) {
	runtime := &blockingColdPathRuntime{
		started: make(chan string, 4),
		release: make(chan struct{}, 4),
	}
	runner := NewColdPathRunner(runtime)

	if scheduled := runner.Trigger("workspace-a"); !scheduled {
		t.Fatal("expected first trigger to be scheduled")
	}

	select {
	case <-runtime.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first cold path run")
	}

	if scheduled := runner.Trigger("workspace-a"); !scheduled {
		t.Fatal("expected second trigger to mark pending work")
	}

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		if err := runner.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !runner.Trigger("workspace-a") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runner.Trigger("workspace-a") {
		t.Fatal("expected Trigger to reject new work after Close")
	}

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Close to finish")
	}

	select {
	case workspace := <-runtime.started:
		t.Fatalf("unexpected pending cold path run after Close for %q", workspace)
	case <-time.After(150 * time.Millisecond):
	}

	if got := runtime.runCount.Load(); got != 1 {
		t.Fatalf("runCount = %d, want 1", got)
	}
	if got := runtime.cancelCount.Load(); got != 1 {
		t.Fatalf("cancelCount = %d, want 1", got)
	}
}
