package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

func (b *JSONLBackend) ReadPublicHistoryPage(ctx context.Context, request memory.PublicHistoryPageRequest) (memory.PublicHistoryPage, error) {
	store, ok := b.store.(interface {
		ReadPublicHistoryPage(context.Context, memory.PublicHistoryPageRequest) (memory.PublicHistoryPage, error)
	})
	if !ok {
		return memory.PublicHistoryPage{}, memory.ErrHistoryUnsupported
	}
	return store.ReadPublicHistoryPage(ctx, request)
}
