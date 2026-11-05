package memory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestHistoryDeletionEmptyWarmDiscoveryAfterMetadataRemoval(t *testing.T) {
	for _, fault := range []string{"none", "after-meta-unlink", "before-journal-unlink"} {
		t.Run(fault, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_audit_empty_cleanup"
			for _, role := range []string{"user", "assistant", "user", "assistant"} {
				if err = s.AddMessage(t.Context(), key, role, "same"); err != nil {
					t.Fatal(err)
				}
			}
			frame := initializedContent(t, s, key)
			result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 0))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.AcknowledgeHistoryContentMutation(
				t.Context(),
				key,
				typedAppliedAck(result.Transition),
			); err != nil {
				t.Fatal(err)
			}
			age(t, s, key, 48*time.Hour)
			hooks := 0
			hook := func(SessionMeta) error { hooks++; return nil }
			if fault != "none" {
				s.contentStorageFailure = func(name, phase string) error {
					if fault == "after-meta-unlink" && name == filepath.Base(s.metaPath(key)) &&
						phase == "delete-source-directory-sync" ||
						fault == "before-journal-unlink" && phase == "delete-intent-remove" {
						return syscall.EIO
					}
					return nil
				}
			}
			removed, err := s.PruneEmptySessions(24*time.Hour, time.Now(), hook)
			if fault == "none" {
				if err != nil || !slices.Contains(removed, key) {
					t.Fatal("control did not perform eligible cleanup", removed, err)
				}
			} else {
				if !errors.Is(err, syscall.EIO) || len(removed) != 0 {
					t.Fatal("fault did not interrupt authorized cleanup", removed, err)
				}
				if _, err = os.Lstat(s.metaPath(key)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("fault did not pass metadata unlink", err)
				}
				if _, err = os.Lstat(s.historyRecoveryPath(key)); err != nil {
					t.Fatal("interrupted cleanup lost authorization", err)
				}
				s.contentStorageFailure = nil
				removed, err = s.PruneEmptySessions(24*time.Hour, time.Now(), hook)
				if err != nil || !slices.Contains(removed, key) {
					t.Errorf(
						"warm sweep lost authorized cleanup after metadata unlink: removed=%v err=%v",
						removed,
						err,
					)
				}
			}
			if hooks != 1 {
				t.Errorf("authorized cleanup reran hook: calls=%d", hooks)
			}
			for _, path := range append(s.historyDeletionSourcePaths(key), s.historyRecoveryPath(key)) {
				if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("transient retry left authorized cleanup file %s: %v", filepath.Base(path), err)
				}
			}
		})
	}
}

func TestHistoryDeletionCronWarmDiscoveryAfterMetadataRemoval(t *testing.T) {
	for _, fault := range []string{"none", "after-meta-unlink", "before-journal-unlink"} {
		t.Run(fault, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "agent:cron-audit-cleanup-retry"
			for _, role := range []string{"user", "assistant", "user", "assistant"} {
				if err = s.AddMessage(t.Context(), key, role, "same"); err != nil {
					t.Fatal(err)
				}
			}
			frame := initializedContent(t, s, key)
			result, err := s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 2))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.AcknowledgeHistoryContentMutation(
				t.Context(),
				key,
				typedAppliedAck(result.Transition),
			); err != nil {
				t.Fatal(err)
			}
			age(t, s, key, 48*time.Hour)
			faults := 0
			if fault != "none" {
				s.contentStorageFailure = func(name, phase string) error {
					if fault == "after-meta-unlink" && name == filepath.Base(s.metaPath(key)) &&
						phase == "delete-source-directory-sync" ||
						fault == "before-journal-unlink" && phase == "delete-intent-remove" {
						faults++
						return syscall.EIO
					}
					return nil
				}
			}
			removed, err := s.PruneCronRuns(24*time.Hour, time.Now())
			if fault == "none" {
				if err != nil || removed != 1 {
					t.Fatal("control did not prune eligible run", removed, err)
				}
			} else {
				if err != nil || removed != 0 || faults == 0 {
					t.Fatal("fault did not interrupt authorized prune", removed, err, faults)
				}
				if _, err = os.Lstat(s.metaPath(key)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("fault did not pass metadata unlink", err)
				}
				if _, err = os.Lstat(s.historyRecoveryPath(key)); err != nil {
					t.Fatal("interrupted prune lost authorization", err)
				}
				s.contentStorageFailure = nil
				removed, err = s.PruneCronRuns(24*time.Hour, time.Now())
				if err != nil || removed != 1 {
					t.Errorf(
						"warm retention ignored its already-authorized fresh journal after metadata unlink: removed=%d err=%v",
						removed,
						err,
					)
				}
			}
			for _, path := range append(s.historyDeletionSourcePaths(key), s.historyRecoveryPath(key)) {
				if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("transient retention retry left authorized cleanup file %s: %v", filepath.Base(path), err)
				}
			}
		})
	}
}
