package session

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/memory"
)

type ReadOrderRecoveryStore interface {
	WithReadOrderRecovery(context.Context, string, memory.ReadOrderRecoveryRequest, func(memory.ReadOrderRecovery) error) error
	ReleaseReadOrderRecovery(context.Context, string, string) error
}

func (b *JSONLBackend) WithReadOrderRecovery(ctx context.Context, key string, request memory.ReadOrderRecoveryRequest, apply func(memory.ReadOrderRecovery) error) error {
	store, ok := b.store.(ReadOrderRecoveryStore)
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.WithReadOrderRecovery(ctx, key, request, apply)
}

func (b *JSONLBackend) ReleaseReadOrderRecovery(ctx context.Context, key, attempt string) error {
	store, ok := b.store.(ReadOrderRecoveryStore)
	if !ok {
		return memory.ErrHistoryUnsupported
	}
	return store.ReleaseReadOrderRecovery(ctx, key, attempt)
}

func (b *JSONLBackend) ReadOrderRecoveryActive(ctx context.Context, key string) (bool, error) {
	store, ok := b.store.(interface {
		ReadOrderRecoveryActive(context.Context, string) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return store.ReadOrderRecoveryActive(ctx, key)
}
