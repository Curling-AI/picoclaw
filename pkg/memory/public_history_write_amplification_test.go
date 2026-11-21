package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

// Nothing displays a cron run's own transcript, so its session keeps only the
// model history: no .jsonl.public/.index pair to write on every message. The
// run survives a rollback cut and a truncating save, its projection readers
// report missing coverage, the prune still removes every file, and a
// conversation keeps its projection.
func TestCronRunSessionKeepsNoPublicProjection(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	conversation := "sk_v1_conversation"
	runs := []string{
		"agent:cron-job-1-0b89ab77-2375-42d6-ac89-83d2ebc233b6",
		"agent:cronmodel-job-1-1c89ab77-2375-42d6-ac89-83d2ebc233b6",
	}
	done := providers.Message{Role: "assistant", Content: "done"}
	for _, key := range append([]string{conversation}, runs...) {
		// The agent records the session's metadata before its first message.
		if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.AddMessage(ctx, key, "user", "run the report"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendHistoryReceipt(ctx, key, done); err != nil {
			t.Fatal(err)
		}
	}

	for _, key := range runs {
		if err := store.AddMessage(ctx, key, "user", "aborted turn"); err != nil {
			t.Fatal(err)
		}
		history, err := store.GetHistory(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.SetHistory(ctx, key, history[:2]); err != nil {
			t.Fatal("rollback:", err)
		}
		if err = store.TruncateHistory(ctx, key, 1); err != nil {
			t.Fatal("truncate:", err)
		}
		if err = store.CompactForSessionSave(ctx, key); err != nil {
			t.Fatal("save:", err)
		}
		history, err = store.GetHistory(ctx, key)
		if err != nil || len(history) != 1 || history[0].Content != "done" {
			t.Fatalf("%s history = %+v, %v; want the retained reply", key, history, err)
		}
		_, err = store.ReadPublicHistoryPage(ctx, PublicHistoryPageRequest{Key: key})
		if !errors.Is(err, ErrPublicHistoryCoverage) {
			t.Fatalf("%s page read = %v, want missing coverage", key, err)
		}
		if _, err = store.PreparePublicHistory(ctx, key, ""); !errors.Is(err, ErrPublicHistoryCoverage) {
			t.Fatalf("%s preparation = %v, want missing coverage", key, err)
		}
		name := sanitizeKey(key)
		want := []string{name + ".jsonl", name + ".jsonl.order-witness", name + ".meta.json"}
		if got := sessionFileNames(t, store.dir, name); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s files = %v, want %v", key, got, want)
		}
	}
	if !exists(t, store.publicHistoryPath(conversation)) || !exists(t, store.publicIndexPath(conversation)) {
		t.Fatal("a conversation lost its public projection")
	}

	removed, err := store.PruneCronRuns(time.Hour, time.Now().Add(48*time.Hour))
	if err != nil || removed != len(runs) {
		t.Fatalf("prune removed %d runs (%v), want %d", removed, err, len(runs))
	}
	for _, key := range runs {
		if got := sessionFileNames(t, store.dir, sanitizeKey(key)); len(got) != 0 {
			t.Fatalf("%s left %v behind", key, got)
		}
	}
}

func sessionFileNames(t *testing.T, dir, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), name+".") {
			names = append(names, entry.Name())
		}
	}
	return names
}
