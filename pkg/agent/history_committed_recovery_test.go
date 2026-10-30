package agent

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// Storage fault/NEW classification is tested at the real file boundary in
// memory. This wrapper exercises the caller's resulting public protocol after
// one real append, without exposing a production fault API.
type committedRecoveryContentBackend struct {
	*session.JSONLBackend
	appends  int
	fallback int
}

func (b *committedRecoveryContentBackend) AppendHistoryContent(
	ctx context.Context, key string, msg providers.Message,
) (memory.ContentAppendResult, error) {
	b.appends++
	result, err := b.JSONLBackend.AppendHistoryContent(ctx, key, msg)
	if err != nil {
		return result, err
	}
	result.Receipt, result.Entry.ReceiptBinding, result.Entry.Ref = nil, nil, nil
	result.DomainID, result.BeforeState, result.AfterState = "", "", ""
	return result, fmt.Errorf("%w: %w", memory.ErrHistoryInputCommitted,
		errors.Join(syscall.EIO, memory.ErrHistoryUnsupported, memory.ErrHistoryReceiptUnavailable))
}

func (b *committedRecoveryContentBackend) AppendMessage(key string, msg providers.Message) error {
	b.fallback++
	return b.JSONLBackend.AppendMessage(key, msg)
}

type committedRecoveryReceiptBackend struct {
	*session.JSONLBackend
	appends  int
	fallback int
}

func (b *committedRecoveryReceiptBackend) AppendHistoryReceipt(
	ctx context.Context, key string, msg providers.Message,
) (memory.CommittedHistoryMessage, error) {
	b.appends++
	if err := b.JSONLBackend.AppendHistoryContext(ctx, key, msg); err != nil {
		return memory.CommittedHistoryMessage{}, err
	}
	return memory.CommittedHistoryMessage{}, fmt.Errorf("%w: %w", memory.ErrHistoryInputCommitted,
		errors.Join(syscall.EIO, memory.ErrHistoryUnsupported, memory.ErrHistoryReceiptUnavailable))
}

func (b *committedRecoveryReceiptBackend) AppendMessage(key string, msg providers.Message) error {
	b.fallback++
	return b.JSONLBackend.AppendMessage(key, msg)
}

func TestContentMutationMirroredConfirmedInputDoesNotRetryOrInventReceipt(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "receipt", true: "content"}[typed], func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()
			store, err := memory.NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			backend := session.NewJSONLBackend(store)
			content := &committedRecoveryContentBackend{JSONLBackend: backend}
			receipt := &committedRecoveryReceiptBackend{JSONLBackend: backend}
			if typed {
				agent.Sessions = content
			} else {
				agent.Sessions = receipt
			}
			key := "sk_v1_committed_mirror_recovery"
			var entries []memory.ContentEntry
			if typed {
				al.SetSessionHistoryContentObserver(func(_ string, batch []memory.ContentEntry) { entries = batch })
			}
			proofs, notifications := 0, 0
			al.SetSessionHistoryReceiptObserver(func(_ string, batch []memory.CommittedHistoryMessage) {
				proofs += len(batch)
				notifications++
			})
			if err = al.writeMirrored(agent, key, []providers.Message{{
				Role: "assistant", Content: "one durable delivered response",
			}}); err != nil {
				t.Fatal("confirmed input invited a delivery retry", err)
			}
			history, err := store.GetHistory(t.Context(), key)
			if err != nil || len(history) != 1 || history[0].Content != "one durable delivered response" ||
				proofs != 0 || notifications == 0 || content.fallback+receipt.fallback != 0 ||
				content.appends+receipt.appends != 1 {
				t.Fatal("caller duplicated/lost source NEW or invented a receipt", history, proofs, notifications,
					content.appends, receipt.appends, content.fallback, receipt.fallback, err)
			}
			if typed && (len(entries) != 1 || entries[0].Ref != nil || entries[0].ReceiptBinding != nil ||
				entries[0].Message.Content != history[0].Content) {
				t.Fatal("content observer lost the unproved accepted response", entries)
			}
		})
	}
}
