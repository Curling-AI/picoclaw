package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

func (b *JSONLBackend) ReadPublicHistoryPage(
	ctx context.Context,
	request memory.PublicHistoryPageRequest,
) (memory.PublicHistoryPage, error) {
	store, ok := b.store.(interface {
		ReadPublicHistoryPage(ctx context.Context, request memory.PublicHistoryPageRequest) (memory.PublicHistoryPage, error)
	})
	if !ok {
		return memory.PublicHistoryPage{}, memory.ErrHistoryUnsupported
	}
	key, err := b.resolveSessionKeyContext(ctx, request.Key)
	if err != nil {
		return memory.PublicHistoryPage{}, err
	}
	request.Key = key
	return store.ReadPublicHistoryPage(ctx, request)
}
