package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentMutationDecidedNEWDoesNotDependOnLostReceiptWitness(t *testing.T) {
	for _, metaAfterError := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal-new", true: "lost-meta-response"}[metaAfterError], func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			before, err := s.readRecoveryMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			plan := typedPrefixPlan(frame, 2)
			fired := false
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if path == s.metaPath(key) && !fired {
					fired = true
					if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
						return removeErr
					}
					if metaAfterError {
						return io.ErrUnexpectedEOF
					}
				}
				return nil
			}
			result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if err != nil || !fired || result.Status != ContentMutationPending {
				t.Fatal("a decided content transition was coupled to unavailable Order", result, err)
			}
			s.writeFile = nil
			found, err := s.ReadHistoryContentMutation(t.Context(), key, plan.MutationID, plan.IntentDigest)
			if err != nil || !reflect.DeepEqual(found, result) {
				t.Fatal("receipt loss hid an already committed content operation", found, err)
			}
			appended, err := s.AppendHistoryContent(
				t.Context(),
				key,
				providers.Message{Role: "user", Content: "new without receipts"},
			)
			if err != nil || appended.Entry.Ref == nil || appended.Receipt != nil ||
				appended.Entry.ReceiptBinding != nil {
				t.Fatal("warm writer reattested lost Order or blocked ordinary content", appended, err)
			}
			after, err := s.readRecoveryMeta(key)
			if err != nil || !after.HistoryRecoveryRequired || after.HistorySequence != before.HistorySequence ||
				after.HistoryStreamID != before.HistoryStreamID {
				t.Fatal("independent proof loss rebuilt a counter/stream", after.HistorySequence, err)
			}
			if _, err = s.ReadHistoryWithReceipts(t.Context(), key); err == nil {
				t.Fatal("content-only mutation/append emitted new receipt eligibility")
			}
		})
	}
}

func TestContentMutationNEWKeepsOperationalWitnessFailuresExplicit(t *testing.T) {
	for _, failure := range []string{"content-witness", "order-recovery-marker"} {
		t.Run(failure, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			plan := typedPrefixPlan(frame, 2)
			fired := false
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if (failure == "content-witness" && path == s.contentDomainPath(key)) ||
					(failure == "order-recovery-marker" && path == s.historyRecoveryRequiredPath(key)) {
					return &os.PathError{Op: "write", Path: path, Err: syscall.EACCES}
				}
				if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
					return writeErr
				}
				if failure == "order-recovery-marker" && path == s.metaPath(key) && !fired {
					fired = true
					if removeErr := os.Remove(s.historyOrderWitnessPath(key)); removeErr != nil {
						return removeErr
					}
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			if _, err := s.CommitHistoryContentMutation(t.Context(), key, plan); !errors.Is(err, syscall.EACCES) {
				t.Fatal("actual/mixed I/O was disguised as successful content proof", err)
			}
			s.writeFile = nil
			replayed, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if err != nil || replayed.Transition.MutationID != plan.MutationID {
				t.Fatal("repairing I/O did not recover the exact existing operation", replayed, err)
			}
			messages, err := s.GetHistory(t.Context(), key)
			if err != nil || len(messages) != 2 {
				t.Fatal("operational failure retry executed the cut twice", messages, err)
			}
		})
	}
}

func TestContentMutationWarmAppendSeesLaterOrderLoss(t *testing.T) {
	s, key, frame := typedMutationSource(t)
	if _, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2)); err != nil {
		t.Fatal(err)
	}
	before, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
		t.Fatal(err)
	}
	// No pure frame getter or receipt call has latched that loss for this warm
	// writer. The append boundary itself must inspect the actual witness.
	appended, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "still durable"},
	)
	if err != nil || appended.Entry.Ref == nil || appended.Receipt != nil || appended.Entry.ReceiptBinding != nil {
		t.Fatal("warm append reattested missing order evidence", appended, err)
	}
	after, err := s.readRecoveryMeta(key)
	if err != nil || !after.HistoryRecoveryRequired || after.HistorySequence != before.HistorySequence ||
		after.HistoryStreamID != before.HistoryStreamID {
		t.Fatal("warm append reset or reconstructed unavailable Order", err)
	}
}

