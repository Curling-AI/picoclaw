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
