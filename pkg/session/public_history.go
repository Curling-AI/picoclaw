package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

type publicHistoryStore interface {
	ReadPublicHistory(ctx context.Context, key string) (memory.PublicHistory, error)
	ImportPublicHistory(ctx context.Context, request memory.PublicHistoryImport) (memory.PublicHistory, error)
}

func (b *JSONLBackend) ReadPublicHistory(ctx context.Context, key string) (memory.PublicHistory, error) {
	store, ok := b.store.(publicHistoryStore)
	if !ok {
		return memory.PublicHistory{}, memory.ErrHistoryUnsupported
	}
	return store.ReadPublicHistory(ctx, b.resolveSessionKey(key))
}

func (b *JSONLBackend) ImportPublicHistory(
	ctx context.Context,
	request memory.PublicHistoryImport,
) (memory.PublicHistory, error) {
	store, ok := b.store.(publicHistoryStore)
	if !ok {
		return memory.PublicHistory{}, memory.ErrHistoryUnsupported
	}
	resolved := request
	resolved.Key = b.resolveSessionKey(request.Key)
	return store.ImportPublicHistory(ctx, resolved)
}

func (b *JSONLBackend) RecordPublicMedia(ctx context.Context, key, runID, content string) error {
	store, ok := b.store.(interface {
		RecordPublicMedia(ctx context.Context, key, runID, content string) error
	})
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.RecordPublicMedia(ctx, b.resolveSessionKey(key), runID, content)
}

func (b *JSONLBackend) PreparePublicHistory(
	ctx context.Context,
	key, cursor string,
) (memory.PublicHistoryPreparationResult, error) {
	store, ok := b.store.(interface {
		PreparePublicHistory(ctx context.Context, key, cursor string) (memory.PublicHistoryPreparationResult, error)
	})
	if !ok {
		return memory.PublicHistoryPreparationResult{}, memory.ErrHistoryUnsupported
	}
	return store.PreparePublicHistory(ctx, b.resolveSessionKey(key), cursor)
}

func (b *JSONLBackend) SetPublicHistoryProjector(projector memory.PublicHistoryProjector) {
	if store, ok := b.store.(interface {
		SetPublicHistoryProjector(projector memory.PublicHistoryProjector)
	}); ok {
		store.SetPublicHistoryProjector(projector)
	}
}
