package protocoltypes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync/atomic"
)

type turnIDContextKey struct{}

// WithTurnID keeps auxiliary model calls correlated with the turn that caused them.
func WithTurnID(ctx context.Context, turnID string) context.Context {
	return context.WithValue(ctx, turnIDContextKey{}, turnID)
}

func TurnIDFromContext(ctx context.Context) string {
	turnID, _ := ctx.Value(turnIDContextKey{}).(string)
	return turnID
}

type taskIDContextKey struct{}

type task struct {
	id     string
	labels []string
}

// WithTaskID names the task a model call belongs to: everything between a
// message and its answer, sub-turns included. The id sits in a holder of its
// own, so MoveTaskID on an outer context does not reach this one. labels are
// the task's own (task:, channel:), kept with the id so whatever inherits the
// task inherits them.
func WithTaskID(ctx context.Context, taskID string, labels ...string) context.Context {
	holder := &atomic.Pointer[task]{}
	holder.Store(&task{id: taskID, labels: slices.Clone(labels)})
	return context.WithValue(ctx, taskIDContextKey{}, holder)
}

// MoveTaskID starts a new task for the calls made from now on under ctx's
// holder: a message the person sends mid-turn is a new request.
func MoveTaskID(ctx context.Context, taskID string, labels ...string) {
	if holder, ok := ctx.Value(taskIDContextKey{}).(*atomic.Pointer[task]); ok {
		holder.Store(&task{id: taskID, labels: slices.Clone(labels)})
	}
}

func taskFromContext(ctx context.Context) task {
	holder, ok := ctx.Value(taskIDContextKey{}).(*atomic.Pointer[task])
	if !ok {
		return task{}
	}
	return *holder.Load()
}

func TaskIDFromContext(ctx context.Context) string {
	return taskFromContext(ctx).id
}

// TaskLabelsFromContext returns the labels of ctx's task, without a call's role.
func TaskLabelsFromContext(ctx context.Context) []string {
	return slices.Clone(taskFromContext(ctx).labels)
}

type taskRoleContextKey struct{}

// WithTaskRole says what the calls made under ctx do within their task.
func WithTaskRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, taskRoleContextKey{}, role)
}

// TaskCallLabels are the labels one call carries: its task's, plus its role.
func TaskCallLabels(ctx context.Context) []string {
	_, labels := TaskCallFromContext(ctx)
	return labels
}

// TaskCallFromContext reads ctx's task once, for a call: its id and the labels
// the call carries. Two reads could straddle a MoveTaskID and pair one task's
// id with another's labels.
func TaskCallFromContext(ctx context.Context) (string, []string) {
	t := taskFromContext(ctx)
	labels := slices.Clone(t.labels)
	if role, _ := ctx.Value(taskRoleContextKey{}).(string); role != "" {
		labels = append(labels, role)
	}
	return t.id, labels
}

// NewTraceID mints a 96-bit id with prefix for the gateway to trace by; "" when
// the system has no randomness to give.
func NewTraceID(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return prefix + hex.EncodeToString(buf[:])
}
