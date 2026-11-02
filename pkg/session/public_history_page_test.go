package session_test

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestJSONLBackendPublicHistoryPageResolvesAliasesToThePreparedSource(t *testing.T) {
	store, openErr := memory.NewJSONLStore(t.TempDir())
	if openErr != nil {
		t.Fatal(openErr)
	}
	backend := session.NewJSONLBackend(store)
	key, alias := "sk_v1_paged_alias", "agent:main:paged-alias"
	if _, err := backend.AppendHistoryReceipt(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "canonical occurrence"},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSessionMeta(t.Context(), key, nil, []string{alias}); err != nil {
		t.Fatal(err)
	}
	page, err := backend.ReadPublicHistoryPage(t.Context(), memory.PublicHistoryPageRequest{Key: alias})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message.Content != "canonical occurrence" {
		t.Fatal("alias selected a different public source", page, err)
	}
}
