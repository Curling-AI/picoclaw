package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

func TestContentLineageCoverageDoesNotRecoverLostOrder(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_coverage_order_loss"
	_, _, ack := pendingCoverageContent(t, s, key, coverageFixtureMessages(), 1)
	before, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, legacyPublicContentPolicy()); err != nil {
		t.Fatal("independent Order loss blocked qualified content coverage", err)
	}
	after, err := s.GetSessionMeta(t.Context(), key)
	if err != nil || after.HistoryStreamID != before.HistoryStreamID ||
		after.HistorySequence != before.HistorySequence ||
		!after.HistoryRecoveryRequired {
		t.Fatal("archive coverage reconstructed or replaced an Order stream", after, err)
	}
	if _, err = s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryReceiptUnavailable) {
		t.Fatal("archive coverage invented receipt eligibility", err)
	}
}

func TestContentLineageCoverageACKRollbackAndLostResponseRemainAtomic(t *testing.T) {
	for _, phase := range []string{"journal", "meta-before", "meta-torn", "meta-after"} {
		t.Run(phase, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_coverage_ack_atomic"
			_, transition, ack := pendingCoverageContent(t, s, key, coverageFixtureMessages(), 1)
			physical, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("coverage acknowledgement publication failed")
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
			ackErr := s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, legacyPublicContentPolicy())
			if phase == "meta-after" {
				if ackErr != nil {
					t.Fatal("own proven NEW confirmation was mislabeled as OLD", ackErr)
				}
			} else if !errors.Is(ackErr, sentinel) {
				t.Fatal("precommit acknowledgement failure lost its cause", ackErr)
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := s.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "meta-after" {
				if meta.ContentIdentity.Pending != nil || meta.ContentIdentity.Last == nil ||
					meta.ContentIdentity.Last.AckDigest == "" {
					t.Fatal("NEW archive confirmation was not durable", meta.ContentIdentity)
				}
			} else if meta.ContentIdentity.Pending == nil || meta.ContentIdentity.Pending.Transition.MutationID != transition.MutationID {
				t.Fatal("failed acknowledgement discarded its pending transition", meta.ContentIdentity)
			}
			if err = s.AcknowledgeHistoryCompactionCoverage(
				t.Context(),
				key,
				ack,
				legacyPublicContentPolicy(),
			); err != nil {
				t.Fatal("same qualified archive transaction could not retry or replay", err)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(after, physical) {
				t.Fatal("acknowledgement failure/recovery discarded physical source", err)
			}
		})
	}
}
