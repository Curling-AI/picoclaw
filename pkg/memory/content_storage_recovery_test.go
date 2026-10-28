package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentStorageIndexOLDTruncatesOnlyItsExactJournalDelta(t *testing.T) {
	for _, alien := range []bool{false, true} {
		t.Run(map[bool]string{false: "own-partial", true: "unowned-suffix"}[alien], func(t *testing.T) {
			s, key, _ := typedMutationSource(t)
			old, err := s.readRecoveryMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			index := old.ContentIdentity.storage.Header.Index
			path := filepath.Join(s.dir, index.File.name())
			indexBefore, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			bodyBefore, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			s.contentStorageFailure = func(name, phase string) error {
				if name == index.File.name() && phase == "file-sync" {
					if alien {
						f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
						if openErr != nil {
							return openErr
						}
						_, writeErr := f.WriteString("foreign complete suffix\n")
						return errors.Join(writeErr, f.Close(), syscall.EIO)
					}
					if truncateErr := os.Truncate(path, index.File.Bytes+20); truncateErr != nil {
						return truncateErr
					}
					return syscall.EIO
				}
				return nil
			}
			input, err := s.AppendHistoryContentInput(
				t.Context(),
				key,
				providers.Message{Role: "assistant", Content: "not decided"},
			)
			if err == nil || errors.Is(err, ErrHistoryInputCommitted) || input.Entry.Message.Content != "" {
				t.Fatal("OLD index failure became accepted input", input, err)
			}
			s.contentStorageFailure = nil
			indexAfter, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if alien {
				if !bytes.HasSuffix(indexAfter, []byte("foreign complete suffix\n")) {
					t.Fatal("rollback deleted unowned index bytes")
				}
				if _, err = os.Stat(s.historyRecoveryPath(key)); err != nil {
					t.Fatal("ambiguous OLD lost its journal", err)
				}
			} else {
				bodyAfter, readErr := os.ReadFile(s.jsonlPath(key))
				if readErr != nil || !bytes.Equal(indexBefore, indexAfter) || !bytes.Equal(bodyBefore, bodyAfter) {
					t.Fatal("own OLD rollback changed its source", readErr)
				}
			}
		})
	}
}

func TestContentStorageNEWProofLossKeepsInputAndFencesIdentityWithoutBlockingLaterWrites(t *testing.T) {
	s, key, before := typedMutationSource(t)
	pending, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(before, 2))
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	index := old.ContentIdentity.storage.Header.Index
	indexPath := filepath.Join(s.dir, index.File.name())
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
			return err
		}
		if path == s.metaPath(key) {
			if err := os.Remove(indexPath); err != nil {
				return err
			}
			return errors.New("lost reply after NEW metadata and identity artifact loss")
		}
		return nil
	}
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "user", Content: "decided exactly once"},
	)
	if !errors.Is(err, ErrHistoryInputCommitted) || !errors.Is(err, ErrContentIdentityUnavailable) ||
		input.Entry.Message.Content != "decided exactly once" || input.Receipt != nil || input.Entry.Ref != nil {
		t.Fatal("NEW proof loss was a generic retry or emitted proof", input, err)
	}
	s.writeFile = nil
	meta, err := s.readRecoveryMeta(key)
	if err != nil || meta.Count != 3 {
		t.Fatal("NEW was rolled back", meta, err)
	}
	floor, present, err := s.readContentDomain(key)
	if err != nil || !present || !floor.Unavailable || floor.Storage == nil ||
		floor.Storage.Pending.MutationID != pending.Transition.MutationID {
		t.Fatal("proof loss lost durable roots", floor, err)
	}
	if _, err = os.Stat(s.historyRecoveryPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pure identity loss blocked the writer", err)
	}
	for _, body := range []string{"later durable one", "later durable two"} {
		input, err = s.AppendHistoryContentInput(t.Context(), key, providers.Message{Role: "assistant", Content: body})
		if err != nil || input.Entry.Message.Content != body || input.Entry.Ref != nil {
			t.Fatal("later content was disabled", input, err)
		}
	}
	after, err := s.readRecoveryMeta(key)
	if err != nil || after.HistorySequence != old.HistorySequence+3 || after.HistoryRecoveryRequired {
		t.Fatal("genuine independent order was rebuilt or unnecessarily lost", after, err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUnavailable || len(frame.Entries) != 5 {
		t.Fatal(frame, err)
	}
	for _, entry := range frame.Entries {
		if entry.Ref != nil {
			t.Fatal("lost index produced a ref")
		}
	}
	if _, err = s.InitializeHistoryContentIdentity(
		t.Context(),
		key,
		frame.Observation,
	); !errors.Is(
		err,
		ErrContentIdentityUnavailable,
	) {
		t.Fatal("proof loss silently opened another domain", err)
	}
	if err = s.AcknowledgeHistoryContentMutation(
		t.Context(),
		key,
		typedAppliedAck(pending.Transition),
	); !errors.Is(
		err,
		ErrContentIdentityUnavailable,
	) {
		t.Fatal("old ACK bypassed missing identity", err)
	}
}

func TestContentStorageMetaRollbackCannotForgetPendingOrLowerDiskFloor(t *testing.T) {
	s, key, before := typedMutationSource(t)
	oldMeta, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	oldBody, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(before)
	transition, err := s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: before.DomainID, BeforeState: before.StateToken,
		Summary: "summary-only new checkpoint", Retained: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	currentFloor, err := os.ReadFile(s.contentDomainPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), oldMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUnavailable {
		t.Fatal("meta rollback forgot a logical Pending", frame, err)
	}
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "preserve after rollback"},
	)
	if err != nil || input.Entry.Ref != nil {
		t.Fatal(input, err)
	}
	floor, _, err := s.readContentDomain(key)
	if err != nil || floor.DiskVersion != contentStorageVersion ||
		floor.Storage.Pending.MutationID != transition.MutationID {
		t.Fatal("ordinary write lowered floor or adopted an orphan", floor, err)
	}
	if err = s.PrepareHistoryContentInlineRollback(t.Context(), key); !errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("unproved rollback was downconverted", err)
	}
	body, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(body, oldBody) {
		t.Fatal("rollback repair discarded physical body", err)
	}
	// The floor may only acquire a permanent unavailable flag; every root is
	// still from the exact NEW checkpoint, rather than the restored metadata.
	var oldFloor contentDomainWitness
	if err = json.Unmarshal(currentFloor, &oldFloor); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(floor.Storage, oldFloor.Storage) || floor.Checkpoint != oldFloor.Checkpoint {
		t.Fatal("rollback replaced current ownership roots")
	}
}
