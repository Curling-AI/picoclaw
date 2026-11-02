package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

type boundedHistoryStore interface {
	ReadHistoryPage(ctx context.Context, request memory.HistoryPageRequest) (memory.HistoryPage, error)
}

// ReadHistoryPage is optional so legacy SessionStore implementations keep their
// existing contract and explicitly decline the new capture lane.
func (b *JSONLBackend) ReadHistoryPage(
	ctx context.Context,
	request memory.HistoryPageRequest,
) (memory.HistoryPage, error) {
	reader, ok := b.store.(boundedHistoryStore)
	if !ok {
		return memory.HistoryPage{}, memory.ErrHistoryUnsupported
	}
	request.Key = b.resolveSessionKey(request.Key)
	return reader.ReadHistoryPage(ctx, request)
}
