package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentStorageHotInputKeepsVectorsOutsideMetadataAndJournal(t *testing.T) {
	s, key, before := typedMutationSource(t)
	plan := typedPrefixPlan(before, 2)
	pending, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil || meta.ContentIdentity.storage == nil || meta.ContentIdentity.storage.Header.Pending == nil {
		t.Fatal("source did not write its versioned storage header", meta, err)
	}
	header := meta.ContentIdentity.storage.Header
	pendingBytes, err := os.ReadFile(filepath.Join(s.dir, header.Pending.File.name()))
	if err != nil {
		t.Fatal(err)
	}
	var journalSizes []int
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyRecoveryPath(key) {
			journalSizes = append(journalSizes, len(data))
			if bytes.Contains(data, []byte(`"rows":`)) || bytes.Contains(data, []byte(`"transition":`)) {
				t.Fatal("ordinary WAL duplicated an expanded identity vector")
			}
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	s.contentStorageFailure = func(name, phase string) error {
		if phase == "read" {
			return &os.PathError{Op: "forbidden hot hydration", Path: name, Err: syscall.EIO}
		}
		return nil
	}
	for i := range 24 {
		input, appendErr := s.AppendHistoryContentInput(t.Context(), key,
			providers.Message{Role: "assistant", Content: strings.Repeat("body", i+1)})
		if appendErr != nil || input.Entry.Ref != nil || input.DomainID != "" {
			t.Fatal("ordinary input hydrated or claimed the prefix", i, input, appendErr)
		}
	}
	if len(journalSizes) != 24 || journalSizes[len(journalSizes)-1] > journalSizes[0]+160 {
		t.Fatal("WAL grew with prior vectors", journalSizes)
	}
	current, err := s.readRecoveryMeta(key)
	if err != nil || current.ContentIdentity.storage.Header.Pending.File != header.Pending.File ||
		current.ContentIdentity.storage.Header.Index.File.ID != header.Index.File.ID ||
		current.ContentIdentity.storage.Header.Index.Records != header.Index.Records+24 {
		t.Fatal("hot append rewrote immutable Pending or changed the index generation", current, err)
	}
	afterPending, err := os.ReadFile(filepath.Join(s.dir, header.Pending.File.name()))
	if err != nil || !bytes.Equal(pendingBytes, afterPending) {
		t.Fatal("hot append rewrote frozen Pending payload", err)
	}
	if _, err = s.ReadHistoryContentFrame(t.Context(), key); !errors.Is(err, syscall.EIO) {
		t.Fatal("explicit strict read did not hydrate the index/slots", err)
	}
	s.contentStorageFailure = nil
	s.writeFile = nil
	for _, side := range []ContentTransitionSide{ContentBefore, ContentAfter} {
		frame, readErr := s.ReadHistoryContentMutationFrame(t.Context(), key, plan.MutationID, side)
		count := 4
		if side == ContentAfter {
			count = 2
		}
		if readErr != nil || len(frame.Entries) != count {
			t.Fatal("hot appends changed a frozen pair", side, frame, readErr)
		}
	}
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, typedAppliedAck(pending.Transition)); err != nil {
		t.Fatal(err)
	}
}

