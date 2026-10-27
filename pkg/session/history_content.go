package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// HistoryContentReader is optional and returns ordinary history with explicit
// ownership, context and I/O errors, without returning private order evidence.
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
