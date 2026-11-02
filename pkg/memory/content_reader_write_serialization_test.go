package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentReadersWaitForTheirOwnActiveAppend(t *testing.T) {
	for _, operation := range []struct {
		name string
		read func(context.Context, *JSONLStore, string) error
	}{
		{"frame", func(ctx context.Context, s *JSONLStore, key string) error {
			frame, err := s.ReadHistoryContentFrame(ctx, key)
			if err == nil && (frame.IdentityStatus != ContentIdentityKnown || len(frame.Entries) != 5) {
				return fmt.Errorf("reader did not observe the complete append: %+v", frame)
			}
			return err
		}},
		{"initialization", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.InitializeHistoryContentIdentity(ctx, key, "already initialized")
			return err
		}},
		{"pending-mutation", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.ReadPendingHistoryContentMutation(ctx, key)
			return err
		}},
		{"latest-mutation", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.ReadLatestHistoryContentMutation(ctx, key)
			return err
		}},
		{"pending-compaction", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.ReadPendingHistoryCompaction(ctx, key)
			return err
		}},
		{"mutation-frame", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.ReadHistoryContentMutationFrame(ctx, key, "not retained", ContentAfter)
			if errors.Is(err, ErrContentMutationNotRetained) {
				return nil
			}
			return fmt.Errorf("absent mutation frame should still be rejected: %w", err)
		}},
		{"compaction-frame", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.ReadHistoryCompactionFrame(ctx, key, "not retained", ContentAfter)
			if errors.Is(err, ErrContentArchiveAck) {
				return nil
			}
			return fmt.Errorf("absent compaction frame should still be rejected: %w", err)
		}},
		{"legacy-commit", func(ctx context.Context, s *JSONLStore, key string) error {
			_, err := s.CommitHistoryCompaction(ctx, key, ContentCompactionPlan{})
			if errors.Is(err, ErrContentStateChanged) {
				return nil
			}
			return fmt.Errorf("invalid plan should still be rejected after waiting: %w", err)
		}},
		{"legacy-ack", func(ctx context.Context, s *JSONLStore, key string) error {
			err := s.AcknowledgeHistoryCompaction(ctx, key, ContentArchiveAck{})
			if errors.Is(err, ErrContentArchiveAck) {
				return nil
			}
			return fmt.Errorf("invalid ACK should still be rejected after waiting: %w", err)
		}},
		{"typed-ack", func(ctx context.Context, s *JSONLStore, key string) error {
			err := s.AcknowledgeHistoryContentMutation(ctx, key, ContentMutationAppliedAck{})
			if errors.Is(err, ErrContentMutationAck) {
				return nil
			}
			return fmt.Errorf("invalid applied ACK should still be rejected after waiting: %w", err)
		}},
		{"coverage-ack", func(ctx context.Context, s *JSONLStore, key string) error {
			ack := ContentArchiveCoverageAck{CoverageVersion: ContentArchiveCoverageVersion, PolicyID: "local"}
			policy := ContentArchivePolicy{ID: "local", Classify: func(ContentEntry) (ContentArchiveExclusionReason, error) {
				return "", nil
			}}
			err := s.AcknowledgeHistoryCompactionCoverage(ctx, key, ack, policy)
			if errors.Is(err, ErrContentArchiveAck) {
				return nil
			}
			return fmt.Errorf("invalid coverage ACK should still be rejected after waiting: %w", err)
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			s, key, _ := typedMutationSource(t)
			journalWritten := make(chan struct{})
			releaseAppend := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(releaseAppend) }) }
			defer release()
			s.writeFile = func(path string, data []byte, mode os.FileMode) error {
				if err := fileutil.WriteFileAtomic(path, data, mode); err != nil {
					return err
				}
				if path == s.historyRecoveryPath(key) {
					if err := s.syncHistoryFile(path); err != nil {
						return err
					}
					close(journalWritten)
					<-releaseAppend
				}
				return nil
			}
			appendDone := make(chan error, 1)
			go func() {
				_, err := s.AppendHistoryContentInput(t.Context(), key, providers.Message{
					Role: "assistant", Content: "append owns its journal and source lock",
				})
				appendDone <- err
			}()
			<-journalWritten
			readerStarted, readerDone := make(chan struct{}), make(chan error, 1)
			go func() {
				close(readerStarted)
				readerDone <- operation.read(t.Context(), s, key)
			}()
			<-readerStarted
			select {
			case err := <-readerDone:
				release()
				<-appendDone
				t.Fatalf("reader bypassed its writer lock and inspected a transient journal: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			release()
			if err := <-appendDone; err != nil {
				t.Fatal(err)
			}
			if err := <-readerDone; err != nil {
				t.Fatal("settled reader rejected its owning append", err)
			}
			// A genuinely unresolved journal must still refuse proof. Waiting for
			// the owning writer does not authorize recovery or mutate a getter.
			s.writeFile = nil
			if err := os.WriteFile(s.historyRecoveryPath(key), []byte("unresolved"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadHistoryContentFrame(t.Context(), key); !errors.Is(err, ErrHistoryRecoveryRequired) {
				t.Fatal("unresolved journal no longer fences the getter", err)
			}
		})
	}
}
