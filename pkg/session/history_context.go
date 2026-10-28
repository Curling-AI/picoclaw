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
