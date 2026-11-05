package providers

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
)

type (
	historyRunKey  struct{}
	historyCallKey struct{}
)

// WithHistoryRun attaches persistence provenance without adding provider fields.
func WithHistoryRun(ctx context.Context, runID string) context.Context {
	if uuid.Validate(runID) != nil {
		return ctx
	}
	return context.WithValue(ctx, historyRunKey{}, runID)
}

func HistoryRun(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(historyRunKey{}).(string)
	return value
}

// WithHistoryCall carries the producer's request identity to its live stream.
func WithHistoryCall(ctx context.Context, callID string) context.Context {
	if !ValidHistoryCallID(callID) {
		callID = ""
	}
	return context.WithValue(ctx, historyCallKey{}, callID)
}

func HistoryCall(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(historyCallKey{}).(string)
	return value
}

func ValidHistoryCallID(value string) bool {
	if len(value) != 27 || !strings.HasPrefix(value, "pc-") {
		return false
	}
	_, err := hex.DecodeString(value[3:])
	return err == nil
}
