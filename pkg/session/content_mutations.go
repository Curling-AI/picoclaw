package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

type HistoryContentMutations = memory.HistoryContentMutations

func (b *JSONLBackend) CommitHistoryContentMutation(
	ctx context.Context,
	key string,
	plan memory.ContentMutationPlan,
) (memory.ContentMutationResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentMutationResult{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentMutations)
	if !ok {
		return memory.ContentMutationResult{}, memory.ErrHistoryUnsupported
	}
	return store.CommitHistoryContentMutation(ctx, key, plan)
}

func (b *JSONLBackend) ReadHistoryContentMutation(
	ctx context.Context,
	key, mutationID, intentDigest string,
) (memory.ContentMutationResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentMutationResult{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentMutations)
	if !ok {
		return memory.ContentMutationResult{}, memory.ErrHistoryUnsupported
	}
	return store.ReadHistoryContentMutation(ctx, key, mutationID, intentDigest)
}

func (b *JSONLBackend) ReadPendingHistoryContentMutation(
	ctx context.Context,
	key string,
) (memory.ContentMutationResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentMutationResult{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentMutations)
	if !ok {
		return memory.ContentMutationResult{}, memory.ErrHistoryUnsupported
	}
	return store.ReadPendingHistoryContentMutation(ctx, key)
}

func (b *JSONLBackend) ReadLatestHistoryContentMutation(
	ctx context.Context,
	key string,
) (memory.ContentMutationResult, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentMutationResult{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentMutations)
	if !ok {
		return memory.ContentMutationResult{}, memory.ErrHistoryUnsupported
	}
	return store.ReadLatestHistoryContentMutation(ctx, key)
}

func (b *JSONLBackend) ReadHistoryContentMutationFrame(
	ctx context.Context,
	key, mutationID string,
	side memory.ContentTransitionSide,
) (memory.ContentFrame, error) {
	if ctx != nil && ctx.Err() != nil {
		return memory.ContentFrame{}, ctx.Err()
	}
	store, ok := b.store.(HistoryContentMutations)
	if !ok {
		return memory.ContentFrame{}, memory.ErrHistoryUnsupported
	}
	return store.ReadHistoryContentMutationFrame(ctx, key, mutationID, side)
}

func (b *JSONLBackend) AcknowledgeHistoryContentMutation(
	ctx context.Context,
	key string,
	ack memory.ContentMutationAppliedAck,
) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	store, ok := b.store.(HistoryContentMutations)
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.AcknowledgeHistoryContentMutation(ctx, key, ack)
}

var _ HistoryContentMutations = (*JSONLBackend)(nil)
