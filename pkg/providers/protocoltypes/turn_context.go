package protocoltypes

import "context"

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
// message and its answer, sub-turns included.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, taskIDContextKey{}, taskID)
}

func TaskIDFromContext(ctx context.Context) string {
	taskID, _ := ctx.Value(taskIDContextKey{}).(string)
	return taskID
}
