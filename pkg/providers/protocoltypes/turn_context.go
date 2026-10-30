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
