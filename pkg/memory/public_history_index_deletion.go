package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Old intents omitted indexes and cannot authorize deleting an unrecorded file.
func (s *JSONLStore) checkPublicIndexDeletion(ctx context.Context, key string, files []historyDeletionFile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		seen[file.Name] = true
	}
	for _, path := range s.publicIndexFiles(key) {
		if seen[filepath.Base(path)] {
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrContentIdentityUnavailable, err)
		}
	}
	return nil
}
