package protocoltypes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// WithTaskID names the task a model call belongs to: everything between a
// message and its answer, sub-turns included. The id sits in a holder of its
// own, so MoveTaskID on an outer context does not reach this one.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	holder := &atomic.Pointer[string]{}
	holder.Store(&taskID)
	return context.WithValue(ctx, taskIDContextKey{}, holder)
}

// MoveTaskID starts a new task for the calls made from now on under ctx's
// holder: a message the person sends mid-turn is a new request.
func MoveTaskID(ctx context.Context, taskID string) {
	if holder, ok := ctx.Value(taskIDContextKey{}).(*atomic.Pointer[string]); ok {
		holder.Store(&taskID)
	}
}

func TaskIDFromContext(ctx context.Context) string {
	holder, ok := ctx.Value(taskIDContextKey{}).(*atomic.Pointer[string])
	if !ok {
		return ""
	}
	return *holder.Load()
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
