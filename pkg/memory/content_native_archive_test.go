package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func nativeArchiveSource(t *testing.T) (*JSONLStore, string, ContentFrame) {
	t.Helper()
	store, createErr := NewJSONLStore(t.TempDir())
	if createErr != nil {
		t.Fatal(createErr)
	}
	key := "sk_v1_native_archive"
	if err := store.InitializeNewHistoryContentIdentity(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	runs := []string{uuid.NewString(), uuid.NewString()}
	for index, role := range []string{"user", "assistant", "user", "assistant"} {
		if err := store.AddFullMessage(
			providers.WithHistoryRun(t.Context(), runs[index/2]),
			key,
			providers.Message{Role: role, Content: "confirmed " + role},
		); err != nil {
			t.Fatal(err)
		}
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return store, key, frame
}

func nativeArchivePlan(frame ContentFrame) ContentCompactionPlan {
	refs := frameRefs(frame)
	return ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken,
		Retained: refs[2:], SummaryInputs: refs[:2], Summary: "compacted source context",
	}
}

func TestNativeArchiveRequiresContentProofIndependentlyOfReadableBodiesAndOrder(t *testing.T) {
	for _, missing := range []string{"none", "order_witness", "content_witness"} {
		t.Run(missing, func(t *testing.T) {
			store, key, frame := nativeArchiveSource(t)
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			if missing == "order_witness" {
				err = os.Remove(store.historyOrderWitnessPath(key))
			} else if missing == "content_witness" {
				err = store.writeContentCheckpointAvailability(key, meta, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if err != nil || len(page.Entries) != len(frame.Entries) {
				t.Fatal("independent proof loss hid the confirmed display", err)
			}
			if (page.Entries[0].Ref == nil) != (missing == "content_witness") ||
				page.Entries[1].Order.Valid() != (missing != "order_witness") {
				t.Fatal("public page did not preserve independent proof degradation")
			}
			transition := ContentTransition{Dropped: frameRefs(frame)[:2]}
			proof, err := store.verifyNativeArchive(t.Context(), key, meta, frame, transition)
			if missing == "content_witness" {
				if !errors.Is(err, ErrPublicHistoryCoverage) || len(proof.Archived) != 0 {
					t.Fatal("readable bodies were borrowed as a content archive proof", err)
				}
				return
			}
			if err != nil || len(proof.Archived) != len(transition.Dropped) {
				t.Fatal("independent order loss blocked proved content retention", err)
			}
		})
	}
}

func TestNativeArchiveReleasesOnlyVerifiedPublicCoverageAndRecoversTheSamePending(t *testing.T) {
	for _, kind := range []string{"verified", "missing_index", "corrupt_index", "corrupt_public", "missing_ref", "imported_ref"} {
		t.Run(kind, func(t *testing.T) {
			store, key, frame := nativeArchiveSource(t)
			plan := nativeArchivePlan(frame)
			index, err := os.ReadFile(store.publicIndexPath(key))
			if err != nil {
				t.Fatal(err)
			}
			public, err := os.ReadFile(store.publicHistoryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing_index":
				err = os.Remove(store.publicIndexPath(key))
			case "corrupt_index":
				err = os.WriteFile(store.publicIndexPath(key), []byte("corrupt"), 0o600)
			case "corrupt_public":
				changed := append([]byte(nil), public...)
				changed[10] ^= 1
				err = os.WriteFile(store.publicHistoryPath(key), changed, 0o600)
			case "missing_ref", "imported_ref":
				meta, readErr := store.readMeta(key)
				if readErr != nil {
					t.Fatal(readErr)
				}
				entries, readErr := store.readPublicEntries(key, meta)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if kind == "missing_ref" {
					entries[0].Ref = nil
				} else {
					entries[0].ImportID = "legacy:borrowed-ref"
				}
				change, readErr := store.replacePublicEntries(key, entries, &meta)
				if readErr != nil {
					t.Fatal(readErr)
				}
				err = store.writePublicChange(key, change)
				if err == nil {
					err = store.writeMeta(key, meta)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			transition, err := store.CommitHistoryCompaction(t.Context(), key, plan)
			if kind == "verified" {
				if err != nil || transition.MutationID != plan.MutationID {
					t.Fatal("verified native archive refused its exact compaction", transition, err)
				}
				pending, pendingErr := store.ReadPendingHistoryCompaction(t.Context(), key)
				if pendingErr != nil || pending.Version != 0 {
					t.Fatal("verified native retention was not released", pending, pendingErr)
				}
				return
			}
			if err == nil {
				t.Fatal("incomplete native evidence released the raw preimage")
			}
			if transition.Version == 0 {
				current, readErr := store.ReadHistoryContentFrame(t.Context(), key)
				if readErr != nil || current.StateToken != frame.StateToken ||
					len(current.Entries) != len(frame.Entries) {
					t.Fatal("unqualified source changed before committing the pending cut", current, readErr)
				}
				return
			}
			pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
			if err != nil || pending.MutationID != plan.MutationID {
				t.Fatal("failed native coverage discarded its pending operation", pending, err)
			}
			if kind == "missing_index" {
				if err := os.WriteFile(store.publicIndexPath(key), index, 0o600); err != nil {
					t.Fatal(err)
				}
				restarted, err := NewJSONLStore(store.dir)
				if err != nil {
					t.Fatal(err)
				}
				if prepareErr := restarted.PrepareNativeHistoryArchive(t.Context(), key); prepareErr != nil {
					t.Fatal(prepareErr)
				}
				pending, err := restarted.ReadPendingHistoryCompaction(t.Context(), key)
				if err != nil || pending.Version != 0 {
					t.Fatal("writer recovery did not finish exactly the retained cut", pending, err)
				}
			}
			if _, err := store.ReadHistoryCompactionFrame(
				t.Context(),
				key,
				plan.MutationID,
				ContentBefore,
			); kind != "missing_index" &&
				errors.Is(err, ErrContentArchiveAck) {
				t.Fatal("failed native evidence collected its raw immutable side")
			}
		})
	}
}

func TestNativeArchiveKeepsItsSuffixBudgetAndFencesNonLocalImports(t *testing.T) {
	for _, origin := range []string{"native_media", "external_import"} {
		t.Run(origin, func(t *testing.T) {
			store, key, frame := nativeArchiveSource(t)
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			anchor := history.Entries[1]
			if recordErr := store.RecordPublicMedia(
				t.Context(),
				key,
				anchor.RunID,
				"confirmed supplement",
			); recordErr != nil {
				t.Fatal(recordErr)
			}
			if origin == "external_import" {
				history, err = store.ReadPublicHistory(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.ImportPublicHistory(t.Context(), PublicHistoryImport{
					Key:        key,
					Generation: history.Generation,
					Revision:   history.Revision,
					BeforeID:   anchor.ID,
					Entries: []PublicHistoryEntry{
						{
							ID:          uuid.NewString(),
							ImportID:    "legacy:nonlocal",
							AnchorID:    anchor.ID,
							AnchorRef:   anchor.Ref,
							AnchorOrder: anchor.Order,
							Message:     providers.Message{Role: "assistant", Content: "legacy supplement"},
						},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			meta, err := store.readMeta(key)
			if err != nil || meta.PublicHistoryNonLocalImports != (origin == "external_import") {
				t.Fatal("import provenance was not fenced", meta.PublicHistoryNonLocalImports, err)
			}
			entries, err := store.readPublicEntries(key, meta)
			if err != nil {
				t.Fatal(err)
			}
			prefix := make([]PublicHistoryEntry, maxNativeArchiveRows)
			for index := range prefix {
				prefix[index] = PublicHistoryEntry{
					ID:      uuid.NewString(),
					Message: providers.Message{Role: "assistant", Content: "previously archived source"},
				}
			}
			change, err := store.replacePublicEntries(key, slices.Concat(prefix, entries), &meta)
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := store.writePublicChange(key, change); writeErr != nil {
				t.Fatal(writeErr)
			}
			if writeErr := store.writeMeta(key, meta); writeErr != nil {
				t.Fatal(writeErr)
			}
			restarted, err := NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			plan := nativeArchivePlan(frame)
			_, err = restarted.CommitHistoryCompaction(t.Context(), key, plan)
			pending, pendingErr := restarted.ReadPendingHistoryCompaction(t.Context(), key)
			if origin == "native_media" {
				if err != nil || pendingErr != nil || pending.Version != 0 {
					t.Fatal("native suffix required an unbounded archive scan", err, pending, pendingErr)
				}
			} else if !errors.Is(err, ErrHistoryCapacity) || pendingErr != nil || pending.Version != 0 {
				t.Fatal("nonlocal imports escaped the bounded proof or published an unprovable pending cut", err, pending, pendingErr)
			}
			if origin == "external_import" {
				current, readErr := restarted.ReadHistoryContentFrame(t.Context(), key)
				if readErr != nil || current.StateToken != frame.StateToken || current.Summary != frame.Summary {
					t.Fatal("capacity refusal changed the source frame", current, readErr)
				}
			}
		})
	}
}

func TestNativeArchiveImportFenceIsCheckpointedAndRecoversOnlyItsJournal(t *testing.T) {
	for _, phase := range []string{"committed", "witness_interrupted"} {
		t.Run(phase, func(t *testing.T) {
			store, key, before := nativeArchiveSource(t)
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			legacyDigest := contentJSONDigest(struct {
				State any    `json:"state"`
				Count int    `json:"count"`
				Skip  int    `json:"skip"`
				Bytes *int64 `json:"bytes"`
			}{contentIdentityWire(meta.ContentIdentity), meta.Count, meta.Skip, meta.HistoryBytes})
			if contentCheckpointDigest(meta) != legacyDigest {
				t.Fatal("an absent import fence changed existing checkpoint digests")
			}
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			failures := 0
			if phase == "witness_interrupted" {
				store.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if path == store.contentDomainPath(key) {
						failures++
						return errors.New("injected import fence checkpoint failure")
					}
					return fileutil.WriteFileAtomic(path, data, mode)
				}
			}
			anchor := history.Entries[1]
			_, importErr := store.ImportPublicHistory(t.Context(), PublicHistoryImport{
				Key:        key,
				Generation: history.Generation,
				Revision:   history.Revision,
				BeforeID:   history.Entries[0].ID,
				Entries: []PublicHistoryEntry{
					{
						ID:          uuid.NewString(),
						ImportID:    "legacy:checkpoint-fence",
						AnchorID:    anchor.ID,
						AnchorRef:   anchor.Ref,
						AnchorOrder: anchor.Order,
						Message:     providers.Message{Role: "assistant", Content: "imported supplement"},
					},
				},
			})
			if phase == "committed" && importErr != nil {
				t.Fatal(importErr)
			}
			if phase == "witness_interrupted" && failures == 0 {
				t.Fatal("the import did not reach its checkpoint publication")
			}
			store.writeFile = nil
			restarted, err := NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			current, err := restarted.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || current.IdentityStatus != ContentIdentityKnown || current.DomainID != before.DomainID ||
				current.StateToken != before.StateToken {
				t.Fatal("journal recovery changed identity or did not publish its exact fence", current, err)
			}
			meta, err = restarted.readMeta(key)
			if err != nil || !meta.PublicHistoryNonLocalImports || contentCheckpointDigest(meta) == legacyDigest {
				t.Fatal("the import fence was not committed by its witness", err)
			}
			raw, err := os.ReadFile(restarted.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if decodeErr := json.Unmarshal(raw, &object); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			delete(object, "public_history_non_local_imports")
			lost, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			if writeErr := os.WriteFile(restarted.metaPath(key), lost, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			transcript, err := os.ReadFile(restarted.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			current, readErr := restarted.ReadHistoryContentFrame(t.Context(), key)
			if readErr == nil && current.IdentityStatus == ContentIdentityKnown {
				t.Fatal("a lost fence silently selected the suffix proof")
			}
			if _, commitErr := restarted.CommitHistoryCompaction(
				t.Context(),
				key,
				nativeArchivePlan(before),
			); commitErr == nil {
				t.Fatal("a lost fence released raw retention")
			}
			after, err := os.ReadFile(restarted.jsonlPath(key))
			if err != nil || !bytes.Equal(after, transcript) {
				t.Fatal("a lost fence changed the retained transcript", err)
			}
		})
	}
}
