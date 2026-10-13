package cron

import "context"

type Trigger string

const (
	Scheduled Trigger = "scheduled"
	Manual    Trigger = "manual"
)

// Execution identifies one dispatch after admission and the idle check. Job IDs
// remain opaque; the UUID identifies this run even when a one-shot job is deleted.
type Execution struct {
	Progress func(stage string)
	RunID    string
	Trigger  Trigger
	Job      *CronJob
}

type ExecutionHandler func(context.Context, Execution) (string, error)

// SetOnExecution is additive. A nil handler restores the legacy JobHandler.
// Context cancellation is controlled by the embedder; Stop retains its existing
// scheduler-only semantics and does not claim that an in-flight run was canceled.
func (cs *CronService) SetOnExecution(ctx context.Context, handler ExecutionHandler) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.executionContext, cs.onExecution = ctx, handler
}
