package session_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/session"
)

func TestJSONLBackendDeleteRetiresPendingContentDomain(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_backend_delete"
	backend := session.NewJSONLBackend(store)
	for range 4 {
		backend.AddMessage(key, "user", "durable source")
	}
	frame, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	frame, err = backend.InitializeHistoryContentIdentity(t.Context(), key, frame.Observation)
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]memory.ContentRef, len(frame.Entries))
	for i, entry := range frame.Entries {
		refs[i] = *entry.Ref
	}
	pending, err := backend.CommitHistoryCompaction(t.Context(), key, memory.ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken,
		SummaryInputs: refs[:2], Retained: refs[2:], Summary: "pending summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The gateway discovers this optional method on SessionStore. A history
	// replacement is deliberately not equivalent to an explicit owner DELETE.
	deleter, ok := any(backend).(interface{ DeleteSession(key string) error })
	if !ok {
		t.Fatal("JSONL backend has no explicit owner DELETE for the gateway")
	}
	if err = deleter.DeleteSession(key); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{
		".jsonl", ".meta.json", ".jsonl.commit.json", ".jsonl.commit.decision",
		".jsonl.recovery-required", ".jsonl.order-witness", ".jsonl.content-domain",
	} {
		if _, statErr := os.Stat(filepath.Join(dir, key+suffix)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("owner DELETE left a source or its identity behind", suffix, statErr)
		}
	}
	if got := backend.GetHistory(key); len(got) != 0 || backend.GetSummary(key) != "" {
		t.Fatal("warm backend resurrected deleted content", got)
	}
	if err = backend.AcknowledgeHistoryCompaction(t.Context(), key, memory.ContentArchiveAck{
		DomainID: pending.DomainID, MutationID: pending.MutationID,
		BeforeState: pending.BeforeState, AfterState: pending.AfterState,
		Archived: pending.Dropped, ArchiveCommitID: uuid.NewString(),
	}); err == nil {
		t.Fatal("old archive confirmation resurrected a deleted domain")
	}
	backend.AddMessage(key, "user", "new session")
	before, err := backend.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	recreated, err := backend.InitializeHistoryContentIdentity(t.Context(), key, before.Observation)
	if err != nil || recreated.DomainID == pending.DomainID || len(recreated.Entries) != 1 {
		t.Fatal("recreated source reused the deleted domain", recreated, err)
	}
}