func TestContentStorageMissingIndexPreservesOrdinaryInputWithoutReinitializing(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	index := meta.ContentIdentity.storage.Header.Index
	if err = os.Remove(filepath.Join(s.dir, index.File.name())); err != nil {
		t.Fatal(err)
	}
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "keep new durable body"},
	)
	if err != nil || input.Entry.Message.Content != "keep new durable body" || input.Entry.Ref != nil {
		t.Fatal("optional proof loss disabled ordinary input", input, err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(frame.Entries) != 5 || frame.IdentityStatus != ContentIdentityUnavailable {
		t.Fatal("missing index was silently recreated or hid body", frame, err)
	}
	if _, err = os.Lstat(filepath.Join(s.dir, index.File.name())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ordinary append reconstructed the missing index", err)
	}
	if _, err = s.InitializeHistoryContentIdentity(
		t.Context(),
		key,
		frame.Observation,
	); !errors.Is(
		err,
		ErrContentIdentityUnavailable,
	) {
		t.Fatal("proof loss became another domain", err)
	}
}

func TestContentStorageIndexTamperIsCheckedOnlyByQualifiedProof(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	index := meta.ContentIdentity.storage.Header.Index
	path := filepath.Join(s.dir, index.File.name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := meta.ContentIdentity.storage.Header.Index.File.DomainID
	changed := bytes.Replace(data, []byte(old), []byte(uuid.NewString()), 1)
	if len(data) != len(changed) || bytes.Equal(data, changed) {
		t.Fatal("not a same-size earlier index edit")
	}
	if err = os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "new input"},
	)
	if err != nil || input.Entry.Ref != nil || input.DomainID != "" {
		t.Fatal("ordinary input was qualified without a scan", input, err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUnavailable || len(frame.Entries) != 5 {
		t.Fatal("strict proof accepted an edited index prefix", frame, err)
	}
	if err = s.TruncateHistory(t.Context(), key, 1); !errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("a cut bypassed strict index proof", err)
	}
}

func TestContentStorageAppliedReplayDoesNotReadRetiredRawOrIndex(t *testing.T) {
	s, key, before := typedMutationSource(t)
	plan := typedPrefixPlan(before, 1)
	pending, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
	if err != nil {
		t.Fatal(err)
	}
	ack := typedAppliedAck(pending.Transition)
	if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err != nil {
		t.Fatal(err)
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(s.dir, meta.ContentIdentity.storage.Header.Index.File.name())); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.jsonlPath(key)); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := s.ReadLatestHistoryContentMutation(t.Context(), key)
	if err != nil || applied.Status != ContentMutationApplied || applied.AppliedAck == nil ||
		!reflect.DeepEqual(*applied.AppliedAck, ack) {
		t.Fatal("closed Last depended on retired source data", applied, err)
	}
}

func TestContentStorageUnknownFormatPreservesOpaqueDescriptorsOnOrdinaryWrite(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	data, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]json.RawMessage
	if err = json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	var identity map[string]any
	if err = json.Unmarshal(meta["content_identity"], &identity); err != nil {
		t.Fatal(err)
	}
	identity["diskVersion"] = float64(99)
	identity["futureIndex"] = map[string]any{"descriptor": "never erase this"}
	meta["content_identity"], _ = json.Marshal(identity)
	data, _ = json.Marshal(meta)
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "new body"},
	); err != nil {
		t.Fatal("unknown proof format disabled content", err)
	}
	data, err = os.ReadFile(s.metaPath(key))
	if err != nil || json.Unmarshal(data, &meta) != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if json.Unmarshal(meta["content_identity"], &after) != nil || !reflect.DeepEqual(identity, after) {
		t.Fatal("ordinary write discarded unknown descriptors", identity, after)
	}
	if err = s.DeleteSession(t.Context(), key); !errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("unknown ownership roots were destroyed", err)
	}
}

func TestContentStorageUnverifiedIndexCountCannotDriveAllocation(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentIdentity.storage.Header.Index.Count = int(^uint(0) >> 2)
	meta.ContentIdentity.storage.Header.Index.Records = meta.ContentIdentity.storage.Header.Index.Count
	meta.ContentIdentity.storage.Header.Index.Start = 0
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	witness, err := os.ReadFile(s.contentDomainPath(key))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if value := recover(); value != nil {
			t.Fatalf("unproved index count caused allocation panic: %v", value)
		}
	}()
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUnavailable || len(frame.Entries) != 4 {
		t.Fatal("corrupt optional index granted proof or hid durable content", frame, err)
	}
	after, err := os.ReadFile(s.metaPath(key))
	floorAfter, floorErr := os.ReadFile(s.contentDomainPath(key))
	if err != nil || floorErr != nil || !bytes.Equal(data, after) || !bytes.Equal(witness, floorAfter) {
		t.Fatal("getter repaired corrupt index metadata", err, floorErr)
	}
}
