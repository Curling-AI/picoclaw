package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// HistoryContentLineage is negotiated explicitly by the internal consumer.
// Merely implementing this optional interface does not activate it for old callers.
type HistoryContentLineage = memory.HistoryContentLineage

func (b *JSONLBackend) HistoryContentIdentityActive(ctx context.Context, key string) (bool, error) {
	if ctx != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	store, ok := b.store.(memory.HistoryContentActivation)
	if !ok {
		return false, memory.ErrHistoryUnsupported
	}
	return store.HistoryContentIdentityActive(ctx, key)
}

func (b *JSONLBackend) ReadHistoryContentFrame(ctx context.Context, key string) (memory.ContentFrame, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentFrame{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ContentFrame{}, memory.ErrHistoryUnsupported
	}
	return store.ReadHistoryContentFrame(ctx, key)
}

func (b *JSONLBackend) InitializeHistoryContentIdentity(
	ctx context.Context,
	key, observation string,
) (memory.ContentFrame, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentFrame{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ContentFrame{}, memory.ErrHistoryUnsupported
	}
	return store.InitializeHistoryContentIdentity(ctx, key, observation)
}

func (b *JSONLBackend) CommitHistoryCompaction(
	ctx context.Context,
	key string,
	plan memory.ContentCompactionPlan,
) (memory.ContentTransition, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentTransition{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ContentTransition{}, memory.ErrHistoryUnsupported
	}
	return store.CommitHistoryCompaction(ctx, key, plan)
}

func (b *JSONLBackend) ReadPendingHistoryCompaction(ctx context.Context, key string) (memory.ContentTransition, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentTransition{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ContentTransition{}, memory.ErrHistoryUnsupported
	}
	return store.ReadPendingHistoryCompaction(ctx, key)
}

func (b *JSONLBackend) ReadHistoryCompactionFrame(
	ctx context.Context,
	key, mutation string,
	side memory.ContentTransitionSide,
) (memory.ContentFrame, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentFrame{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ContentFrame{}, memory.ErrHistoryUnsupported
	}
	return store.ReadHistoryCompactionFrame(ctx, key, mutation, side)
}

func (b *JSONLBackend) AcknowledgeHistoryCompaction(
	ctx context.Context,
	key string,
	ack memory.ContentArchiveAck,
) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.AcknowledgeHistoryCompaction(ctx, key, ack)
}

func (b *JSONLBackend) CommitHistoryContentEdit(
	ctx context.Context,
	key string,
	plan memory.ContentEditPlan,
) (memory.ContentFrame, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentFrame{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentLineage)
	if !ok {
		return memory.ContentFrame{}, memory.ErrHistoryUnsupported
	}
	return store.CommitHistoryContentEdit(ctx, key, plan)
}

func (b *JSONLBackend) AppendHistoryContent(
	ctx context.Context,
	key string,
	message providers.Message,
) (memory.ContentAppendResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentAppendResult{}, ctx.Err()
	}
	store, ok := b.store.(memory.HistoryContentAppender)
	if !ok {
		return memory.ContentAppendResult{}, memory.ErrHistoryUnsupported
	}
	return store.AppendHistoryContent(ctx, key, message)
}
