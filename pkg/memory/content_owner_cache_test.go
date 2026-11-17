package memory

import (
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// A key with ':' scans every session's identity. Warm identities come from the
// write-through cache; only a cold store pays (and fails on) the disk read.
func TestContentKeyScanServesWarmIdentitiesFromCache(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, key := "agent:cron-a-1", "agent:cron-b-2"
	hello := providers.Message{Role: "user", Content: "hi"}
	for _, k := range []string{other, key} {
		if _, err = store.AppendHistoryReceipt(t.Context(), k, hello); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(store.metaPath(other), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err = store.ReadHistoryContentFrame(t.Context(), key); err != nil {
		t.Fatal("warm scan read another session's meta from disk:", err)
	}

	cold, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cold.ReadHistoryContentFrame(t.Context(), key); !errors.Is(err, ErrHistoryOwnershipUnavailable) {
		t.Fatal("cold scan must still fail on an unreadable identity, got", err)
	}
}
