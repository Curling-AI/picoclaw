package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// HistoryContentReader optionally reads the requested canonical session with
// explicit context/I/O errors. It rejects aliases and returns no order evidence.
type HistoryContentReader interface {
	ReadHistoryContent(ctx context.Context, key string) ([]providers.Message, error)
}

func (b *JSONLBackend) ReadHistoryContent(ctx context.Context, key string) ([]providers.Message, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	store, ok := b.store.(HistoryContentReader)
	if !ok {
		return nil, memory.ErrHistoryUnsupported
	}
	return store.ReadHistoryContent(ctx, key)
}
