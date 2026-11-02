package memory

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

func TestPublicHistoryReadDigestBindsTheRawOccurrenceBeforeProjection(t *testing.T) {
	at := time.Date(2026, 11, 2, 8, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	raw := providers.Message{
		Role: "assistant", Content: "[report](report.pdf)", CreatedAt: &at,
		HistoryID: uuid.NewString(), HistoryOrderVersion: 1, HistoryStreamID: uuid.NewString(), HistorySequence: 1,
	}
	projected := providers.Message{Role: "assistant", Content: "[report](artifact:version.pdf)", CreatedAt: &at}
	raw.HistoryPublicMessage = &projected
	entries := publicHistoryEntriesAt([]providers.Message{raw}, SessionMeta{}, 0)
	if len(entries) != 1 || entries[0].Message.Content != projected.Content ||
		entries[0].SourceDigest != messageutil.SourceContentDigest(at.Format(time.RFC3339Nano), raw.Content) ||
		entries[0].SourceDigest == messageutil.SourceContentDigest(at.Format(time.RFC3339Nano), projected.Content) {
		t.Fatal("projected body replaced its raw receipt binding", entries)
	}
	if !validPublicPageEntry(entries[0]) {
		t.Fatal("producer digest rejected by the bounded reader")
	}
	imported := entries[0]
	imported.ImportID = "legacy:attempt"
	if validPublicEntry(imported) || validPublicPageEntry(imported) {
		t.Fatal("an imported occurrence forged a native raw receipt binding")
	}
	legacy := entries[0]
	legacy.SourceDigest = ""
	if !validPublicPageEntry(legacy) {
		t.Fatal("older confirmed source without a digest became unreadable")
	}
	corrupt := entries[0]
	corrupt.SourceDigest = "source:v1:invalid"
	if validPublicPageEntry(corrupt) {
		t.Fatal("corrupt private digest was accepted")
	}
}

func TestPublicHistoryRewritePreparationBindsRawRetainedPrefixBeforeProjection(t *testing.T) {
	store, key := newTestStore(t), "sk_v1_rewrite_raw_binding"
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	raw := providers.Message{Role: "assistant", Content: "[report](report.pdf)", CreatedAt: &at}
	projected := providers.Message{Role: "assistant", Content: "[report](artifact:version.pdf)", CreatedAt: &at}
	raw.HistoryPublicMessage = &projected
	if err := store.AddFullMessage(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.publicHistoryPath(key)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.publicIndexPath(key)); err != nil {
		t.Fatal(err)
	}
	meta.PublicHistoryVersion = 0
	meta.Skip = meta.Count
	next := meta
	change, err := store.preparePublicRewrite(key, meta, &next, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := decodePublicEntries(change.Data, next)
	if err != nil || len(entries) != 1 || entries[0].Message.Content != projected.Content ||
		entries[0].SourceDigest != publicSourceDigest(raw) {
		t.Fatal("rewrite preparation rebound a native receipt to its projected display", entries, err)
	}
}
