// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import "sync"

// rearmableOnce is a sync.Once that a config reload can re-arm.
//
// A sync.Once cannot be reused: overwriting it with a zero value also wipes
// the mutex Do holds for as long as the callback runs. The MCP init dials
// every server inside that callback, so a reload landing during a slow
// connect zeroed a held lock, and the deferred unlock then killed the process
// with "sync: unlock of unlocked mutex" (no recover catches a fatal). Here one
// lock covers both Do and Reset, so a reset WAITS for the in-flight init and
// re-arms afterwards instead of pulling the lock from under it.
type rearmableOnce struct {
	mu   sync.Mutex
	done bool
}

// Do runs f unless it already ran since the last Reset. Concurrent callers
// block until the running f returns, exactly like sync.Once.
func (o *rearmableOnce) Do(f func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	f()
	o.done = true
}

// Reset re-arms Do. clearState runs under the same lock, so the state it clears
// cannot be rebuilt by an init that slipped in between the re-arm and the
// clear. It may be nil.
func (o *rearmableOnce) Reset(clearState func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done = false
	if clearState != nil {
		clearState()
	}
}
