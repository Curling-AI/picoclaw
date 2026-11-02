package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryIndexFreshWriteRetainsCronUntilAllCompanionsAreOld(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-public-index-retention"
	if operationErr := store.SetHistory(
		t.Context(),
		key,
		[]providers.Message{{Role: "assistant", Content: "confirmed"}},
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	age(t, store, key, 48*time.Hour)
	now := time.Now()
	if operationErr := os.Chtimes(store.publicIndexPath(key), now, now); operationErr != nil {
		t.Fatal(operationErr)
	}
	removed, err := store.PruneCronRuns(24*time.Hour, now)
	if err != nil || removed != 0 {
		t.Fatal("retention removed a fresh index write", removed, err)
	}
	age(t, store, key, 48*time.Hour)
	removed, err = store.PruneCronRuns(24*time.Hour, now)
	if err != nil || removed != 1 {
		t.Fatal("retention kept an entirely old cron source", removed, err)
	}
	if _, operationErr := os.Stat(store.publicIndexPath(key)); !errors.Is(operationErr, os.ErrNotExist) {
		t.Fatal("retention stranded an old index", operationErr)
	}
}

func TestPublicHistoryIndexKeepsOlderDeletionIntentsRecoverable(t *testing.T) {
	for _, unexpected := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy absent index", true: "unproven index"}[unexpected], func(t *testing.T) {
			store, key := pauseOwnedDeletion(t)
			data, operationErr0 := os.ReadFile(store.historyRecoveryPath(key))
			if operationErr0 != nil {
				t.Fatal(operationErr0)
			}
			var record historyDeletionRecord
			if operationErr1 := json.Unmarshal(data, &record); operationErr1 != nil {
				t.Fatal(operationErr1)
			}
			indexFiles := map[string]bool{}
			for _, path := range []string{store.publicIndexPath(key), store.publicIndexPreparationPath(key), store.publicIndexBuildingPath(key)} {
				indexFiles[filepath.Base(path)] = true
				if operationErr2 := os.Remove(path); operationErr2 != nil && !errors.Is(operationErr2, os.ErrNotExist) {
					t.Fatal(operationErr2)
				}
			}
			var legacy []historyDeletionFile
			for _, file := range record.Deletion.Files {
				if !indexFiles[file.Name] {
					legacy = append(legacy, file)
				}
			}
			record.Deletion.Files = legacy
			record.Deletion.Fingerprint = historyDeletionFingerprint(record.Deletion)
			data, operationErr0 = json.Marshal(record)
			if operationErr0 != nil {
				t.Fatal(operationErr0)
			}
			if operationErr3 := os.WriteFile(store.historyRecoveryPath(key), data, 0o600); operationErr3 != nil {
				t.Fatal(operationErr3)
			}
			if unexpected {
				if operationErr4 := os.WriteFile(
					store.publicIndexPath(key),
					[]byte("unproven replacement"),
					0o600,
				); operationErr4 != nil {
					t.Fatal(operationErr4)
				}
			}
			operationErr0 = store.recoverHistoryWrite(key)
			if unexpected {
				if !errors.Is(operationErr0, ErrContentIdentityUnavailable) {
					t.Fatal("old intent authorized deletion of an unproven index", operationErr0)
				}
				return
			}
			if operationErr0 != nil {
				t.Fatal("index introduction broke a valid older deletion intent", operationErr0)
			}
			if _, operationErr5 := os.Stat(store.metaPath(key)); !errors.Is(operationErr5, os.ErrNotExist) {
				t.Fatal("older deletion did not complete", operationErr5)
			}
		})
	}
}
