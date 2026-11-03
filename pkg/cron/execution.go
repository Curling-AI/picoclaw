package cron

import (
	"context"
	"errors"
	"time"
)

type Trigger string

const (
	Scheduled Trigger = "scheduled"
	Manual    Trigger = "manual"
)

var (
	ErrDuplicateExecution  = errors.New("duplicate cron execution")
	ErrExecutionSuperseded = errors.New("cron job changed before execution effects")
)

// Execution identifies one dispatch after admission and the idle check. Job IDs
// remain opaque; the UUID identifies this run even when a one-shot job is deleted.
type Execution struct {
	Progress       func(stage string)
	RunID          string
	StartedAt      time.Time
	ManualIdentity bool
	Trigger        Trigger
	Job            *CronJob
	// BeginEffects durably claims effects for an admission-aware embedder.
	BeginEffects func() error
	Deferred     bool
}

type ExecutionHandler func(context.Context, Execution) (string, error)

// SetOnExecution is additive. A nil handler restores the legacy JobHandler.
// Context cancellation is controlled by the embedder; Stop retains its existing
// scheduler-only semantics and does not claim that an in-flight run was canceled.
func (cs *CronService) SetOnExecution(ctx context.Context, handler ExecutionHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.executionContext, cs.onExecution = ctx, handler
	cs.durableAdmission = false
}

// SetOnExecutionWithAdmission opts into durable admission retries. The handler
// must call BeginEffects before executing work, and may defer only before it.
func (cs *CronService) SetOnExecutionWithAdmission(ctx context.Context, handler ExecutionHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.executionContext, cs.onExecution, cs.durableAdmission = ctx, handler, true
}
