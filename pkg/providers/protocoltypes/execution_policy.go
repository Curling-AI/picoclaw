package protocoltypes

import "context"

type restrictedExecutionKey struct{}

// WithRestrictedExecution lets providers reject internal tools outside the tool registry.
func WithRestrictedExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, restrictedExecutionKey{}, true)
}

func RestrictedExecution(ctx context.Context) bool {
	restricted, _ := ctx.Value(restrictedExecutionKey{}).(bool)
	return restricted
}
