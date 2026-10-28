package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

type publicHistoryStore interface {
	ReadPublicHistory(context.Context, string) (memory.PublicHistory, error)
	ImportPublicHistory(context.Context, memory.PublicHistoryImport) (memory.PublicHistory, error)
}

func (b *JSONLBackend) ReadPublicHistory(ctx context.Context, key string) (memory.PublicHistory, error) {
	store, ok := b.store.(publicHistoryStore)
	if !ok {
		return memory.PublicHistory{}, memory.ErrHistoryUnsupported
	}
	return store.ReadPublicHistory(ctx, key)
}

func (b *JSONLBackend) ImportPublicHistory(ctx context.Context, request memory.PublicHistoryImport) (memory.PublicHistory, error) {
	store, ok := b.store.(publicHistoryStore)
	if !ok {
		return memory.PublicHistory{}, memory.ErrHistoryUnsupported
	}
	return store.ImportPublicHistory(ctx, request)
}

func (b *JSONLBackend) RecordPublicMedia(ctx context.Context, key, runID, content string) error {
	store, ok := b.store.(interface {
		RecordPublicMedia(context.Context, string, string, string) error
	})
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.RecordPublicMedia(ctx, key, runID, content)
}

func (b *JSONLBackend) DeleteSession(key string) error {
	store, ok := b.store.(interface {
		DeleteSession(context.Context, string) error
	})
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.DeleteSession(context.Background(), key)
}
