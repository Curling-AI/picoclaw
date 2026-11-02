package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicHistoryIndexKeepsOlderDeletionIntentsRecoverable(t *testing.T) {
	for _, unexpected := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy absent index", true: "unproven index"}[unexpected], func(t *testing.T) {
			store, key := pauseOwnedDeletion(t)
			data, err := os.ReadFile(store.historyRecoveryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			var record historyDeletionRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			indexFiles := map[string]bool{}
			for _, path := range []string{store.publicIndexPath(key), store.publicIndexPreparationPath(key), store.publicIndexBuildingPath(key)} {
				indexFiles[filepath.Base(path)] = true
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
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
			data, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.historyRecoveryPath(key), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if unexpected {
				if err := os.WriteFile(store.publicIndexPath(key), []byte("unproven replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = store.recoverHistoryWrite(key)
			if unexpected {
				if !errors.Is(err, ErrContentIdentityUnavailable) {
					t.Fatal("old intent authorized deletion of an unproven index", err)
				}
				return
			}
			if err != nil {
				t.Fatal("index introduction broke a valid older deletion intent", err)
			}
			if _, err := os.Stat(store.metaPath(key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("older deletion did not complete", err)
			}
		})
	}
}