func TestContentMutationCounterRollbackAfterTailGCNeverUsesRetainedMaximum(t *testing.T) {
	for _, cold := range []bool{false, true} {
		for _, lostSemanticField := range []bool{false, true} {
			name := map[bool]string{false: "warm", true: "cold"}[cold] + map[bool]string{false: "/counter-only", true: "/semantic-field-lost"}[lostSemanticField]
			t.Run(name, func(t *testing.T) {
				s, key, frame := typedMutationSource(t)
				pending, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2))
				if err != nil {
					t.Fatal(err)
				}
				if err = s.AcknowledgeHistoryContentMutation(
					t.Context(),
					key,
					typedAppliedAck(pending.Transition),
				); err != nil {
					t.Fatal(err)
				}
				meta, err := s.readRecoveryMeta(key)
				if err != nil || meta.HistorySequence != 4 {
					t.Fatal(err)
				}
				// Retained events carry sequences 1 and 2. A plausible but wrong
				// positive counter 3 cannot be disproved by their maximum.
				meta.HistorySequence = 3
				if lostSemanticField {
					meta.ContentIdentity.MutationVersion = 0
				}
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if cold {
					s, err = NewJSONLStore(s.dir)
					if err != nil {
						t.Fatal(err)
					}
				}
				current, err := s.ReadHistoryContentFrame(t.Context(), key)
				if err != nil || len(current.Entries) != 2 {
					t.Fatal("counter loss hid durable content", current, err)
				}
				for _, entry := range current.Entries {
					if entry.ReceiptBinding != nil {
						t.Fatal("metadata rollback reattested old order", entry)
					}
				}
				marked, markerErr := s.hasHistoryRecoveryMarker(key)
				if markerErr != nil || marked {
					t.Fatal("pure getter wrote a recovery marker", markerErr)
				}
				appended, err := s.AppendHistoryContent(
					t.Context(),
					key,
					providers.Message{Role: "assistant", Content: "after counter loss"},
				)
				if err != nil || appended.Receipt != nil || appended.Entry.ReceiptBinding != nil {
					t.Fatal("append reused a historical discarded sequence", appended, err)
				}
				after, err := s.readRecoveryMeta(key)
				if err != nil || after.HistorySequence != 3 || !after.HistoryRecoveryRequired ||
					after.HistoryStreamID != meta.HistoryStreamID {
					t.Fatal("writer inferred/rebuilt the lost counter", after.HistorySequence, err)
				}
				if _, err = s.ReadHistoryWithReceipts(t.Context(), key); err == nil {
					t.Fatal("receipt API reopened an unproved counter")
				}
			})
		}
	}
}

func TestContentMutationOrderLossClassifierRejectsOperationalAndOwnershipCauses(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		pure bool
	}{
		{"receipt", ErrHistoryReceiptUnavailable, true},
		{"joined-proof", errors.Join(ErrHistoryOrderUnavailable, ErrHistoryRecoveryRequired), true},
		{"mixed-io", errors.Join(ErrHistoryReceiptUnavailable, io.ErrUnexpectedEOF), false},
		{"ownership", ErrHistoryOwnershipUnavailable, false},
		{"mixed-ownership", errors.Join(ErrHistoryReceiptUnavailable, ErrHistoryOwnershipUnavailable), false},
		{"path-wrapper", &os.PathError{Op: "read", Path: "source", Err: ErrHistoryReceiptUnavailable}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := onlyContentMutationOrderLoss(test.err); got != test.pure {
				t.Fatal("wrong all-cause proof classification", got, test.err)
			}
		})
	}
}

