package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicMediaTailAppendDoesNotJournalOrRewriteTheConfirmedPrefix(t *testing.T) {
	store, key, run := newTestStore(t), "sk_v1_public_tail_write", uuid.NewString()
	if err := store.AddMessage(
		providers.WithHistoryRun(t.Context(), run),
		key,
		"assistant",
		strings.Repeat("x", 512<<10),
	); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
	}
	var journal historyWriteRecovery
	publicWrites := 0
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.historyRecoveryPath(key) {
			if decodeErr := json.Unmarshal(data, &journal); decodeErr != nil {
				return decodeErr
			}
		}
		if path == store.publicHistoryPath(key) {
			publicWrites++
		}
		return os.WriteFile(path, data, mode)
	}
	if err = store.RecordPublicMedia(t.Context(), key, run, "[report](artifact:confirmed.pdf)"); err != nil {
		t.Fatal(err)
	}
	if journal.Public == nil || journal.Public.Offset == nil || *journal.Public.Offset != int64(len(before)) ||
		len(journal.Public.Data) != 0 ||
		publicWrites != 0 {
		t.Fatalf("tail media rewrote its confirmed prefix: full writes=%d", publicWrites)
	}
	after, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil || len(after) <= len(before) || string(after[:len(before)]) != string(before) {
		t.Fatal("tail append changed the confirmed prefix", err)
	}
	store.writeFile = nil
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(history.Entries) != 2 {
		t.Fatal("tail append lost public coverage", history, err)
	}
}

func TestPublicHistoryUnchangedCompactionDoesNotRewriteDisplay(t *testing.T) {
	store, key := newTestStore(t), "sk_v1_public_noop_rewrite"
	for _, text := range []string{"prefix", "retained"} {
		if err := store.AddMessage(t.Context(), key, "assistant", text); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	var journal historyWriteRecovery
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.historyRecoveryPath(key) {
			if err := json.Unmarshal(data, &journal); err != nil {
				return err
			}
		}
		if path == store.publicHistoryPath(key) {
			return errors.New("unchanged public source must not be rewritten")
		}
		return os.WriteFile(path, data, mode)
	}
	if err := store.Compact(t.Context(), key); err != nil {
		t.Fatal("compaction rewrote unchanged display", err)
	}
	if journal.Public != nil {
		t.Fatal("unchanged display added a rollback copy to the journal")
	}
}

func TestPublicMediaTailAppendRollsBackOnlyItsUnconfirmedSuffix(t *testing.T) {
	store, key, run := newTestStore(t), "sk_v1_public_tail_rollback", uuid.NewString()
	if err := store.AddMessage(
		providers.WithHistoryRun(t.Context(), run),
		key,
		"assistant",
		"confirmed prefix",
	); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil {
		t.Fatal(err)
	}
	failing := true
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.metaPath(key) && failing {
			failing = false
			return errors.New("metadata publication interrupted")
		}
		return os.WriteFile(path, data, mode)
	}
	if err = store.RecordPublicMedia(t.Context(), key, run, "unconfirmed supplement"); err == nil {
		t.Fatal("interrupted metadata publication reported committed media")
	}
	store.writeFile = nil
	after, err := os.ReadFile(store.publicHistoryPath(key))
	if err != nil || string(before) != string(after) {
		t.Fatal("rollback did not preserve the exact old public prefix", err)
	}
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil || len(history.Entries) != 1 || history.Entries[0].Message.Content != "confirmed prefix" {
		t.Fatal("rolled-back suffix became confirmed coverage", history, err)
	}
}

type prefixPublicVerifier struct{ seen []string }

func (p *prefixPublicVerifier) Commit(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{Message: message}, nil
}

func (p *prefixPublicVerifier) Legacy(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{Message: message}, nil
}

func (p *prefixPublicVerifier) Verify(_ context.Context, message providers.Message) error {
	p.seen = append(p.seen, message.Content)
	if message.Content == "unrelated retained" {
		return errors.New("must not verify an unrelated retained artifact")
	}
	return nil
}

func TestPublicPrefixVerificationChecksItsSupplementWithoutHashingRetainedArtifacts(t *testing.T) {
	store, key, run := newTestStore(t), "sk_v1_public_prefix_verification", uuid.NewString()
	if err := store.AddMessage(providers.WithHistoryRun(t.Context(), run), key, "assistant", "prefix"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(t.Context(), key, run, "prefix supplement"); err != nil {
		t.Fatal(err)
	}
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	anchor := history.Entries[1]
	if _, err = store.ImportPublicHistory(t.Context(), PublicHistoryImport{
		Key: key, Generation: history.Generation, Revision: history.Revision, AfterID: anchor.ID,
		Entries: []PublicHistoryEntry{
			{
				ID:          uuid.NewString(),
				ImportID:    "nested",
				AnchorID:    anchor.ID,
				AnchorRef:   anchor.Ref,
				AnchorOrder: anchor.Order,
				Message:     providers.Message{Role: "assistant", Content: "nested supplement"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.AddMessage(t.Context(), key, "assistant", "unrelated retained"); err != nil {
		t.Fatal(err)
	}
	if err = store.TruncateHistory(t.Context(), key, 1); err != nil {
		t.Fatal(err)
	}
	projector := &prefixPublicVerifier{}
	store.SetPublicHistoryProjector(projector)
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.verifyPublicPrefix(t.Context(), key, meta); err != nil {
		t.Fatal("unrelated retained artifact blocked prefix verification", err)
	}
	if strings.Join(projector.seen, ",") != "prefix,prefix supplement,nested supplement" {
		t.Fatal("prefix verification omitted its supplement", projector.seen)
	}
}
