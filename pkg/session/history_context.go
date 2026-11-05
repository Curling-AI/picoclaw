package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// AppendHistoryContext carries private run provenance through an optional
// context writer, preserving compatibility with other session backends.
func (b *JSONLBackend) AppendHistoryContext(ctx context.Context, key string, message providers.Message) error {
	return b.store.AddFullMessage(ctx, b.resolveSessionKey(key), message)
}

func (b *JSONLBackend) SetHistoryContext(ctx context.Context, key string, messages []providers.Message) error {
	return b.store.SetHistory(ctx, b.resolveSessionKey(key), messages)
}

// ClearHistoryContext carries explicit owner intent without discarding scope or aliases.
func (b *JSONLBackend) ClearHistoryContext(ctx context.Context, key string) error {
	if clearer, ok := b.store.(interface {
		ClearHistory(ctx context.Context, key string) error
	}); ok {
		return clearer.ClearHistory(ctx, b.resolveSessionKey(key))
	}
	return b.SetHistoryContext(ctx, key, nil)
}
