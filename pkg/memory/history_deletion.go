package memory

import (
	"errors"
	"os"
)

// CheckHistoryDeletion fences the launcher's file-based deletion path with the
// same recovery checks as the store. It resolves only this key's pending write.
// The caller must still exclude concurrent writers; this is not a transaction
// across processes.
func CheckHistoryDeletion(dir, key string) error {
	s := &JSONLStore{
		dir: dir, metaCache: make(map[string]SessionMeta), historyPending: make(map[string]bool),
	}
	if _, err := os.Stat(s.historyRecoveryPath(key)); err == nil {
		s.markHistoryPending(key)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	meta, err := s.readMeta(key)
	if err != nil {
		return err
	}
	return s.guardHistoryMutation(key, meta)
}
