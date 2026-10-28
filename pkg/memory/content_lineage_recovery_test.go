package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

func TestContentLineageCompactionOLDNEWFailuresRemainAtomic(t *testing.T) {
	for _, phase := range []string{"journal", "meta-before", "meta-torn", "meta-after"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_content_atomic"
			for range 4 {
				if err = s.AddMessage(t.Context(), key, "user", "duplicate"); err != nil {
					t.Fatal(err)
				}
			}
			before := initializedContent(t, s, key)
			refs := frameRefs(before)
			bytesBefore, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			plan := ContentCompactionPlan{
				MutationID:    uuid.NewString(),
				DomainID:      before.DomainID,
				BeforeState:   before.StateToken,
				SummaryInputs: refs[:2],
				Retained:      refs[2:],
				Summary:       "new summary",
			}
			sentinel := errors.New("injected content checkpoint phase failure")
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if phase == "journal" && path == s.historyRecoveryPath(key) {
					return sentinel
				}
				if path == s.metaPath(key) {
					switch phase {
					case "meta-before":
						return sentinel
					case "meta-torn":
						return errors.Join(sentinel, os.WriteFile(path, []byte(`{"content_identity":`), mode))
					case "meta-after":
						return errors.Join(sentinel, fileutil.WriteFileAtomic(path, data, mode))
					}
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			transition, operationErr := s.CommitHistoryCompaction(t.Context(), key, plan)
			if phase != "meta-after" && !errors.Is(operationErr, sentinel) {
				t.Fatal("pre-commit phase failure was hidden", operationErr)
			}
			if phase == "meta-after" && (operationErr != nil || transition.MutationID != plan.MutationID) {
				t.Fatal("durable NEW was reported as retryable OLD", transition, operationErr)
			}
			s, err = NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			after, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "meta-after" {
				if after.Summary != plan.Summary || after.StateToken != transition.AfterState ||
					!reflect.DeepEqual(frameRefs(after), refs[2:]) {
					t.Fatal("NEW mixed summary/window/identity", after)
				}
				replayed, replayErr := s.CommitHistoryCompaction(t.Context(), key, plan)
				if replayErr != nil || !reflect.DeepEqual(replayed, transition) {
					t.Fatal("replay created a new compaction", replayed, transition, replayErr)
				}
			} else if after.Summary != before.Summary || after.StateToken != before.StateToken || !reflect.DeepEqual(frameRefs(after), refs) {
				t.Fatal("OLD mixed summary/window/identity", after)
			}
			bytesAfter, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(bytesBefore, bytesAfter) {
				t.Fatal("unacknowledged compaction rewrote physical content", err)
			}
		})
	}
}

func TestContentLineageExpiredRetentionPreservesRowsAndAcceptsQualifiedRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_retention_expired"
	_, _, transition := pendingContent(t, s, key)
	meta, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentIdentity.Pending.Transition.ExpiresAt = time.Now().Add(-time.Second)
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.ReadPendingHistoryCompaction(t.Context(), key)
	if !errors.Is(err, ErrContentLineageExpired) || pending.MutationID != transition.MutationID {
		t.Fatal("expired pending operation became silent or disappeared", pending, err)
	}
	if err = s.Compact(t.Context(), key); err != nil {
		t.Fatal("expired physical GC blocked ordinary Save", err)
	}
	if err = s.AddMessage(t.Context(), key, "assistant", "late durable content"); err != nil {
		t.Fatal("expired retention blocked ordinary append", err)
	}
	retained, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(retained, before) {
		t.Fatal("expiration discarded the original source", err)
	}
	ack := ContentArchiveAck{
		DomainID:        transition.DomainID,
		MutationID:      transition.MutationID,
		BeforeState:     transition.BeforeState,
		AfterState:      transition.AfterState,
		Archived:        transition.Dropped,
		ArchiveCommitID: uuid.NewString(),
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ack); err != nil {
		t.Fatal("explicit qualified recovery could not release retention", err)
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ack); err != nil {
		t.Fatal("lost ACK response could not be replayed", err)
	}
	if err = s.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	after, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || after.IdentityStatus != ContentIdentityKnown || len(after.Entries) != 3 ||
		after.Entries[2].Message.Content != "late durable content" {
		t.Fatal("qualified GC lost retained identity or the concurrent append", after, err)
	}
}

func TestContentLineageBaselineWitnessFailureRecoversOnlyThroughOwningJournal(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_witness_failure"
	if err = s.AddMessage(t.Context(), key, "assistant", "durable"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected torn content witness")
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.contentDomainPath(key) {
			return errors.Join(sentinel, os.WriteFile(path, []byte(`{"version":`), mode))
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	if _, err = s.InitializeHistoryContentIdentity(t.Context(), key, before.Observation); !errors.Is(err, sentinel) {
		t.Fatal("witness failure did not retain its cause", err)
	}
	if _, err = s.ReadHistoryContentFrame(t.Context(), key); !errors.Is(err, ErrHistoryRecoveryRequired) {
		t.Fatal("getter repaired a pending witness publication", err)
	}
	s, err = NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || after.IdentityStatus != ContentIdentityKnown || len(after.Entries) != 1 ||
		after.Entries[0].Message.Content != "durable" {
		t.Fatal("own NEW journal could not finish identity publication", after, err)
	}
	if err = os.Remove(s.contentDomainPath(key)); err != nil {
		t.Fatal(err)
	}
	lost, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || lost.IdentityStatus != ContentIdentityUnavailable || lost.DomainID != "" {
		t.Fatal("missing identity outside a journal was repaired automatically", lost, err)
	}
}
