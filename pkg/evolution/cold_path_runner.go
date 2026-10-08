package evolution

import (
	"context"
	"errors"
	"sync"
	"time"
)

type coldPathRuntime interface {
	RunColdPathOnce(ctx context.Context, workspace string) error
}

// ColdPathRunnerOptions tunes how often a workspace's cold path may run.
type ColdPathRunnerOptions struct {
	// OnError receives every failed run. Nil discards them.
	OnError func(error)
	// MinInterval is the least time between the starts of two runs of the same
	// workspace. Triggers that arrive sooner are coalesced into one run when the
	// interval ends. Zero runs back to back.
	MinInterval time.Duration
	// NoCreditPause refuses triggers for a workspace for this long after a run
	// fails for lack of credit. Zero disables the pause.
	NoCreditPause time.Duration
	// Now and After replace the wall clock in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

type ColdPathRunner struct {
	runtime       coldPathRuntime
	async         func(func())
	onError       func(error)
	minInterval   time.Duration
	noCreditPause time.Duration
	now           func() time.Time
	after         func(time.Duration) <-chan time.Time
	ctx           context.Context
	cancel        context.CancelFunc

	mu          sync.Mutex
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closed      bool
	running     map[string]workspaceRunState
	lastStart   map[string]time.Time
	pausedUntil map[string]time.Time
}

func NewColdPathRunner(runtime coldPathRuntime) *ColdPathRunner {
	return NewColdPathRunnerWithOptions(runtime, ColdPathRunnerOptions{})
}

func NewColdPathRunnerWithErrorHandler(runtime coldPathRuntime, onError func(error)) *ColdPathRunner {
	return NewColdPathRunnerWithOptions(runtime, ColdPathRunnerOptions{OnError: onError})
}

func NewColdPathRunnerWithOptions(runtime coldPathRuntime, opts ColdPathRunnerOptions) *ColdPathRunner {
	onError := opts.OnError
	if onError == nil {
		onError = func(error) {}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	after := opts.After
	if after == nil {
		after = time.After
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &ColdPathRunner{
		runtime: runtime,
		async: func(run func()) {
			go run()
		},
		onError:       onError,
		minInterval:   opts.MinInterval,
		noCreditPause: opts.NoCreditPause,
		now:           now,
		after:         after,
		ctx:           ctx,
		cancel:        cancel,
		running:       make(map[string]workspaceRunState),
		lastStart:     make(map[string]time.Time),
		pausedUntil:   make(map[string]time.Time),
	}
}

type workspaceRunState struct {
	running bool
	pending bool
}

func (r *ColdPathRunner) Trigger(workspace string) bool {
	if r == nil || r.runtime == nil || workspace == "" {
		return false
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	if until, paused := r.pausedUntil[workspace]; paused {
		if r.now().Before(until) {
			r.mu.Unlock()
			return false
		}
		delete(r.pausedUntil, workspace)
	}
	state, exists := r.running[workspace]
	if exists && state.running {
		state.pending = true
		r.running[workspace] = state
		r.mu.Unlock()
		return true
	}
	r.running[workspace] = workspaceRunState{running: true}
	r.wg.Add(1)
	r.mu.Unlock()

	r.async(func() {
		defer r.wg.Done()
		r.runWorkspace(workspace)
	})

	return true
}

func (r *ColdPathRunner) runWorkspace(workspace string) {
	for {
		if !r.waitForInterval(workspace) {
			r.mu.Lock()
			delete(r.running, workspace)
			r.mu.Unlock()
			return
		}
		// The run about to start sees every record that arrived during the wait,
		// so triggers from the wait are served by it. Only triggers that arrive
		// while it runs ask for another.
		r.mu.Lock()
		if state, ok := r.running[workspace]; ok {
			state.pending = false
			r.running[workspace] = state
		}
		r.lastStart[workspace] = r.now()
		r.mu.Unlock()

		err := r.runtime.RunColdPathOnce(r.ctx, workspace)
		if err != nil && isNoCreditError(err) && r.noCreditPause > 0 {
			// Pending work is dropped too: without credit it would fail the same way.
			r.mu.Lock()
			r.pausedUntil[workspace] = r.now().Add(r.noCreditPause)
			delete(r.running, workspace)
			r.mu.Unlock()
			r.onError(err)
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			r.onError(err)
		}

		r.mu.Lock()
		state, exists := r.running[workspace]
		if !exists || r.closed {
			delete(r.running, workspace)
			r.mu.Unlock()
			return
		}
		if state.pending {
			state.pending = false
			r.running[workspace] = state
			r.mu.Unlock()
			continue
		}
		delete(r.running, workspace)
		r.mu.Unlock()
		return
	}
}

// waitForInterval blocks until MinInterval has passed since the workspace's
// last run started. It reports false when the runner closes while waiting.
func (r *ColdPathRunner) waitForInterval(workspace string) bool {
	if r.minInterval <= 0 {
		return true
	}
	r.mu.Lock()
	last, ran := r.lastStart[workspace]
	r.mu.Unlock()
	if !ran {
		return true
	}
	wait := last.Add(r.minInterval).Sub(r.now())
	if wait <= 0 {
		return true
	}
	select {
	case <-r.after(wait):
		return true
	case <-r.ctx.Done():
		return false
	}
}

func (r *ColdPathRunner) Close() error {
	if r == nil {
		return nil
	}

	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.cancel()
	})
	r.wg.Wait()
	return nil
}
