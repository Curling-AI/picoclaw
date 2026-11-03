package memory

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestNativeArchiveClassifiesEveryHiddenSystemPrefix(t *testing.T) {
	store, key, _ := nativeArchiveSource(t)
	if err := store.AddMessage(t.Context(), key, "user", "[System: async:spawn] private result"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(t.Context(), key, "user", "next prompt"); err != nil {
		t.Fatal(err)
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken, Summary: "summary", SummaryInputs: refs[:5], Retained: refs[5:]}
	if _, err = store.CommitHistoryCompaction(t.Context(), key, plan); err != nil {
		t.Fatal("hidden system row blocked native archive", err)
	}
	pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
	if err != nil || pending.Version != 0 {
		t.Fatal("native archive did not finish", pending, err)
	}
}

func TestNativeArchivePolicySurvivesClearAndNextCompaction(t *testing.T) {
	store, key, _ := nativeArchiveSource(t)
	if err := store.ClearHistory(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.ContentIdentity.ArchivePolicy != nativePublicArchivePolicy {
		t.Fatal("clear lost native archive policy", err)
	}
	for _, role := range []string{"user", "assistant", "user", "assistant"} {
		if err := store.AddMessage(t.Context(), key, role, "new domain"); err != nil {
			t.Fatal(err)
		}
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitHistoryCompaction(t.Context(), key, nativeArchivePlan(frame)); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
	if err != nil || pending.Version != 0 {
		t.Fatal("post-clear archive was not acknowledged", pending, err)
	}
}

func TestNativeArchiveImportCannotReattestMissingContentWitness(t *testing.T) {
	for _, kind := range []string{"missing", "unreadable", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			store, key, _ := nativeArchiveSource(t)
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				err = os.Remove(store.contentDomainPath(key))
			case "unreadable":
				err = os.WriteFile(store.contentDomainPath(key), []byte("invalid"), 0600)
			case "unavailable":
				err = store.writeContentCheckpointAvailability(key, meta, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			history, err = store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			anchor := history.Entries[1]
			_, err = store.ImportPublicHistory(t.Context(), PublicHistoryImport{Key: key, Generation: history.Generation, Revision: history.Revision, BeforeID: anchor.ID, Entries: []PublicHistoryEntry{{ID: uuid.NewString(), ImportID: "legacy:lost-witness", AnchorID: anchor.ID, AnchorRef: anchor.Ref, AnchorOrder: anchor.Order, Message: providers.Message{Role: "assistant", Content: "import"}}}})
			if !errors.Is(err, ErrContentIdentityUnavailable) {
				t.Fatal("import did not reject unavailable content proof", err)
			}
			if _, err = os.Stat(store.historyRecoveryPath(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected import left recovery journal", err)
			}
			frame, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || frame.IdentityStatus != ContentIdentityUnavailable {
				t.Fatal("import reattested or blocked source", frame, err)
			}
		})
	}
}

func TestNativeContentInitializationDoesNotConfuseDottedKeys(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_new"
	if err := os.WriteFile(store.jsonlPath(key+".other"), []byte("other session"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeNewHistoryContentIdentity(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.ContentIdentity == nil || meta.ContentIdentity.ArchivePolicy != nativePublicArchivePolicy {
		t.Fatal("neighboring dotted key blocked initialization", err)
	}
}

func TestNativeArchiveVerifiesTransitiveAndBoundarySupplements(t *testing.T) {
	for _, link := range []string{"anchor", "boundary"} {
		t.Run(link, func(t *testing.T) {
			store, key, frame := nativeArchiveSource(t)
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			anchor := history.Entries[0]
			first := PublicHistoryEntry{ID: uuid.NewString(), ImportID: "legacy:first", AnchorID: anchor.ID, AnchorRef: anchor.Ref, AnchorOrder: anchor.Order, Message: providers.Message{Role: "assistant", Content: "safe supplement"}}
			history, err = store.ImportPublicHistory(t.Context(), PublicHistoryImport{Key: key, Generation: history.Generation, Revision: history.Revision, BeforeID: anchor.ID, Entries: []PublicHistoryEntry{first}})
			if err != nil {
				t.Fatal(err)
			}
			second := PublicHistoryEntry{ID: uuid.NewString(), ImportID: "legacy:second", Message: providers.Message{Role: "assistant", Content: "[missing](artifact:gone.pdf)"}}
			if link == "anchor" {
				second.AnchorID = first.ID
			}
			history, err = store.ImportPublicHistory(t.Context(), PublicHistoryImport{Key: key, Generation: history.Generation, Revision: history.Revision, BeforeID: first.ID, Entries: []PublicHistoryEntry{second}})
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CommitHistoryCompaction(t.Context(), key, nativeArchivePlan(frame))
			if !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("unverified linked supplement released archive", err)
			}
			pending, err := store.ReadPendingHistoryCompaction(t.Context(), key)
			after, frameErr := store.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || frameErr != nil || pending.Version != 0 || after.StateToken != before.StateToken {
				t.Fatal("unverified supplement published a pending cut", pending, err, frameErr)
			}
		})
	}
}

func TestNativeContentFrameDoesNotBorrowStaleProjection(t *testing.T) {
	store, key, _ := nativeArchiveSource(t)
	stale := providers.Message{Role: "assistant", Content: "confirmed", ModelName: "old model"}
	if err := store.AddFullMessage(t.Context(), key, providers.Message{Role: "assistant", Content: "confirmed", ModelName: "new model", HistoryPublicMessage: &stale}); err != nil {
		t.Fatal(err)
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityKnown {
		t.Fatal("stale optional display blocked raw frame", frame, err)
	}
	if frame.Entries[len(frame.Entries)-1].Message.HistoryPublicMessage != nil {
		t.Fatal("stale projection was borrowed by frame")
	}
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken, Summary: "summary", SummaryInputs: refs}
	if _, err := store.CommitHistoryCompaction(t.Context(), key, plan); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatal("stale projection released raw archive coverage", err)
	}
}

func TestNativeArchiveRecoveryUsesItsCommittedPublicFrontier(t *testing.T) {
	store, key, frame := nativeArchiveSource(t)
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.PublicHistoryNonLocalImports = true
	if err := store.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	if err := store.writeContentCheckpoint(key, meta); err != nil {
		t.Fatal(err)
	}
	transition := ContentTransition{Dropped: frameRefs(frame)[:2]}
	proof, err := store.verifyNativeArchive(t.Context(), key, meta, frame, transition)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.readPublicEntries(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxNativeArchiveRows; index++ {
		entries = append(entries, PublicHistoryEntry{ID: uuid.NewString(), Message: providers.Message{Role: "assistant", Content: "later confirmed append"}})
	}
	change, err := store.replacePublicEntries(key, entries, &meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.writePublicChange(key, change); err != nil {
		t.Fatal(err)
	}
	if err = store.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := store.verifyNativeArchiveAt(t.Context(), key, meta, frame, transition, &proof.Frontier); err != nil {
		t.Fatal("later append consumed pending operation proof budget", err)
	}
	if _, err := store.verifyNativeArchive(t.Context(), key, meta, frame, transition); !errors.Is(err, ErrHistoryCapacity) {
		t.Fatal("control did not exceed current archive budget", err)
	}
}

func TestNativeArchivePendingFrontierSurvivesRestartAndLaterAppends(t *testing.T) {
	store, key, frame := nativeArchiveSource(t)
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.PublicHistoryNonLocalImports = true
	if err := store.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	if err := store.writeContentCheckpoint(key, meta); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(store.publicIndexPath(key))
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
			return err
		}
		if path == store.metaPath(key) {
			writes++
			if writes == 1 {
				return os.WriteFile(store.publicIndexPath(key), []byte("corrupt"), 0600)
			}
		}
		return nil
	}
	transition, err := store.CommitHistoryCompaction(t.Context(), key, nativeArchivePlan(frame))
	store.writeFile = nil
	if transition.Version == 0 || !errors.Is(err, ErrHistoryInputCommitted) {
		t.Fatal("post-commit proof failure was not distinguishable", transition, err)
	}
	if err := os.WriteFile(store.publicIndexPath(key), index, 0600); err != nil {
		t.Fatal(err)
	}
	meta, err = store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.readPublicEntries(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxNativeArchiveRows; i++ {
		entries = append(entries, PublicHistoryEntry{ID: uuid.NewString(), Message: providers.Message{Role: "assistant", Content: "later append"}})
	}
	change, err := store.replacePublicEntries(key, entries, &meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writePublicChange(key, change); err != nil {
		t.Fatal(err)
	}
	if err := store.writeMeta(key, meta); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.PrepareNativeHistoryArchive(t.Context(), key); err != nil {
		t.Fatal("pending frontier was lost across restart", err)
	}
	pending, err := restarted.ReadPendingHistoryCompaction(t.Context(), key)
	if err != nil || pending.Version != 0 {
		t.Fatal("proved pending cut was not acknowledged", pending, err)
	}
}
