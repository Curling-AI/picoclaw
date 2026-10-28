package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestHandleDeleteSession_RetiresPendingContentIdentity(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	dir := sessionsTestDir(t, configPath)
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := legacyPicoSessionPrefix + "content-lineage-delete"
	for range 4 {
		if err = store.AddMessage(t.Context(), key, "assistant", "same"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := store.InitializeHistoryContentIdentity(t.Context(), key, before.Observation)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]memory.ContentRef, len(frame.Entries))
	for i, entry := range frame.Entries {
		refs[i] = *entry.Ref
	}
	plan := memory.ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		SummaryInputs: refs[:2],
		Retained:      refs[2:],
		Summary:       "old summary",
	}
	transition, err := store.CommitHistoryCompaction(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	oldAck := memory.ContentArchiveAck{
		DomainID:        transition.DomainID,
		MutationID:      transition.MutationID,
		BeforeState:     transition.BeforeState,
		AfterState:      transition.AfterState,
		Archived:        transition.Dropped,
		ArchiveCommitID: uuid.NewString(),
	}
	mux := http.NewServeMux()
	NewHandler(configPath).RegisterRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/content-lineage-delete", nil))
	if response.Code != http.StatusNoContent {
		t.Fatal("explicit owner deletion was blocked by pending retention", response.Code, response.Body.String())
	}
	base := filepath.Join(dir, sanitizeSessionKey(key))
	for _, suffix := range []string{".jsonl", ".meta.json", ".jsonl.commit.json", ".jsonl.commit.decision", ".jsonl.content-domain", ".jsonl.order-witness"} {
		if _, err = os.Stat(base + suffix); !os.IsNotExist(err) {
			t.Fatal("explicit deletion left a prior domain file", suffix, err)
		}
	}
	if err = store.AcknowledgeHistoryCompaction(
		t.Context(),
		key,
		oldAck,
	); !errors.Is(
		err,
		memory.ErrContentIdentityUnavailable,
	) {
		t.Fatal("old archive confirmation recreated deleted identity", err)
	}
	if _, err = os.Stat(base + ".jsonl"); !os.IsNotExist(err) {
		t.Fatal("old ACK recreated deleted content", err)
	}
	newMessage, err := store.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "same"},
	)
	if err != nil || newMessage.Entry.Ref != nil {
		t.Fatal("fresh content inherited the deleted domain", newMessage, err)
	}
	newBefore, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.InitializeHistoryContentIdentity(t.Context(), key, newBefore.Observation)
	if err != nil || fresh.DomainID == frame.DomainID || len(fresh.Entries) != 1 || *fresh.Entries[0].Ref == refs[0] ||
		fresh.Summary != "" {
		t.Fatal("fresh baseline remapped deleted identity", fresh, err)
	}
	if err = store.AcknowledgeHistoryCompaction(
		t.Context(),
		key,
		oldAck,
	); !errors.Is(
		err,
		memory.ErrContentArchiveAck,
	) {
		t.Fatal("old ACK was accepted by a new domain", err)
	}
	if _, err = store.CommitHistoryCompaction(t.Context(), key, plan); !errors.Is(err, memory.ErrContentStateChanged) {
		t.Fatal("old compaction was replayed into the recreated source", err)
	}
}
