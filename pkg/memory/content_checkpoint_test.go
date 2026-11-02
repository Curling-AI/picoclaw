package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

func TestContentMutationMatchingWitnessRetriesFileAndDirectorySync(t *testing.T) {
	for _, stage := range []string{"file", "directory"} {
		for _, persistent := range []bool{false, true} {
			name := stage + map[bool]string{false: "/transient", true: "/persistent"}[persistent]
			t.Run(name, func(t *testing.T) {
				s, key, frame := typedMutationSource(t)
				plan := typedPrefixPlan(frame, 2)
				calls := 0
				s.contentCheckpointSyncFailure = func(path, currentStage string) error {
					if currentStage == stage {
						calls++
						if persistent || calls == 1 {
							return &os.PathError{Op: "sync " + stage, Path: path, Err: syscall.EIO}
						}
					}
					return nil
				}
				s.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if path == s.contentDomainPath(key) {
						// Publish legible NEW bytes without the required sync phase.
						// Recovery must not equate equality with durability.
						if writeErr := os.WriteFile(path, data, mode); writeErr != nil {
							return writeErr
						}
						return s.syncContentCheckpoint(key)
					}
					return fileutil.WriteFileAtomic(path, data, mode)
				}
				result, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
				if calls < 2 || (persistent && !errors.Is(err, syscall.EIO)) || (!persistent && err != nil) {
					t.Fatal("NEW witness was accepted without completed sync recovery", calls, result, err)
				}
				if persistent {
					if _, readErr := s.ReadPendingHistoryContentMutation(
						t.Context(),
						key,
					); !errors.Is(
						readErr,
						ErrHistoryRecoveryRequired,
					) {
						t.Fatal("persistent sync failure discarded its journal fence", readErr)
					}
				}
				s.contentCheckpointSyncFailure, s.writeFile = nil, nil
				replayed, err := s.CommitHistoryContentMutation(t.Context(), key, plan)
				if err != nil || replayed.Transition.MutationID != plan.MutationID {
					t.Fatal("actual sync repair could not recover the exact operation", replayed, err)
				}
			})
		}
	}
}

func TestContentMutationCheckpointRejectsLinksWithoutChangingTheirTarget(t *testing.T) {
	for _, kind := range []string{"symbolic", "hard"} {
		t.Run(kind, func(t *testing.T) {
			s, key, frame := typedMutationSource(t)
			meta, err := s.readRecoveryMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			path := s.contentDomainPath(key)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "unique-owned-by-someone-else")
			if err = os.WriteFile(other, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if kind == "symbolic" {
				err = os.Symlink(other, path)
			} else {
				err = os.Link(other, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2)); err == nil {
				t.Fatal("typed mutation trusted an unsafe checkpoint target")
			}
			if err = s.writeContentCheckpoint(key, meta); !errors.Is(err, ErrContentIdentityUnavailable) {
				t.Fatal("checkpoint publication replaced an unowned link", err)
			}
			got, err := os.ReadFile(other)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatal("checkpoint write modified an external target", err)
			}
			current, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "symbolic" && current.Mode()&os.ModeSymlink == 0 {
				t.Fatal("checkpoint write silently replaced the symbolic link")
			}
			if kind == "hard" {
				outside, statErr := os.Stat(other)
				if statErr != nil || !os.SameFile(current, outside) {
					t.Fatal("checkpoint write silently replaced the hard link", statErr)
				}
			}
		})
	}
}
