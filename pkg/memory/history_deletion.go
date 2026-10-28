package memory

import (
	"context"
	"errors"
	"os"
	"strings"
)

// DeleteSession applies explicit owner intent to this single-writer source.
// Recover the source's own journal before deleting anything; automatic pruning
// and unqualified history replacements retain their stricter retention guards.
func (s *JSONLStore) DeleteSession(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return ErrHistoryOwnershipUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryWrite(key); err != nil {
		return err
	}
	if _, err := s.contentIdentityActiveLocked(ctx, key); err != nil {
		return err
	}
	s.markHistoryPending(key)
	// Keep the established launcher deletion order: remove the listing before
	// the payload, then retire every proof and journal belonging to this source.
	for _, path := range []string{
		s.metaPath(key), s.jsonlPath(key), s.historyRecoveryPath(key), s.historyDecisionPath(key),
		s.historyRecoveryRequiredPath(key), s.contentDomainPath(key), s.historyOrderWitnessPath(key),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	sanitized := sanitizeKey(key)
	s.metaMu.Lock()
	delete(s.metaCache, sanitized)
	delete(s.historyPending, sanitized)
	delete(s.historyMarkers, sanitized)
	delete(s.historyWitnesses, sanitized)
	s.metaMu.Unlock()
	return nil
}

// CheckHistoryDeletion resolves this key's pending write before owner-requested
// deletion. Missing capture proof does not override an explicit deletion intent.
// The caller must still exclude concurrent writers; this is not a transaction
// across processes.
func CheckHistoryDeletion(dir, key string) error {
	s := &JSONLStore{
		dir: dir, metaCache: make(map[string]SessionMeta), historyPending: make(map[string]bool),
	}
	return s.recoverHistoryWrite(key)
}
