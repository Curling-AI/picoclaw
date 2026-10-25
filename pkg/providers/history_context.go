package providers

import (
	"context"

	"github.com/google/uuid"
)

type historyRunKey struct{}

// WithHistoryRun attaches persistence provenance without adding provider fields.
func WithHistoryRun(ctx context.Context, runID string) context.Context {
	if uuid.Validate(runID) != nil {
		return ctx
	}
	return context.WithValue(ctx, historyRunKey{}, runID)
}

func HistoryRun(ctx context.Context) string {
	value, _ := ctx.Value(historyRunKey{}).(string)
	return value
}
