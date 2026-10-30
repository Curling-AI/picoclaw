package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func deletionRetrySource(
	t *testing.T,
	inline, pending bool,
) (*JSONLStore, string, []*contentPreimage, []contentArtifactDescriptor) {
	t.Helper()
	s, key, frame := typedMutationSource(t)
	result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2))
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		// Applied Last intentionally retains exact failed-retirement provenance.
		s.contentStorageFailure = func(_, phase string) error {
			if phase == "remove" {
				return syscall.EIO
			}
			return nil
		}
		if err = s.AcknowledgeHistoryContentMutation(t.Context(), key, typedAppliedAck(result.Transition)); err != nil {
			t.Fatal(err)
		}
		s.contentStorageFailure = nil
	}
	if inline {
		if err = s.PrepareHistoryContentInlineRollback(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	images, err := s.contentImagesForDeletion(t.Context(), key, meta)
	if err != nil {
		t.Fatal(err)
	}
	files, floorImages, err := s.contentStorageForDeletion(t.Context(), key, meta)
	if err != nil {
		t.Fatal(err)
	}
	images = append(images, floorImages...)
	if len(images) == 0 {
		t.Fatal("fixture has no complete rooted private preimage")
	}
	return s, key, images, files
}

func TestContentStorageDeleteRetainsRootsAcrossUnlinkFailureAndRestart(t *testing.T) {
	for _, inline := range []bool{true, false} {
		for _, prune := range []bool{false, true} {
			for _, phase := range []string{"remove", "remove-directory-sync"} {
				for _, nth := range []int{1, 2, 3} {
					if inline && nth > 1 {
						continue
					}
					name := map[bool]string{true: "inline", false: "disk2"}[inline] + map[bool]string{true: "-prune", false: "-owner"}[prune] + "-" + phase + "-" + strconv.Itoa(
						nth,
					)
					t.Run(name, func(t *testing.T) {
						s, key, images, files := deletionRetrySource(t, inline, !prune)
						seen := 0
						s.contentStorageFailure = func(name, operation string) error {
							if operation == phase {
								seen++
								if seen == nth {
									return &os.PathError{Op: operation, Path: name, Err: syscall.EIO}
								}
							}
							return nil
						}
						deleteSource := func(s *JSONLStore) error {
							if prune {
								return s.removeSessionFiles(sanitizeKey(key))
							}
							return s.DeleteSession(t.Context(), key)
						}
						if err := deleteSource(s); !errors.Is(err, syscall.EIO) {
							t.Fatal("unlink/directory failure was hidden", seen, err)
						}
						if _, err := os.Stat(s.metaPath(key)); err != nil {
							t.Fatal("failed deletion discarded its last ownership descriptors", err)
						}
						var err error
						s, err = NewJSONLStore(s.dir)
						if err != nil {
							t.Fatal(err)
						}
						if err = deleteSource(s); err != nil {
							t.Fatal("restart/retry could not complete exact rooted cleanup", err)
						}
						for _, image := range images {
							if _, err = os.Lstat(
								filepath.Join(s.dir, contentPreimageName(key, image.ID)),
							); !errors.Is(
								err,
								os.ErrNotExist,
							) {
								t.Fatal("successful delete stranded owned private content", err)
							}
						}
						for _, file := range files {
							if _, err = os.Lstat(filepath.Join(s.dir, file.name())); !errors.Is(err, os.ErrNotExist) {
								t.Fatal("successful delete stranded owned storage", err)
							}
						}
						for _, path := range []string{s.metaPath(key), s.jsonlPath(key), s.contentDomainPath(key)} {
							if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
								t.Fatal("successful delete left live source roots", err)
							}
						}
					})
				}
			}
		}
	}
}
