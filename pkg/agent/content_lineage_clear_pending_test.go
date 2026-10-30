package agent

import (
	"errors"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
)

func TestContentLineageClearPendingReturnsCheckedError(t *testing.T) {
	al, store, _, key := newContentCompactionLoop(t)
	if err := al.contextManager.Compact(
		t.Context(),
		&CompactRequest{SessionKey: key, Reason: ContextCompressReasonRetry},
	); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	err = al.contextManager.Clear(t.Context(), key)
	if !errors.Is(err, memory.ErrContentLineagePending) {
		t.Error("Clear hid checked refusal and reported success", err)
	}
	after, readErr := store.ReadHistoryContentFrame(t.Context(), key)
	if readErr != nil || after.StateToken != before.StateToken || after.Summary != before.Summary ||
		len(after.Entries) != len(before.Entries) {
		t.Fatal("refused clear partially changed summary/content", after.Summary, readErr)
	}
}
