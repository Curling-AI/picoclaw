package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestContentLineageOwnerDeleteRefusesUnresolvedJournalWithoutPartialClear(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_owner_delete_journal"
	pendingContent(t, s, key)
	before := make(map[string][]byte)
	for _, path := range []string{s.jsonlPath(key), s.metaPath(key), s.contentDomainPath(key)} {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.DeleteSession(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatal("owner DELETE ignored cancellation", err)
	}
	if err = os.WriteFile(s.historyRecoveryPath(key), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(t.Context(), key); err == nil {
		t.Fatal("owner DELETE discarded a source with an unresolved write journal")
	}
	for path, want := range before {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(want, got) {
			t.Fatal("refused owner DELETE partially cleared source/summary/domain", path, readErr)
		}
	}
}
