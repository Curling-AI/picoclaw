package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func pauseOwnedDeletion(t *testing.T) (*JSONLStore, string) {
	t.Helper()
	s, key, _, _ := deletionRetrySource(t, false, true)
	s.contentStorageFailure = func(_, phase string) error {
		if phase == "remove" {
			return syscall.EIO
		}
		return nil
	}
	if err := s.DeleteSession(t.Context(), key); !errors.Is(err, syscall.EIO) {
		t.Fatal(err)
	}
	s.contentStorageFailure = nil
	return s, key
}

func TestHistoryDeletionIntentDurabilityPrecedesEveryPayloadUnlink(t *testing.T) {
	for _, phase := range []string{"delete-intent-create", "delete-intent-write", "delete-intent-file-sync", "delete-intent-directory-sync"} {
		t.Run(phase, func(t *testing.T) {
			s, key, images, files := deletionRetrySource(t, false, true)
			original := make(map[string][]byte)
			for _, path := range append(s.historyDeletionSourcePaths(key), filepath.Join(s.dir, contentPreimageName(key, images[0].ID))) {
				if data, err := os.ReadFile(path); err == nil {
					original[path] = data
				}
			}
			for _, file := range files {
				path := filepath.Join(s.dir, file.name())
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				original[path] = data
			}
			s.contentStorageFailure = func(_, operation string) error {
				if operation == phase {
					return syscall.EIO
				}
				return nil
			}
			if err := s.DeleteSession(t.Context(), key); !errors.Is(err, syscall.EIO) {
				t.Fatal("intent durability failure hidden", err)
			}
			for path, want := range original {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("unconfirmed intent deleted or changed source", path, err)
				}
			}
			s.contentStorageFailure = nil
			if err := s.DeleteSession(t.Context(), key); err != nil {
				t.Fatal("transient failure was not retryable", err)
			}
			if _, err := os.Stat(s.metaPath(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("retry left source", err)
			}
		})
	}
}

