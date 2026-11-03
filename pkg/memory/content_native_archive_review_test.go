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
	if operationErr := store.AddMessage(
		t.Context(),
		key,
		"user",
		"[System: async:spawn] private result",
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.AddMessage(t.Context(), key, "user", "next prompt"); operationErr != nil {
		t.Fatal(operationErr)
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Summary:       "summary",
		SummaryInputs: refs[:5],
		Retained:      refs[5:],
	}
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
	if operationErr := store.ClearHistory(t.Context(), key); operationErr != nil {
		t.Fatal(operationErr)
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.ContentIdentity.ArchivePolicy != nativePublicArchivePolicy {
		t.Fatal("clear lost native archive policy", err)
	}
	for _, role := range []string{"user", "assistant", "user", "assistant"} {
		if operationErr := store.AddMessage(t.Context(), key, role, "new domain"); operationErr != nil {
			t.Fatal(operationErr)
		}
	}
	frame, err := store.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, operationErr := store.CommitHistoryCompaction(
		t.Context(),
		key,
		nativeArchivePlan(frame),
	); operationErr != nil {
		t.Fatal(operationErr)
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
			meta, err := store.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				err = os.Remove(store.contentDomainPath(key))
			case "unreadable":
				err = os.WriteFile(store.contentDomainPath(key), []byte("invalid"), 0o600)
			case "unavailable":
				err = store.writeContentCheckpointAvailability(key, meta, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			history, err := store.ReadPublicHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			anchor := history.Entries[1]
			_, err = store.ImportPublicHistory(
				t.Context(),
				PublicHistoryImport{
					Key:        key,
					Generation: history.Generation,
					Revision:   history.Revision,
					BeforeID:   anchor.ID,
					Entries: []PublicHistoryEntry{
						{
							ID:          uuid.NewString(),
							ImportID:    "legacy:lost-witness",
							AnchorID:    anchor.ID,
							AnchorRef:   anchor.Ref,
							AnchorOrder: anchor.Order,
							Message:     providers.Message{Role: "assistant", Content: "import"},
						},
					},
				},
			)
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
			if appendErr := store.AddMessage(
				t.Context(),
				key,
				"user",
				"allowed append without content proof",
			); appendErr != nil {
				t.Fatal("proof refusal blocked ordinary append", appendErr)
			}
			after, readErr := store.ReadHistoryContentFrame(t.Context(), key)
			if readErr != nil || after.IdentityStatus != ContentIdentityUnavailable ||
				len(after.Entries) != len(frame.Entries)+1 {
				t.Fatal("ordinary append reattested or lost degraded source", after, readErr)
			}
		})
	}
}

