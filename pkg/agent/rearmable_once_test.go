package agent

import (
	"sync"
	"testing"
	"time"
)

func TestRearmableOnce_RunsOnceUntilReset(t *testing.T) {
	var o rearmableOnce
	runs := 0
	o.Do(func() { runs++ })
	o.Do(func() { runs++ })
	if runs != 1 {
		t.Fatalf("runs = %d, want 1 before reset", runs)
	}
	o.Reset(nil)
	o.Do(func() { runs++ })
	if runs != 2 {
		t.Fatalf("runs = %d, want 2 after reset", runs)
	}
}

// Regression: a reload (Reset) arriving while another goroutine is inside Do
// used to zero the lock Do was holding and crash the process with
// "sync: unlock of unlocked mutex". Reset must wait for the running init.
func TestRearmableOnce_ResetWaitsForInFlightDo(t *testing.T) {
	var o rearmableOnce
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		o.Do(func() {
			close(started)
			<-release
		})
	}()
	<-started

	resetDone := make(chan struct{})
	cleared := false
	go func() {
		o.Reset(func() { cleared = true })
		close(resetDone)
	}()
	select {
	case <-resetDone:
		t.Fatal("Reset returned while an init was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	wg.Wait()
	<-resetDone
	if !cleared {
		t.Fatal("clear callback did not run")
	}

	reran := false
	o.Do(func() { reran = true })
	if !reran {
		t.Fatal("Do did not run again after Reset")
	}
}
