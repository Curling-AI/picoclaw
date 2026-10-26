package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// HistoryReceiptStore is optional. Unsupported stores cannot fabricate durable
// ordering evidence; the original SessionStore contract remains compatible.
type HistoryReceiptStore interface {
	AppendHistoryReceipt(
		ctx context.Context,
		key string,
		message providers.Message,
	) (memory.CommittedHistoryMessage, error)
	ReadHistoryWithReceipts(ctx context.Context, key string) ([]memory.CommittedHistoryMessage, error)
}

func (b *JSONLBackend) AppendHistoryReceipt(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.CommittedHistoryMessage, error) {
	store, ok := b.store.(HistoryReceiptStore)
	if !ok {
		return memory.CommittedHistoryMessage{}, memory.ErrHistoryUnsupported
	}
	// The receipt store resolves and verifies canonical ownership under its
	// commit lock; the legacy resolver's error fallback must not mint a proof.
	return store.AppendHistoryReceipt(ctx, key, message)
}

func (b *JSONLBackend) ReadHistoryWithReceipts(
	ctx context.Context,
	key string,
) ([]memory.CommittedHistoryMessage, error) {
	store, ok := b.store.(HistoryReceiptStore)
	if !ok {
		return nil, memory.ErrHistoryUnsupported
	}
	return store.ReadHistoryWithReceipts(ctx, key)
}
