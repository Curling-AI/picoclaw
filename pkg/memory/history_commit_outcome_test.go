package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentMutationMatchingMetadataRetriesDurability(t *testing.T) {
	for _, stage := range []string{"file", "directory"} {
		for _, persistent := range []bool{false, true} {
			name := stage + map[bool]string{false: "/transient", true: "/persistent"}[persistent]
			t.Run(name, func(t *testing.T) {
				s, key, before := typedMutationSource(t)
				plan := typedPrefixPlan(before, 2)
				calls := 0
				s.historyMetadataSyncFailure = func(path, phase string) error {
					if phase == stage {
						calls++
						if persistent || calls == 1 {
							return &os.PathError{Op: "sync metadata " + phase, Path: path, Err: syscall.EIO}
						}
					}
					return nil
				}
				s.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if path == s.metaPath(key) {
						if err := os.WriteFile(path, data, mode); err != nil {
							return err
						}
						return s.syncHistoryMetadata(key)
					}
					return fileutil.WriteFileAtomic(path, data, mode)
				}
				result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
				if calls < 2 || (persistent && !errors.Is(err, syscall.EIO)) || (!persistent && err != nil) {
					t.Fatal("readable NEW metadata replaced its required durability phase", calls, result, err)
				}
				if persistent {
					if _, readErr := s.ReadLatestHistoryContentMutation(
						t.Context(),
						key,
					); !errors.Is(
						readErr,
						ErrHistoryRecoveryRequired,
					) {
						t.Fatal("persistent metadata sync discarded the journal", readErr)
					}
				}
				s.historyMetadataSyncFailure, s.writeFile = nil, nil
				replayed, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
				if err != nil || replayed.Transition.MutationID != plan.MutationID {
					t.Fatal("same command did not settle its own NEW metadata", replayed, err)
				}
				history, err := s.GetHistory(t.Context(), key)
				if err != nil || len(history) != 2 {
					t.Fatal("recovery truncated or duplicated source a second time", history, err)
				}
			})
		}
	}
}

func TestContentMutationMetadataDirectFallbackCannotSkipSync(t *testing.T) {
	s, key, before := typedMutationSource(t)
	plan := typedPrefixPlan(before, 2)
	calls := 0
	s.historyMetadataSyncFailure = func(path, phase string) error {
		if phase == "directory" {
			calls++
			return &os.PathError{Op: "sync metadata directory", Path: path, Err: syscall.EIO}
		}
		return nil
	}
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.metaPath(key) {
			// The shared direct-write fallback can report success without either
			// durability barrier. The opted-in writer must impose its own.
			return os.WriteFile(path, data, mode)
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	if _, err := s.CommitHistoryContentMutation(t.Context(), key, plan); !errors.Is(err, syscall.EIO) || calls < 2 {
		t.Fatal("direct fallback bypassed source metadata durability", calls, err)
	}
	if _, err := s.ReadLatestHistoryContentMutation(t.Context(), key); !errors.Is(err, ErrHistoryRecoveryRequired) {
		t.Fatal("failed directory sync retired recovery", err)
	}
	s.writeFile, s.historyMetadataSyncFailure = nil, nil
	result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
	if err != nil || result.Transition.MutationID != plan.MutationID {
		t.Fatal("sync repair did not replay the original command", result, err)
	}
}

func TestContentMutationAppendNEWKeepsOperationalErrorAndAcceptedBody(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "typed"}[typed], func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			pending, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 4))
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
			s.contentCheckpointSyncFailure = func(path, _ string) error {
				return &os.PathError{Op: "sync committed checkpoint", Path: path, Err: syscall.EIO}
			}
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if path == s.contentDomainPath(key) {
					if writeErr := os.WriteFile(path, data, mode); writeErr != nil {
						return writeErr
					}
					return s.syncContentCheckpoint(key)
				}
				return fileutil.WriteFileAtomic(path, data, mode)
			}
			message := providers.Message{
				Role:     "assistant",
				Content:  "the one accepted body",
				Delivery: &providers.Delivery{Origin: "cron:committed"},
			}
			var result ContentAppendResult
			if typed {
				result, err = s.AppendHistoryContent(t.Context(), key, message)
				if result.Entry.Message.Content != message.Content || result.Entry.Ref != nil ||
					result.Entry.ReceiptBinding != nil || result.Receipt != nil || result.AfterState != "" {
					t.Fatal("confirmed input was lost or falsely attested", result)
				}
			} else {
				err = s.AddFullMessage(t.Context(), key, message)
			}
			if !errors.Is(err, syscall.EIO) || !errors.Is(err, ErrHistoryInputCommitted) ||
				errors.Is(err, ErrHistoryReceiptUnavailable) {
				t.Fatal("input NEW was hidden or relabeled as retryable pre-write failure", err)
			}
			meta, readErr := s.readRecoveryMeta(key)
			if readErr != nil || meta.Count != 5 {
				t.Fatal("input was not NEW", meta, readErr)
			}
			data, readErr := os.ReadFile(s.jsonlPath(key))
			if readErr != nil || bytes.Count(data, []byte(message.Content)) != 1 {
				t.Fatal("operational error lost or repeated the accepted input", string(data), readErr)
			}
			var accepted providers.Message
			rows := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
			if decodeErr := json.Unmarshal(rows[len(rows)-1], &accepted); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			acceptedID := deliveryID(t, accepted)
			if !validDeliveryID(acceptedID) || (typed && deliveryID(t, result.Entry.Message) != acceptedID) {
				t.Fatal("confirmed NEW lost its durable delivery identity", acceptedID, result)
			}
			if _, readErr = s.ReadHistoryContentFrame(
				t.Context(),
				key,
			); !errors.Is(
				readErr,
				ErrHistoryRecoveryRequired,
			) {
				t.Fatal("unfinished recovery emitted a new frame", readErr)
			}
			s.writeFile, s.contentCheckpointSyncFailure = nil, nil
			if _, readErr = s.readMeta(key); readErr != nil {
				t.Fatal(readErr)
			}
			history, readErr := s.GetHistory(t.Context(), key)
			if readErr != nil || len(history) != 5 || history[4].Content != message.Content ||
				deliveryID(t, history[4]) != acceptedID {
				t.Fatal("repair did not retain the single committed input", history, readErr)
			}
		})
	}
}