func TestHistoryDeletionIntentPreservesReplacementAndUnexpectedFiles(t *testing.T) {
	for _, change := range []string{"body-same-inode", "body-new-inode-same-bytes", "meta-new-inode", "formerly-absent-marker", "payload-symlink", "payload-hardlink"} {
		t.Run(change, func(t *testing.T) {
			s, key := pauseOwnedDeletion(t)
			journal, err := os.ReadFile(s.historyRecoveryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			var record historyDeletionRecord
			if err = json.Unmarshal(journal, &record); err != nil {
				t.Fatal(err)
			}
			if len(journal) > maxHistoryDeletionJournal || bytes.Contains(journal, []byte(`"summary"`)) ||
				bytes.Contains(journal, []byte(`"content"`)) {
				t.Fatal("cleanup journal copied message/summary payload")
			}
			path := s.jsonlPath(key)
			switch change {
			case "body-same-inode":
				f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = f.WriteString("new source bytes\n")
				err = errors.Join(err, f.Close())
			case "body-new-inode-same-bytes", "meta-new-inode":
				if change == "meta-new-inode" {
					path = s.metaPath(key)
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				replacement := path + ".new-source"
				if err = os.WriteFile(replacement, data, 0o600); err == nil {
					err = os.Rename(replacement, path)
				}
			case "formerly-absent-marker":
				path = s.historyRecoveryRequiredPath(key)
				err = os.WriteFile(path, []byte("new proof-loss evidence\n"), 0o600)
			case "payload-symlink", "payload-hardlink":
				for _, file := range record.Deletion.Files {
					if !file.Absent && (file.Image != nil || file.Artifact != nil) {
						path = filepath.Join(s.dir, file.Name)
						break
					}
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				other := filepath.Join(t.TempDir(), "unowned-payload")
				if err = os.WriteFile(other, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(path); err == nil {
					if change == "payload-symlink" {
						err = os.Symlink(other, path)
					} else {
						err = os.Link(other, path)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := make(map[string][]byte)
			for _, entry := range record.Deletion.Files {
				if data, readErr := os.ReadFile(filepath.Join(s.dir, entry.Name)); readErr == nil {
					before[entry.Name] = data
				}
			}
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = reopened.DeleteSession(
				t.Context(),
				key,
			); !errors.Is(err, ErrContentIdentityUnavailable) &&
				err == nil {
				t.Fatal("stale intent accepted changed source", err)
			}
			for name, want := range before {
				got, readErr := os.ReadFile(filepath.Join(s.dir, name))
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatal("stale intent removed a replacement or another remaining file", name, readErr)
				}
			}
			if _, err = os.Lstat(path); err != nil {
				t.Fatal("replacement/evidence removed", err)
			}
		})
	}
}

func TestHistoryDeletionIntentRejectsOpaqueForeignOrIncompleteAuthorization(t *testing.T) {
	for _, change := range []string{"version", "key", "foreign-name", "missing-descriptor", "fingerprint", "unknown-field"} {
		t.Run(change, func(t *testing.T) {
			s, key := pauseOwnedDeletion(t)
			path := s.historyRecoveryPath(key)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var record historyDeletionRecord
			if err = json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "version":
				record.Deletion.Version++
			case "key":
				record.Deletion.Key = "sk_v1_unowned"
			case "foreign-name":
				record.Deletion.Files[0].Name = "foreign-owned-file"
			case "missing-descriptor":
				record.Deletion.Files = record.Deletion.Files[1:]
			case "fingerprint":
				record.Deletion.Fingerprint = contentDigest([]byte("other"))
			}
			if change != "fingerprint" {
				record.Deletion.Fingerprint = historyDeletionFingerprint(record.Deletion)
			}
			data, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if change == "unknown-field" {
				data = append([]byte(`{"unrecognized":true,`), data[1:]...)
			}
			if err = os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.DeleteSession(t.Context(), key); err == nil {
				t.Fatal("invalid authorization was accepted")
			}
			got, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(body, got) {
				t.Fatal("invalid authorization partially deleted source", err)
			}
		})
	}
}

func TestHistoryDeletionIntentMetadataAndFinalJournalFailuresRemainRetryable(t *testing.T) {
	for _, phase := range []string{"delete-source", "delete-source-directory-sync", "delete-intent-remove", "delete-intent-remove-directory-sync"} {
		t.Run(phase, func(t *testing.T) {
			s, key, images, files := deletionRetrySource(t, false, true)
			s.contentStorageFailure = func(_, operation string) error {
				if operation == phase {
					return syscall.EIO
				}
				return nil
			}
			if err := s.DeleteSession(t.Context(), key); !errors.Is(err, syscall.EIO) {
				t.Fatal("cleanup failure hidden", err)
			}
			s.contentStorageFailure = nil
			reopened, err := NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = reopened.DeleteSession(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			paths := append(reopened.historyDeletionSourcePaths(key), reopened.historyRecoveryPath(key))
			for _, image := range images {
				paths = append(paths, filepath.Join(s.dir, contentPreimageName(key, image.ID)))
			}
			for _, file := range files {
				paths = append(paths, filepath.Join(s.dir, file.name()))
			}
			for _, path := range paths {
				if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("successful retry stranded private source", path, err)
				}
			}
		})
	}
}

func TestHistoryDeletionIntentWarmRetentionRetryIgnoresOnlyItsOwnFreshJournal(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-cleanup-retry"
	for _, role := range []string{"user", "assistant", "user", "assistant"} {
		if err = s.AddFullMessage(
			t.Context(),
			key,
			providers.Message{Role: role, Content: "private same content"},
		); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, s, key)
	result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2))
	if err != nil {
		t.Fatal(err)
	}
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
	age(t, s, key, 48*time.Hour)
	seen := 0
	s.contentStorageFailure = func(_, phase string) error {
		if phase == "remove-directory-sync" {
			seen++
			if seen == 1 {
				return syscall.EIO
			}
		}
		return nil
	}
	if n, pruneErr := s.PruneCronRuns(24*time.Hour, time.Now()); pruneErr != nil || n != 0 {
		t.Fatal("failed prune claimed completion", n, pruneErr)
	}
	s.contentStorageFailure = nil
	if n, pruneErr := s.PruneCronRuns(24*time.Hour, time.Now()); pruneErr != nil || n != 1 {
		t.Fatal("own fresh cleanup journal blocked authorized retention retry", n, pruneErr)
	}
	if _, err = os.Stat(s.metaPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retention retry left source", err)
	}
}