func TestContentMutationSourceOLDNEWAndLostDecisionRetry(t *testing.T) {
	for _, phase := range []string{"journal_before", "raw_before", "meta_before", "meta_after", "witness_before", "witness_after", "decision_before"} {
		t.Run(phase, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			plan := typedPrefixPlan(frame, 2)
			originalBody, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			fired := false
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if phase == "decision_before" && path == s.historyDecisionPath(key) {
					return io.ErrClosedPipe
				}
				match := (phase == "journal_before" && path == s.historyRecoveryPath(key)) ||
					(phase == "raw_before" && path == s.jsonlPath(key)) ||
					((phase == "meta_before" || phase == "meta_after" || phase == "decision_before") && path == s.metaPath(key)) ||
					((phase == "witness_before" || phase == "witness_after") && path == s.contentDomainPath(key))
				if !fired && match {
					fired = true
					if phase == "meta_after" || phase == "witness_after" || phase == "decision_before" {
						if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
							return writeErr
						}
					}
					return io.ErrUnexpectedEOF
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			result, commitErr := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if !fired {
				t.Fatal("fault never reached the intended publication boundary")
			}
			old := phase == "journal_before" || phase == "raw_before" || phase == "meta_before"
			uncertain := phase == "decision_before"
			if old || uncertain {
				if commitErr == nil || result.Transition.MutationID != "" {
					t.Fatal("OLD/undecided operation was reported as accepted", result, commitErr)
				}
			} else if commitErr != nil || result.Transition.MutationID != plan.MutationID {
				t.Fatal("confirmed NEW invited a duplicate-producing retry", result, commitErr)
			}
			if old {
				actual, readErr := os.ReadFile(s.jsonlPath(key))
				if readErr != nil || !bytes.Equal(actual, originalBody) {
					t.Fatal("recovered OLD lost the original raw content", readErr)
				}
			}
			if uncertain {
				if _, readErr := s.ReadPendingHistoryContentMutation(
					t.Context(),
					key,
				); !errors.Is(
					readErr,
					ErrHistoryRecoveryRequired,
				) {
					t.Fatal("getter silently settled an undecided journal", readErr)
				}
			}
			s.writeFile = nil
			retried, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
			if err != nil || retried.Status != ContentMutationPending {
				t.Fatal("same admission could not finish/replay safely", retried, err)
			}
			if !old && !uncertain && !reflect.DeepEqual(result, retried) {
				t.Fatal("NEW retry changed the operation identity/pair", result, retried)
			}
			messages, err := s.GetHistory(t.Context(), key)
			if err != nil || len(messages) != 2 {
				t.Fatal("source cut was duplicated or did not settle", messages, err)
			}
			before, err := s.ReadHistoryContentMutationFrame(t.Context(), key, plan.MutationID, ContentBefore)
			if err != nil || len(before.Entries) != 4 {
				t.Fatal("recovery lost the exact overwritten preimage", before, err)
			}
			for _, path := range []string{s.historyRecoveryPath(key), s.historyDecisionPath(key)} {
				if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("successful owning retry left a write fence", path, err)
				}
			}
		})
	}
}

func TestContentMutationACKRecoveryRetainsClosedReplay(t *testing.T) {
	for _, phase := range []string{"meta_before", "meta_after", "witness_before", "decision_before"} {
		t.Run(phase, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			pending, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2))
			if err != nil {
				t.Fatal(err)
			}
			ack := typedAppliedAck(pending.Transition)
			fired := false
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if phase == "decision_before" && path == s.historyDecisionPath(key) {
					return io.ErrClosedPipe
				}
				match := (phase != "witness_before" && path == s.metaPath(key)) ||
					(phase == "witness_before" && path == s.contentDomainPath(key))
				if !fired && match {
					fired = true
					if phase == "meta_after" || phase == "decision_before" {
						if writeErr := fileutil.WriteFileAtomic(path, data, mode); writeErr != nil {
							return writeErr
						}
					}
					return io.ErrUnexpectedEOF
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			ackErr := s.AcknowledgeHistoryContentMutation(t.Context(), key, ack)
			if !fired || ((phase == "meta_before" || phase == "decision_before") != (ackErr != nil)) {
				t.Fatal("wrong ACK OLD/NEW result", fired, ackErr)
			}
			s.writeFile = nil
			if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, ack); err != nil {
				t.Fatal("same exact application could not finish/replay", err)
			}
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			applied, err := reopened.ReadLatestHistoryContentMutation(t.Context(), key)
			if err != nil || applied.Status != ContentMutationApplied || applied.AppliedAck == nil ||
				!reflect.DeepEqual(*applied.AppliedAck, ack) {
				t.Fatal("application recovery changed/lost the exact closed result", applied, err)
			}
		})
	}
}

func TestContentMutationPreimageExclusiveAndReadOnlyGetters(t *testing.T) {
	s, key, frame := typedMutationSource(t)
	plan := typedPrefixPlan(frame, 2)
	data := []byte("write-once private image\n")
	image := contentPreimage{
		Version: 1, Key: key, DomainID: frame.DomainID, MutationID: plan.MutationID,
		ID: uuid.NewString(), Bytes: int64(len(data)), Digest: contentDigest(data),
	}
	if err := s.createContentPreimage(t.Context(), image, data); err != nil {
		t.Fatal(err)
	}
	if err := s.createContentPreimage(t.Context(), image, data); !errors.Is(err, os.ErrExist) {
		t.Fatal("immutable preimage was overwritten on collision", err)
	}
	got, err := s.readContentPreimage(t.Context(), key, frame.DomainID, plan.MutationID, &image)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("exclusive collision changed the owned bytes", err)
	}
	if _, err = s.CommitHistoryContentMutation(t.Context(), key, plan); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		before[entry.Name()], err = os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ReadLatestHistoryContentMutation(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadHistoryContentMutationFrame(t.Context(), key, plan.MutationID, ContentBefore); err != nil {
		t.Fatal(err)
	}
	for name, want := range before {
		got, readErr := os.ReadFile(filepath.Join(s.dir, name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatal("mutation getter changed durable source/proof bytes", name, readErr)
		}
	}
}