func TestNativeContentInitializationDoesNotConfuseDottedKeys(t *testing.T) {
	store := newTestStore(t)
	key := "sk_v1_new"
	if operationErr := os.WriteFile(
		store.jsonlPath(key+".other"),
		[]byte("other session"),
		0o600,
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.InitializeNewHistoryContentIdentity(t.Context(), key); operationErr != nil {
		t.Fatal(operationErr)
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.ContentIdentity == nil || meta.ContentIdentity.ArchivePolicy != nativePublicArchivePolicy {
		t.Fatal("neighboring dotted key blocked initialization", err)
	}
}

func TestNativeArchiveRecoveryUsesItsCommittedPublicFrontier(t *testing.T) {
	store, key, frame := nativeArchiveSource(t)
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.PublicHistoryNonLocalImports = true
	if operationErr := store.writeMeta(key, meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.writeContentCheckpoint(key, meta); operationErr != nil {
		t.Fatal(operationErr)
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
		entries = append(
			entries,
			PublicHistoryEntry{
				ID:      uuid.NewString(),
				Message: providers.Message{Role: "assistant", Content: "later confirmed append"},
			},
		)
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
	if _, err := store.verifyNativeArchive(
		t.Context(),
		key,
		meta,
		frame,
		transition,
	); !errors.Is(
		err,
		ErrHistoryCapacity,
	) {
		t.Fatal("control did not exceed current archive budget", err)
	}
}

func TestNativeArchivePendingFrontierSurvivesRestartAndLaterAppends(t *testing.T) {
	store, key, frame := nativeArchiveSource(t)
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	first := history.Entries[0]
	replay := PublicHistoryImport{
		Key:        key,
		Generation: history.Generation,
		Revision:   history.Revision,
		BeforeID:   first.ID,
		Entries: []PublicHistoryEntry{
			{
				ID:          uuid.NewString(),
				ImportID:    "legacy:before-compaction",
				AnchorID:    first.ID,
				AnchorRef:   first.Ref,
				AnchorOrder: first.Order,
				Message:     providers.Message{Role: "assistant", Content: "prior import"},
			},
		},
	}
	if _, importErr := store.ImportPublicHistory(t.Context(), replay); importErr != nil {
		t.Fatal(importErr)
	}
	index, err := os.ReadFile(store.publicIndexPath(key))
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
			return writeErr
		}
		if path == store.metaPath(key) {
			writes++
			if writes == 1 {
				return os.WriteFile(store.publicIndexPath(key), []byte("corrupt"), 0o600)
			}
		}
		return nil
	}
	transition, err := store.CommitHistoryCompaction(t.Context(), key, nativeArchivePlan(frame))
	store.writeFile = nil
	if transition.Version == 0 || !errors.Is(err, ErrHistoryInputCommitted) {
		t.Fatal("post-commit proof failure was not distinguishable", transition, err)
	}
	if operationErr := os.WriteFile(store.publicIndexPath(key), index, 0o600); operationErr != nil {
		t.Fatal(operationErr)
	}
	if appendErr := store.AddMessage(
		t.Context(),
		key,
		"user",
		"ordinary append during pending archive",
	); appendErr != nil {
		t.Fatal("native pending archive blocked ordinary append", appendErr)
	}
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.readPublicEntries(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, replayErr := store.ImportPublicHistory(t.Context(), replay); replayErr != nil {
		t.Fatal("pending archive refused unchanged import retry", replayErr)
	}
	anchor := entries[0]
	_, importErr := store.ImportPublicHistory(
		t.Context(),
		PublicHistoryImport{
			Key:        key,
			Generation: meta.PublicHistoryGeneration,
			Revision:   meta.PublicHistoryRevision,
			BeforeID:   anchor.ID,
			Entries: []PublicHistoryEntry{
				{
					ID:          uuid.NewString(),
					ImportID:    "legacy:pending-view",
					AnchorID:    anchor.ID,
					AnchorRef:   anchor.Ref,
					AnchorOrder: anchor.Order,
					Message:     providers.Message{Role: "assistant", Content: "late import"},
				},
			},
		},
	)
	if !errors.Is(importErr, ErrContentLineagePending) {
		t.Fatal("new import invalidated the committed archive frontier", importErr)
	}
	for i := 0; i < maxNativeArchiveRows; i++ {
		entries = append(
			entries,
			PublicHistoryEntry{
				ID:      uuid.NewString(),
				Message: providers.Message{Role: "assistant", Content: "later append"},
			},
		)
	}
	change, err := store.replacePublicEntries(key, entries, &meta)
	if err != nil {
		t.Fatal(err)
	}
	if operationErr := store.writePublicChange(key, change); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.writeMeta(key, meta); operationErr != nil {
		t.Fatal(operationErr)
	}
	restarted, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if prepareErr := restarted.PrepareNativeHistoryArchive(t.Context(), key); prepareErr != nil {
		t.Fatal("pending frontier was lost across restart", prepareErr)
	}
	pending, err := restarted.ReadPendingHistoryCompaction(t.Context(), key)
	if err != nil || pending.Version != 0 {
		t.Fatal("proved pending cut was not acknowledged", pending, err)
	}
}

func TestNativeArchiveClearRetiresItsOldImportFence(t *testing.T) {
	store, key, _ := nativeArchiveSource(t)
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	anchor := history.Entries[0]
	_, err = store.ImportPublicHistory(
		t.Context(),
		PublicHistoryImport{
			Key:        key,
			Generation: history.Generation,
			Revision:   history.Revision,
			BeforeID:   anchor.ID,
			Entries: []PublicHistoryEntry{
				{
					ID:          uuid.NewString(),
					ImportID:    "legacy:old-domain",
					AnchorID:    anchor.ID,
					AnchorRef:   anchor.Ref,
					AnchorOrder: anchor.Order,
					Message:     providers.Message{Role: "assistant", Content: "old import"},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if clearErr := store.ClearHistory(t.Context(), key); clearErr != nil {
		t.Fatal(clearErr)
	}
	meta, err := store.readMeta(key)
	if err != nil || meta.PublicHistoryNonLocalImports || meta.PublicHistoryCount != 0 ||
		meta.ContentIdentity.ArchivePolicy != nativePublicArchivePolicy {
		t.Fatal(
			"clear retained the old domain import fence or lost archive policy",
			meta.PublicHistoryNonLocalImports,
			err,
		)
	}
}
